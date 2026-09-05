import { Events, Updater } from "@wailsio/runtime";
import { Tester } from "../bindings/github.com/sombochea/cubisoft-tester";
import type {
    BackupResult, Config, DiagnoseResult, Hop, LatencyResult, Phase, Profile,
    Progress, RestoreResult, Series, ServerInfo, SpeedResult, Step, TableInfo,
    Toolbox, TraceResult,
} from "../bindings/github.com/sombochea/cubisoft-tester";

type Tab = "diagnose" | "latency" | "trace" | "speed" | "backup";

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;
const val = (id: string) => $<HTMLInputElement>(id).value.trim();
const num = (id: string) => Number($<HTMLInputElement>(id).value) || 0;
const checked = (id: string) => $<HTMLInputElement>(id).checked;

// Everything the server sends back — versions, errors, grants, hostnames — is
// untrusted text going into innerHTML, so it all goes through here first.
const escapeMap: Record<string, string> = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };
const esc = (s: unknown) => String(s ?? "").replace(/[&<>"']/g, (c) => escapeMap[c]);

const ms = (n: number) => `${n.toFixed(n < 10 ? 2 : 1)} ms`;
// The bridge prefixes every Go error with "RuntimeError:", which means nothing
// to the person reading it.
const errText = (e: unknown) => String(e).replace(/^\w*Error:\s*/, "");
// Go marshals a nil slice as null, and the generated bindings type it that way.
const list = <T,>(v: T[] | null | undefined): T[] => v ?? [];
const int = (n: number) => Math.round(n).toLocaleString();

let currentTab: Tab = "diagnose";
let running = false;
let lastResult: unknown = null;

/* ---------- connection form ---------- */

const STORE_KEY = "mysqltester.connection";

function config(): Config {
    return {
        host: val("host"),
        port: num("port") || 3306,
        user: val("user"),
        password: $<HTMLInputElement>("password").value,
        database: val("database"),
        tls: $<HTMLSelectElement>("tls").value,
        connectTimeoutSec: num("timeout") || 10,
    };
}

// The password is deliberately left out: localStorage is a plaintext file on disk.
function saveForm() {
    const { host, port, user, database, tls, connectTimeoutSec } = config();
    localStorage.setItem(STORE_KEY, JSON.stringify({ host, port, user, database, tls, connectTimeoutSec }));
}

function loadForm() {
    try {
        const saved = JSON.parse(localStorage.getItem(STORE_KEY) || "{}");
        if (saved.host) $<HTMLInputElement>("host").value = saved.host;
        if (saved.port) $<HTMLInputElement>("port").value = String(saved.port);
        if (saved.user) $<HTMLInputElement>("user").value = saved.user;
        if (saved.database) $<HTMLInputElement>("database").value = saved.database;
        if (saved.tls) $<HTMLSelectElement>("tls").value = saved.tls;
        if (saved.connectTimeoutSec) $<HTMLInputElement>("timeout").value = String(saved.connectTimeoutSec);
    } catch {
        // A corrupt entry is not worth failing startup over.
    }
}

function setStatus(text: string, state: "idle" | "ok" | "bad" | "busy") {
    const el = $("status");
    el.textContent = text;
    el.dataset.state = state;
}

/* ---------- tabs ---------- */

$("tabs").addEventListener("click", (e) => {
    const tab = (e.target as HTMLElement).closest<HTMLElement>(".tab");
    if (!tab) return;
    currentTab = tab.dataset.tab as Tab;
    document.querySelectorAll(".tab").forEach((t) => t.classList.toggle("is-active", t === tab));
    document.querySelectorAll<HTMLElement>(".panel").forEach((p) => p.classList.toggle("is-active", p.dataset.panel === currentTab));
});

/* ---------- running ---------- */

function setRunning(on: boolean) {
    running = on;
    $<HTMLButtonElement>("run").disabled = on;
    $<HTMLButtonElement>("cancel").hidden = !on;
    $("progress").hidden = !on;
    if (!on) setProgress("", 0);
}

// A negative pct means "no total to measure against" — streaming a dump, say —
// so the bar is left alone and only the label moves.
function setProgress(text: string, pct: number) {
    if (pct >= 0) $("progress-fill").style.width = `${Math.min(100, pct)}%`;
    $("progress-text").textContent = text;
}

Events.On("progress", (e: { data: Progress }) => {
    const p = e.data;
    if (running) setProgress(`${p.kind}: ${p.message}`, p.pct);
});

$("run").addEventListener("click", () => void run());
$("cancel").addEventListener("click", () => void Tester.Cancel());
$("copy").addEventListener("click", () => {
    if (lastResult === null) return;
    void navigator.clipboard.writeText(JSON.stringify(lastResult, null, 2));
    const btn = $("copy");
    btn.textContent = "Copied";
    setTimeout(() => (btn.textContent = "Copy report"), 1200);
});

$("load-dbs").addEventListener("click", async () => {
    setStatus("Listing databases…", "busy");
    try {
        const dbs = await Tester.ListDatabases(config());
        $("db-list").innerHTML = list(dbs).map((d) => `<option value="${esc(d)}"></option>`).join("");
        setStatus(`${list(dbs).length} database(s) available. Click the Database field to pick one.`, "ok");
    } catch (err) {
        setStatus(errText(err), "bad");
    }
});

async function run() {
    if (running) return;
    if (!val("host")) {
        setStatus("Enter a host first.", "bad");
        return;
    }
    saveForm();
    setRunning(true);
    setStatus("Running…", "busy");
    const out = $(`out-${currentTab}`);
    try {
        switch (currentTab) {
            case "diagnose": {
                const r = await Tester.Diagnose(config());
                lastResult = r;
                out.innerHTML = renderDiagnose(r);
                setStatus(r.ok ? `Connected — ${r.server.flavor} ${r.server.version}` : failedStep(r), r.ok ? "ok" : "bad");
                break;
            }
            case "latency": {
                const r = await Tester.Latency({
                    config: config(), count: num("lat-count"), intervalMs: num("lat-interval"), coldConnects: num("lat-cold"),
                });
                lastResult = r;
                out.innerHTML = renderLatency(r);
                setStatus(r.verdict, "ok");
                break;
            }
            case "trace": {
                const r = await Tester.Trace({
                    config: config(), maxHops: num("tr-hops"), probes: num("tr-probes"),
                    timeoutMs: num("tr-timeout"), resolveNames: checked("tr-names"),
                });
                lastResult = r;
                out.innerHTML = renderTrace(r);
                setStatus(r.portOpen ? `MySQL port reachable in ${ms(r.portMs)}` : `Port unreachable: ${r.portError}`, r.portOpen ? "ok" : "bad");
                break;
            }
            case "backup": {
                out.innerHTML = renderTables(await Tester.ListTables(config()));
                setStatus("Listed tables.", "ok");
                break;
            }
            case "speed": {
                const r = await Tester.SpeedTest({
                    config: config(), rows: num("sp-rows"), payloadBytes: num("sp-payload"),
                    batchSize: num("sp-batch"), transactions: num("sp-tx"),
                    concurrency: num("sp-conc"), keepTable: checked("sp-keep"),
                });
                lastResult = r;
                out.innerHTML = renderSpeed(r);
                setStatus(r.ok ? r.summary : "Speed test did not complete.", r.ok ? "ok" : "bad");
                break;
            }
        }
    } catch (err) {
        out.innerHTML = `<div class="verdict bad">${esc(errText(err))}</div>`;
        setStatus(errText(err), "bad");
    } finally {
        setRunning(false);
    }
}

function failedStep(r: DiagnoseResult): string {
    const bad = list(r.steps).find((s) => !s.ok);
    return bad ? `Failed at: ${bad.name}` : "Failed.";
}

/* ---------- shared rendering ---------- */

function card(title: string, body: string, raw = false) {
    return `<section class="card"><h2>${esc(title)}</h2>${raw ? body : `<div class="card-body">${body}</div>`}</section>`;
}

function metrics(items: [string, string][]) {
    return `<div class="metrics">${items.map(([label, v]) => `<div class="metric"><b>${esc(v)}</b><span>${esc(label)}</span></div>`).join("")}</div>`;
}

function noteList(title: string, notes: string[]) {
    if (!notes.length) return "";
    return card(title, `<ul class="notes">${notes.map((n) => `<li>${esc(n)}</li>`).join("")}</ul>`);
}

// spark draws one bar per probe in arrival order. A full-height red bar is a
// probe that never came back, so loss and latency read at a glance.
function spark(samples: number[]) {
    if (!samples.length) return "";
    const ok = samples.filter((v) => v >= 0);
    const max = ok.length ? Math.max(...ok) : 1;
    const h = 50, w = 4, gap = 1;
    const bars = samples.map((v, i) => {
        const height = v >= 0 ? Math.max(1, (v / max) * h) : h;
        return `<rect class="${v < 0 ? "lost" : ""}" x="${i * (w + gap)}" y="${h - height}" width="${w}" height="${height}"/>`;
    }).join("");
    return `<svg class="spark" viewBox="0 0 ${samples.length * (w + gap)} ${h}" preserveAspectRatio="none">${bars}</svg>`;
}

/* ---------- diagnose ---------- */

function renderDiagnose(r: DiagnoseResult) {
    const verdict = r.ok
        ? `<div class="verdict ok">Connection healthy — ${esc(r.server.flavor)} ${esc(r.server.version)} at ${esc(r.server.resolvedAddr)}, ${ms(r.totalMs)} total.</div>`
        : `<div class="verdict bad">${esc(failedStep(r))}</div>`;

    const steps = card("Connection path", list(r.steps).map(stepRow).join(""), true);
    const server = r.server.version ? card("Server", serverInfo(r.server)) : "";
    const grants = list(r.server.grants).length
        ? card("Privileges", `<div class="mono dim">${list(r.server.grants).map((g) => esc(g)).join("<br/>")}</div>`)
        : "";

    return verdict + steps + server + noteList("Warnings", list(r.warnings)) + grants;
}

function stepRow(s: Step) {
    const state = s.skipped ? "skipped" : s.ok ? "ok" : "bad";
    const mark = s.skipped ? "•" : s.ok ? "●" : "✕";
    return `<div class="step ${state}">
        <span class="mark">${mark}</span>
        <span class="name">${esc(s.name)}</span>
        <span class="ms">${s.durationMs > 0 ? ms(s.durationMs) : ""}</span>
        ${s.detail ? `<span class="detail">${esc(s.detail)}</span>` : ""}
        ${s.error ? `<span class="err">${esc(s.error)}</span>` : ""}
        ${s.hint ? `<span class="hint">${esc(s.hint)}</span>` : ""}
    </div>`;
}

function serverInfo(si: ServerInfo) {
    const rows: [string, string][] = [
        ["Version", `${si.flavor} ${si.version}`],
        ["Build", si.versionComment],
        ["Address", si.resolvedAddr],
        ["Encryption", si.tlsCipher || "none (plaintext)"],
        ["Connection ID", String(si.connectionId)],
        ["Current user", si.currentUser],
        ["Character set", `${si.characterSet} / ${si.collation}`],
        ["Time zone", si.timeZone],
        ["Clock skew", `${si.clockSkewMs.toFixed(0)} ms vs this machine`],
        ["Connections", `${int(si.threadsConnected)} of ${int(si.maxConnections)}`],
        ["max_allowed_packet", `${(si.maxAllowedPacket / 1048576).toFixed(1)} MiB`],
        ["wait_timeout", `${int(si.waitTimeoutSec)} s`],
        ["Uptime", uptime(si.uptimeSec)],
        ["Read only", si.readOnly ? "yes" : "no"],
        ["sql_mode", si.sqlMode || "(empty)"],
    ];
    return `<dl class="kv">${rows.map(([k, v]) => `<div><dt>${esc(k)}</dt><dd>${esc(v)}</dd></div>`).join("")}</dl>`;
}

function uptime(sec: number) {
    const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
    return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
}

/* ---------- latency ---------- */

function renderLatency(r: LatencyResult) {
    const verdict = `<div class="verdict ${list(r.warnings).length ? "" : "ok"}">${esc(r.verdict)}</div>`;
    return verdict + list(r.series).map(seriesCard).join("") + noteList("Warnings", list(r.warnings));
}

function seriesCard(s: Series) {
    if (s.error && s.sent === 0) {
        return card(s.name, `<div class="err mono">${esc(s.error)}</div>`);
    }
    const body = metrics([
        ["min", ms(s.stats.min)],
        ["median", ms(s.stats.p50)],
        ["mean", ms(s.stats.avg)],
        ["p95", ms(s.stats.p95)],
        ["p99", ms(s.stats.p99)],
        ["max", ms(s.stats.max)],
        ["jitter", ms(s.stats.jitter)],
        ["loss", `${s.lost}/${s.sent}`],
    ]) + spark(list(s.samples)) + (s.error ? `<div class="err mono">${esc(s.error)}</div>` : "");
    return card(s.name, body);
}

/* ---------- trace ---------- */

function renderTrace(r: TraceResult) {
    const verdict = r.portOpen
        ? `<div class="verdict ok">TCP connect to ${esc(r.targetIp)}:${esc(String(config().port))} succeeded in ${ms(r.portMs)}.</div>`
        : `<div class="verdict bad">Cannot open the MySQL port on ${esc(r.targetIp)}: ${esc(r.portError)}</div>`;

    const summary = metrics([
        ["target", r.targetIp],
        ["hops", r.reached ? String(r.totalHops) : `${r.totalHops}+`],
        ["mode", r.mode],
        ["biggest jump", r.longestHop ? `hop ${r.longestHop}` : "—"],
    ]);

    const rows = list(r.hops).map(hopRow).join("");
    const table = card("Path", `<div class="scroll-x"><table>
        <thead><tr><th class="num">#</th><th>Address</th><th>Hostname</th><th class="num">probes</th><th class="num">avg</th></tr></thead>
        <tbody>${rows}</tbody></table></div>`, true);

    const note = r.note ? `<div class="verdict">${esc(r.note)}</div>` : "";
    return verdict + summary + table + note;
}

function hopRow(h: Hop) {
    const probes = list(h.rtts).map((v) => (v < 0 ? "*" : v.toFixed(1))).join("  ");
    const silent = !h.addr;
    return `<tr class="${silent ? "bad" : ""}">
        <td class="num">${h.ttl}</td>
        <td class="mono">${esc(h.addr || "* * *")}</td>
        <td class="wrap dim">${esc(h.host || (silent ? "no reply" : ""))}${h.note ? ` ${esc(h.note)}` : ""}${h.final ? " <b>(destination)</b>" : ""}</td>
        <td class="num dim">${esc(probes)}</td>
        <td class="num">${h.avgMs > 0 ? ms(h.avgMs) : "—"}</td>
    </tr>`;
}

/* ---------- speed test ---------- */

function renderSpeed(r: SpeedResult) {
    const phases = list(r.phases);
    const failed = phases.find((p) => !p.ok);
    const verdict = r.ok
        ? `<div class="verdict ok">${esc(r.summary)}</div>`
        : `<div class="verdict bad">Stopped at “${esc(failed?.name ?? "unknown phase")}”: ${esc(failed?.error ?? "")}${failed?.hint ? ` — ${esc(failed.hint)}` : ""}</div>`;

    const insert = phases.find((p) => p.name.startsWith("Insert"));
    const scan = phases.find((p) => p.name === "Select: full table scan");
    const tx = phases.find((p) => p.name.startsWith("Transactions: ") && p.opsPerSec > 0);
    const tiles = metrics([
        ["write", insert ? `${insert.miBPerSec.toFixed(2)} MiB/s` : "—"],
        ["read", scan ? `${scan.miBPerSec.toFixed(2)} MiB/s` : "—"],
        ["rows written/s", insert ? int(insert.rowsPerSec) : "—"],
        ["rows read/s", scan ? int(scan.rowsPerSec) : "—"],
        ["commits/s", tx ? tx.opsPerSec.toFixed(1) : "—"],
        ["elapsed", ms(r.totalMs)],
    ]);

    const table = card(`Phases — table ${r.table}`, `<div class="scroll-x"><table>
        <thead><tr><th></th><th>Phase</th><th class="num">time</th><th class="num">rows</th><th class="num">rows/s</th><th class="num">MiB/s</th><th>notes</th></tr></thead>
        <tbody>${phases.map(phaseRow).join("")}</tbody></table></div>`, true);

    return verdict + tiles + table + noteList("Warnings", list(r.warnings));
}

function phaseRow(p: Phase) {
    const note = p.error ? `${p.error}${p.hint ? ` — ${p.hint}` : ""}` : p.detail;
    return `<tr class="${p.ok ? "" : "bad"}">
        <td>${p.ok ? "●" : "✕"}</td>
        <td class="wrap">${esc(p.name)}</td>
        <td class="num">${ms(p.durationMs)}</td>
        <td class="num">${p.rows ? int(p.rows) : "—"}</td>
        <td class="num">${p.rowsPerSec ? int(p.rowsPerSec) : "—"}</td>
        <td class="num">${p.miBPerSec ? p.miBPerSec.toFixed(2) : "—"}</td>
        <td class="wrap dim">${esc(note)}</td>
    </tr>`;
}

/* ---------- small modal ---------- */

const askDialog = $<HTMLDialogElement>("ask");

type AskField = { label: string; type?: string; value?: string };
type AskCheck = { label: string; checked?: boolean };
type AskResult = { value: string; checked: boolean };

// Native <dialog>, so there is no dependency and no focus trap to maintain.
// window.prompt is unavailable in the webview, which is why this exists.
function ask(title: string, body: string, field?: AskField, check?: AskCheck): Promise<AskResult | null> {
    $("ask-title").textContent = title;
    $("ask-body").textContent = body;

    const input = $<HTMLInputElement>("ask-input");
    $("ask-field").hidden = !field;
    if (field) {
        $("ask-label").textContent = field.label;
        input.type = field.type ?? "text";
        input.value = field.value ?? "";
    }

    const box = $<HTMLInputElement>("ask-checkbox");
    $("ask-check").hidden = !check;
    if (check) {
        $("ask-check-label").textContent = check.label;
        box.checked = check.checked ?? false;
    }

    return new Promise((resolve) => {
        askDialog.addEventListener("close", () => {
            resolve(askDialog.returnValue === "ok" ? { value: input.value, checked: box.checked } : null);
        }, { once: true });
        askDialog.showModal();
        if (field) input.select();
    });
}

/* ---------- profiles ---------- */

const profileList = $<HTMLSelectElement>("profile-list");
let profiles: Profile[] = [];
let profilesEncrypted = false;
let profilesLocked = false;
let selectedProfile = "";

function applyProfile(p: Profile) {
    const c = p.config;
    $<HTMLInputElement>("host").value = c.host;
    $<HTMLInputElement>("port").value = String(c.port || 3306);
    $<HTMLInputElement>("user").value = c.user;
    $<HTMLInputElement>("database").value = c.database;
    $<HTMLSelectElement>("tls").value = c.tls || "false";
    $<HTMLInputElement>("timeout").value = String(c.connectTimeoutSec || 10);
    $<HTMLInputElement>("password").value = c.password;
    saveForm();
}

function option(value: string, label: string) {
    return `<option value="${esc(value)}">${esc(label)}</option>`;
}

// Action entries live in the same picker as the profiles, so the sidebar needs
// no buttons at all. They are marked with an attribute rather than a magic
// value: a profile name can never forge one, and the HTML parser cannot mangle
// it the way it rewrites control characters inside attribute values.
function action(name: string, label: string) {
    return `<option value="" data-action="${name}">${esc(label)}</option>`;
}

async function refreshProfiles(selected = "") {
    const info = await Tester.ProfilesInfo();
    profilesEncrypted = info.encrypted;
    profilesLocked = info.encrypted && !info.unlocked;
    profileList.title = info.path;

    if (profilesLocked) {
        profiles = [];
        selectedProfile = "";
        profileList.innerHTML = option("", "Profiles locked") + action("unlock", "Unlock…");
        profileList.value = "";
        return;
    }

    profiles = list(await Tester.ListProfiles());
    selectedProfile = profiles.some((p) => p.name === selected) ? selected : "";

    let html = option("", profiles.length ? "No profile" : "No saved profiles");
    if (profiles.length) {
        html += `<optgroup label="Profiles">${profiles.map((p) => option(p.name, p.name)).join("")}</optgroup>`;
    }
    html += `<optgroup label="Manage">` + action("save", "Save current connection…");
    if (selectedProfile) html += action("delete", `Delete “${selectedProfile}”…`);
    if (profilesEncrypted) html += action("key", "Change secret key…");
    html += `</optgroup>`;

    profileList.innerHTML = html;
    profileList.value = selectedProfile;
}

profileList.addEventListener("change", () => {
    const chosen = profileList.selectedOptions[0]?.dataset.action;
    if (!chosen) {
        selectedProfile = profileList.value;
        const p = profiles.find((x) => x.name === selectedProfile);
        if (p) applyProfile(p);
        void refreshProfiles(selectedProfile); // the Delete entry follows the selection
        return;
    }
    profileList.value = selectedProfile; // running an action is not a selection
    switch (chosen) {
        case "save": void saveProfileAction(); break;
        case "delete": void deleteProfileAction(); break;
        case "key": void changeKeyAction(); break;
        case "unlock": void unlockAction(); break;
    }
});

async function saveProfileAction() {
    const r = await ask("Save profile", "Stored on this machine only.",
        { label: "Name", value: selectedProfile || val("host") },
        { label: "Save the password too", checked: profilesEncrypted });
    if (!r || !r.value.trim()) return;

    try {
        // A password can only be kept in an encrypted file, so ask for the key
        // now rather than letting the save fail.
        if (r.checked && !profilesEncrypted) {
            const key = await ask("Set a secret key",
                "Passwords are only saved into an encrypted profile file. Choose a key — it cannot be recovered.",
                { label: "Secret key", type: "password" });
            if (!key || !key.value) return;
            await Tester.SetProfilesSecret(key.value);
            profilesEncrypted = true;
        }
        await Tester.SaveProfile({ name: r.value.trim(), config: config(), savePassword: r.checked });
        await refreshProfiles(r.value.trim());
        setStatus(`Saved profile “${r.value.trim()}”.`, "ok");
    } catch (err) {
        setStatus(errText(err), "bad");
    }
}

async function deleteProfileAction() {
    if (!selectedProfile) return;
    const name = selectedProfile;
    if (!(await ask("Delete profile", `Delete “${name}”?`))) return;
    try {
        await Tester.DeleteProfile(name);
        await refreshProfiles();
        setStatus(`Deleted profile “${name}”.`, "ok");
    } catch (err) {
        setStatus(errText(err), "bad");
    }
}

async function changeKeyAction() {
    const r = await ask("Secret key",
        "Enter a new key, or leave it blank to remove encryption — which also clears every stored password.",
        { label: "Secret key", type: "password" });
    if (!r) return;
    try {
        await Tester.SetProfilesSecret(r.value);
        await refreshProfiles(selectedProfile);
        setStatus(r.value ? "Secret key changed." : "Encryption removed; stored passwords cleared.", "ok");
    } catch (err) {
        setStatus(errText(err), "bad");
    }
}

async function unlockAction() {
    const r = await ask("Unlock profiles", "The profile file on this machine is encrypted.",
        { label: "Secret key", type: "password" });
    if (!r) return;
    try {
        await Tester.UnlockProfiles(r.value);
        await refreshProfiles();
        setStatus("Profiles unlocked for this session.", "ok");
    } catch (err) {
        setStatus(errText(err), "bad");
    }
}

async function exportProfilesAction() {
    const r = await ask("Export profiles",
        "Give the export a key to encrypt it and carry the saved passwords with it. Leave it blank for plain JSON, which is exported without passwords.",
        { label: "Key for this export", type: "password" });
    if (!r) return;
    try {
        const path = await Tester.ExportProfiles(r.value);
        setStatus(path ? `Exported to ${path}` : "Export cancelled.", path ? "ok" : "idle");
    } catch (err) {
        setStatus(errText(err), "bad");
    }
}

// importFrom drives both the File menu (which picks a file first) and a dropped
// file (path already known). The key is asked for only when the file has one.
async function importFrom(path: string) {
    try {
        if (!path) {
            path = await Tester.PickProfileFile();
            if (!path) return;
        }
        let secret = "";
        if (await Tester.ImportEncrypted(path)) {
            const r = await ask("Import profiles", "This file is encrypted. Enter the key it was exported with.",
                { label: "Secret key", type: "password" });
            if (!r) return;
            secret = r.value;
        }
        const names = list(await Tester.ImportProfiles(path, secret, false));
        // Land on what was just imported rather than making the user hunt for it.
        await refreshProfiles(names[0] ?? selectedProfile);
        const landed = profiles.find((p) => p.name === names[0]);
        if (landed) applyProfile(landed);
        setStatus(names.length ? `Imported ${names.length} profile(s).` : "Nothing to import.", "ok");
    } catch (err) {
        setStatus(errText(err), "bad");
    }
}

Events.On("files-dropped", (e: { data: string }) => void importFrom(e.data));

// The File menu emits actions rather than acting itself, so a menu item and the
// picker run the same handler, prompts and all.
Events.On("menu", (e: { data: string }) => {
    if (e.data === "profiles.export") void exportProfilesAction();
    if (e.data === "profiles.import") void importFrom("");
    if (e.data === "app.update") void checkUpdates();
});

/* ---------- backup ---------- */

function backupOptions() {
    return {
        config: config(),
        tables: val("bk-tables").split(/[\s,]+/).filter(Boolean),
        schemaOnly: checked("bk-schema"),
        dataOnly: checked("bk-data"),
        addDropTable: checked("bk-drop"),
        routines: checked("bk-routines"),
        triggers: checked("bk-triggers"),
        events: checked("bk-events"),
        singleTransaction: checked("bk-tx"),
        compress: checked("bk-gzip"),
        path: "", // blank means "ask me where to save"
    };
}

function renderTables(tables: TableInfo[] | null) {
    const rows = list(tables);
    if (!rows.length) return `<p class="empty">No tables in this database.</p>`;
    const total = rows.reduce((n, t) => n + t.dataBytes + t.indexBytes, 0);
    const noPk = rows.filter((t) => !t.hasPk).map((t) => t.name);
    const notInnoDB = rows.filter((t) => t.engine && t.engine.toLowerCase() !== "innodb");

    const notes: string[] = [];
    if (noPk.length) notes.push(`${noPk.length} table(s) have no primary key (${noPk.slice(0, 5).join(", ")}${noPk.length > 5 ? "…" : ""}). Replication and InnoDB both suffer for it.`);
    if (notInnoDB.length) notes.push(`${notInnoDB.length} table(s) are not InnoDB, so they are not covered by a single-transaction backup and will be locked while it runs.`);

    const table = card(`Tables — ${rows.length}, ${bytes(total)} total`, `<div class="scroll-x"><table>
        <thead><tr><th>Table</th><th>Engine</th><th class="num">rows</th><th class="num">data</th><th class="num">index</th><th>collation</th><th>PK</th></tr></thead>
        <tbody>${rows.map((t) => `<tr class="${t.hasPk ? "" : "bad"}">
            <td class="mono">${esc(t.name)}</td>
            <td class="dim">${esc(t.engine)}</td>
            <td class="num">${int(t.rows)}</td>
            <td class="num">${bytes(t.dataBytes)}</td>
            <td class="num">${bytes(t.indexBytes)}</td>
            <td class="dim">${esc(t.collation)}</td>
            <td>${t.hasPk ? "●" : "✕"}</td>
        </tr>`).join("")}</tbody></table></div>`, true);

    return table + noteList("Notes", notes);
}

const bytes = (n: number) => {
    if (n < 1024) return `${n} B`;
    const units = ["KiB", "MiB", "GiB", "TiB"];
    let v = n / 1024, i = 0;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return `${v.toFixed(1)} ${units[i]}`;
};

async function refreshToolbox() {
    const box: Toolbox = await Tester.Toolbox();
    const line = (t: { name: string; found: boolean; version: string }) =>
        t.found ? `${t.name} ${t.version.replace(/^.*Ver /, "").split(" ")[0]}` : `${t.name} not found`;
    const el = $("bk-tools");
    el.textContent = `${line(box.dump)} · ${line(box.client)}${box.hint ? ` — ${box.hint}` : ""}`;
    el.className = box.dump.found && box.client.found ? "opts-note" : "opts-note warn";
    $<HTMLButtonElement>("bk-dump").disabled = !box.dump.found;
    $<HTMLButtonElement>("bk-restore").disabled = !box.client.found;
}

$("bk-list").addEventListener("click", () => void run());

$("bk-dump").addEventListener("click", async () => {
    if (running) return;
    setRunning(true);
    setStatus("Dumping…", "busy");
    try {
        const r: BackupResult = await Tester.Backup(backupOptions());
        if (!r.path) {
            setStatus("Backup cancelled.", "idle");
        } else {
            lastResult = r;
            $("out-backup").innerHTML = `<div class="verdict ok">Wrote ${bytes(r.bytes)} to ${esc(r.path)} in ${ms(r.durationMs)}.</div>`
                + card("Command", `<div class="mono dim">${esc(r.command)}</div>`)
                + (r.stderr ? card("mysqldump output", `<div class="mono dim">${esc(r.stderr)}</div>`) : "");
            setStatus(`Backup written: ${r.path}`, "ok");
        }
    } catch (err) {
        $("out-backup").innerHTML = `<div class="verdict bad">${esc(errText(err))}</div>`;
        setStatus(errText(err), "bad");
    } finally {
        setRunning(false);
    }
});

$("bk-restore").addEventListener("click", async () => {
    if (running) return;
    const db = val("database");
    if (!db) { setStatus("Select the database to restore into.", "bad"); return; }
    const go = await ask("Restore", `This runs the dump against “${db}” and overwrites whatever it touches. There is no undo.`,
        { label: `Type the database name to confirm`, value: "" });
    if (go === null) return;
    if (go.value.trim() !== db) { setStatus("Database name did not match; restore cancelled.", "bad"); return; }

    setRunning(true);
    setStatus("Restoring…", "busy");
    try {
        const r: RestoreResult = await Tester.Restore({ config: config(), confirm: true, path: "" });
        if (!r.path) {
            setStatus("Restore cancelled.", "idle");
        } else {
            lastResult = r;
            $("out-backup").innerHTML = `<div class="verdict ok">Restored ${bytes(r.bytes)} from ${esc(r.path)} in ${ms(r.durationMs)}.</div>`
                + (r.stderr ? card("mysql output", `<div class="mono dim">${esc(r.stderr)}</div>`) : "");
            setStatus("Restore finished.", "ok");
        }
    } catch (err) {
        $("out-backup").innerHTML = `<div class="verdict bad">${esc(errText(err))}</div>`;
        setStatus(errText(err), "bad");
    } finally {
        setRunning(false);
    }
});

/* ---------- updates ---------- */

const updateBtn = $<HTMLButtonElement>("update");

Tester.Version().then((v) => ($("version").textContent = v === "dev" ? "local build" : `v${v}`));

Events.On(Updater.Events.UpdateAvailable, (e: { data: { version: string } }) => {
    updateBtn.textContent = `Update to v${e.data.version}`;
    updateBtn.hidden = false;
});

// Both buttons open the framework's update window; it renders "up to date" too.
const checkUpdates = async () => {
    try {
        await Tester.CheckForUpdates();
    } catch (err) {
        setStatus(errText(err), "bad");
    }
};
updateBtn.addEventListener("click", () => void checkUpdates());
$("check-update").addEventListener("click", () => void checkUpdates());

/* ---------- boot ---------- */

loadForm();
void refreshProfiles();
void refreshToolbox();
$("conn").addEventListener("change", saveForm);
document.addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") void run();
});

/* ---------- custom select ---------- */

// The native popup is drawn by the OS and cannot be themed, so every <select>
// keeps its element (it stays the source of truth for value, options and
// change events) and gets a styled control rendered over it. Existing code that
// reads `.value` or rebuilds `.innerHTML` needs no changes.
function enhanceSelect(select: HTMLSelectElement) {
    select.classList.add("native-hidden");

    const wrap = document.createElement("div");
    wrap.className = "sel";
    const button = document.createElement("button");
    button.type = "button";
    button.className = "sel-btn";
    button.setAttribute("aria-haspopup", "listbox");
    button.setAttribute("aria-expanded", "false");
    button.innerHTML = `<span class="sel-label"></span>
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="6 9 12 15 18 9"/></svg>`;
    const menu = document.createElement("div");
    menu.className = "sel-menu";
    menu.setAttribute("role", "listbox");
    menu.hidden = true;

    select.parentNode!.insertBefore(wrap, select);
    wrap.append(select, button, menu);

    const label = button.querySelector(".sel-label") as HTMLElement;
    const sync = () => {
        label.textContent = select.selectedOptions[0]?.text ?? "";
        button.disabled = select.disabled;
        button.title = select.title;
    };

    // Options are rendered on open, so a select whose innerHTML was replaced
    // needs no notification.
    const render = () => {
        menu.innerHTML = "";
        for (const node of select.children) {
            if (node instanceof HTMLOptGroupElement) {
                const head = document.createElement("div");
                head.className = "sel-group";
                head.textContent = node.label;
                menu.append(head);
                for (const opt of node.children) addItem(opt as HTMLOptionElement);
            } else if (node instanceof HTMLOptionElement) {
                addItem(node);
            }
        }
    };

    function addItem(opt: HTMLOptionElement) {
        const item = document.createElement("div");
        item.className = "sel-item";
        item.setAttribute("role", "option");
        item.tabIndex = -1;
        item.textContent = opt.text;
        const selected = opt.selected && !opt.dataset.action;
        item.setAttribute("aria-selected", String(selected));
        if (selected) item.classList.add("is-selected");
        item.addEventListener("click", () => {
            close();
            // Selecting through the real element keeps one source of truth.
            select.selectedIndex = opt.index;
            select.dispatchEvent(new Event("change"));
        });
        menu.append(item);
    }

    const open = () => {
        if (select.disabled) return;
        render();
        menu.hidden = false;
        button.setAttribute("aria-expanded", "true");
        // Land on the current choice so the arrows walk from there.
        const start = menu.querySelector<HTMLElement>(".is-selected") ?? menu.querySelector<HTMLElement>(".sel-item");
        start?.scrollIntoView({ block: "nearest" });
        start?.focus();
        document.addEventListener("pointerdown", onOutside, true);
    };
    const close = () => {
        menu.hidden = true;
        button.setAttribute("aria-expanded", "false");
        document.removeEventListener("pointerdown", onOutside, true);
    };
    const onOutside = (e: Event) => {
        if (!wrap.contains(e.target as Node)) close();
    };

    button.addEventListener("click", () => (menu.hidden ? open() : close()));

    // Arrow keys move through the list; Enter commits, Escape backs out.
    wrap.addEventListener("keydown", (e) => {
        const items = [...menu.querySelectorAll<HTMLElement>(".sel-item")];
        if (menu.hidden) {
            if (e.key === "ArrowDown" || e.key === "Enter" || e.key === " ") { e.preventDefault(); open(); }
            return;
        }
        const at = items.indexOf(document.activeElement as HTMLElement);
        if (e.key === "Escape") { e.preventDefault(); close(); button.focus(); }
        else if (e.key === "ArrowDown") { e.preventDefault(); items[Math.min(at + 1, items.length - 1)]?.focus(); }
        else if (e.key === "ArrowUp") { e.preventDefault(); (at <= 0 ? items[0] : items[at - 1])?.focus(); }
        else if (e.key === "Enter" || e.key === " ") { e.preventDefault(); (document.activeElement as HTMLElement)?.click(); button.focus(); }
    });

    select.addEventListener("change", sync);
    // refreshProfiles rebuilds the options wholesale, so watch rather than
    // asking every caller to remember to re-sync.
    new MutationObserver(sync).observe(select, { childList: true, subtree: true, attributes: true });
    sync();
}

document.querySelectorAll<HTMLSelectElement>("select").forEach(enhanceSelect);
