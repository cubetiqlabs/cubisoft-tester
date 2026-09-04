import { Events } from "@wailsio/runtime";
import { Tester } from "../bindings/github.com/sombochea/cubisoft-tester";
import type {
    Config, DiagnoseResult, Hop, LatencyResult, Phase, Progress,
    Series, ServerInfo, SpeedResult, Step, TraceResult,
} from "../bindings/github.com/sombochea/cubisoft-tester";

type Tab = "diagnose" | "latency" | "trace" | "speed";

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;
const val = (id: string) => $<HTMLInputElement>(id).value.trim();
const num = (id: string) => Number($<HTMLInputElement>(id).value) || 0;
const checked = (id: string) => $<HTMLInputElement>(id).checked;

// Everything the server sends back — versions, errors, grants, hostnames — is
// untrusted text going into innerHTML, so it all goes through here first.
const escapeMap: Record<string, string> = { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" };
const esc = (s: unknown) => String(s ?? "").replace(/[&<>"']/g, (c) => escapeMap[c]);

const ms = (n: number) => `${n.toFixed(n < 10 ? 2 : 1)} ms`;
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

function setProgress(text: string, pct: number) {
    $("progress-fill").style.width = `${Math.max(0, Math.min(100, pct))}%`;
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
        $("db-list").innerHTML = dbs.map((d) => `<option value="${esc(d)}"></option>`).join("");
        setStatus(`${dbs.length} database(s) available. Click the Database field to pick one.`, "ok");
    } catch (err) {
        setStatus(String(err), "bad");
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
        out.innerHTML = `<div class="verdict bad">${esc(err)}</div>`;
        setStatus(String(err), "bad");
    } finally {
        setRunning(false);
    }
}

function failedStep(r: DiagnoseResult): string {
    const bad = r.steps.find((s) => !s.ok);
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

    const steps = card("Connection path", r.steps.map(stepRow).join(""), true);
    const server = r.server.version ? card("Server", serverInfo(r.server)) : "";
    const grants = r.server.grants.length
        ? card("Privileges", `<div class="mono dim">${r.server.grants.map((g) => esc(g)).join("<br/>")}</div>`)
        : "";

    return verdict + steps + server + noteList("Warnings", r.warnings) + grants;
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
    const verdict = `<div class="verdict ${r.warnings.length ? "" : "ok"}">${esc(r.verdict)}</div>`;
    return verdict + r.series.map(seriesCard).join("") + noteList("Warnings", r.warnings);
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
    ]) + spark(s.samples) + (s.error ? `<div class="err mono">${esc(s.error)}</div>` : "");
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

    const rows = r.hops.map(hopRow).join("");
    const table = card("Path", `<div class="scroll-x"><table>
        <thead><tr><th class="num">#</th><th>Address</th><th>Hostname</th><th class="num">probes</th><th class="num">avg</th></tr></thead>
        <tbody>${rows}</tbody></table></div>`, true);

    const note = r.note ? `<div class="verdict">${esc(r.note)}</div>` : "";
    return verdict + summary + table + note;
}

function hopRow(h: Hop) {
    const probes = h.rtts.map((v) => (v < 0 ? "*" : v.toFixed(1))).join("  ");
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
    const failed = r.phases.find((p) => !p.ok);
    const verdict = r.ok
        ? `<div class="verdict ok">${esc(r.summary)}</div>`
        : `<div class="verdict bad">Stopped at “${esc(failed?.name ?? "unknown phase")}”: ${esc(failed?.error ?? "")}${failed?.hint ? ` — ${esc(failed.hint)}` : ""}</div>`;

    const insert = r.phases.find((p) => p.name.startsWith("Insert"));
    const scan = r.phases.find((p) => p.name === "Select: full table scan");
    const tx = r.phases.find((p) => p.name.startsWith("Transactions: ") && p.opsPerSec > 0);
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
        <tbody>${r.phases.map(phaseRow).join("")}</tbody></table></div>`, true);

    return verdict + tiles + table + noteList("Warnings", r.warnings);
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

/* ---------- boot ---------- */

loadForm();
$("conn").addEventListener("change", saveForm);
document.addEventListener("keydown", (e) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") void run();
});
