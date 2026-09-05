# MySQL Tester

Desktop app for diagnosing a MySQL connection from the machine that actually has
the problem. Targets MySQL 5.6 through 8.x. Built with Go and [Wails v3](https://v3.wails.io).

| Tab | Answers |
|---|---|
| **Diagnose** | Where does the connection break, and how is this server configured? |
| **Latency** | How long is a round trip, how much does it vary, what does a new connection cost? |
| **Trace route** | What path do packets take, and is the MySQL port reachable? |
| **Speed test** | How fast can this client insert, read, update, commit and delete? |
| **Backup** | What is in this database, how big is it, and can I dump and restore it? |

The speed test draws itself as it runs: throughput per phase on a log axis, so a
bulk insert at 10 MiB/s and a cached scan at 500 are both readable, and a stall
or a sawtooth shows up where the summary numbers cannot.

Connections you use often can be saved as profiles from the picker in the
sidebar. Tick "Save the password too" and the app asks for a secret key, then
keeps the whole profile file encrypted. Export and import live in the **File**
menu; give the export a key of its own and the saved passwords travel with it.
An exported file can also be dropped straight onto the window — you are asked
for its key only if it has one.

Appearance follows the system theme by default; **View → Appearance** switches
it to Light or Dark and remembers the choice.

## Install

**macOS / Linux**

```sh
curl -fsSL https://raw.githubusercontent.com/cubetiqlabs/cubisoft-tester/main/scripts/install.sh | sh
```

**Windows** (PowerShell)

```powershell
irm https://raw.githubusercontent.com/cubetiqlabs/cubisoft-tester/main/scripts/install.ps1 | iex
```

Both scripts pick the right build for your OS and CPU, verify the SHA-256 against
the release's `checksums.txt`, and install. Set `VERSION=1.2.3` to pin one.

Prefer to do it yourself? Grab the archive from the
[releases page](https://github.com/cubetiqlabs/cubisoft-tester/releases):

| Platform | Asset | Where to put it |
|---|---|---|
| macOS (Apple silicon / Intel) | `..._darwin_arm64.zip` / `..._darwin_amd64.zip` | `/Applications` |
| Linux (x86-64 / ARM64) | `..._linux_amd64.tar.gz` / `..._linux_arm64.tar.gz` | anywhere on `PATH` |
| Windows (x86-64 / ARM64) | `..._windows_amd64.zip` / `..._windows_arm64.zip` | anywhere you like |

macOS builds are ad-hoc signed, not notarised. If Gatekeeper objects, run
`xattr -dr com.apple.quarantine "/Applications/MySQL Tester.app"`.

## Updates

The app checks GitHub for a newer release on startup and offers it in the sidebar.
"Check for updates" does the same on demand. Downloads are verified against the
release checksums before anything is replaced. Nothing installs without a click.

## Local development

Needs Go 1.25+ and Node 22+. On Linux, either `libgtk-4-dev` and
`libwebkitgtk-6.0-dev` (the Wails default), or `libgtk-3-dev` and
`libwebkit2gtk-4.1-dev` with `EXTRA_TAGS=gtk3` — which is what the released
Linux builds use, since GTK4 WebKit is missing on anything older than Ubuntu 24.04.

```sh
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.16

wails3 task dev      # live reload
wails3 task build    # binary in bin/
wails3 task package  # .app bundle on macOS
```

Tests:

```sh
mkdir -p frontend/dist    # main.go embeds it; go:embed fails on an empty match
go test -tags server .    # unit tests, no server, no GUI toolchain needed

docker run -d --rm -e MYSQL_ROOT_PASSWORD=testpw -e MYSQL_DATABASE=testdb \
  -p 13306:3306 mysql:8.0
MYSQLTEST_HOST=127.0.0.1 MYSQLTEST_PORT=13306 go test -tags server -run Live -v .
```

The live tests exercise every tab against a real server: that the speed test
leaves no table behind, and that a backup can be dumped, the table dropped, and
the rows restored. CI runs them against MySQL 5.7 and 8.0.

Backup and restore need `mysqldump` and `mysql` on the machine (`brew install
mysql-client`, `apt install mysql-client`); the app says so when they are missing.

## Releasing

`version.txt` is the single source of truth. The script bumps it, commits, tags
and pushes; the tag triggers the build and publishes the release.

```sh
scripts/release.sh patch          # or minor / major / an exact 1.4.0
scripts/release.sh 1.4.0 --force  # move a tag that already exists
```

## Privacy

This tool has no telemetry, no analytics, and no accounts. Nothing about you or
your servers is collected or sent anywhere.

- The app makes exactly three kinds of outbound connection: to the MySQL server
  you typed in, ICMP probes to that host during a trace, and `api.github.com`
  to check for a new release.
- Results stay on your machine until you copy them yourself. "Copy report"
  puts JSON on your clipboard and nowhere else.
- The connection form (never the password) is kept in the app's local storage
  so you don't retype it, and the appearance choice in a `settings.json` beside
  the profiles.
- Profiles live in one file: `profiles.json` under your OS config directory —
  hover the profile picker for the exact path. Without a secret key it is plain
  JSON, and passwords are refused rather than written to it. With one, the whole
  list is encrypted with AES-256-GCM under a key derived by PBKDF2-SHA256
  (600,000 iterations), and a password is kept only for profiles you asked to
  keep it for. The key is held in memory for the session only, and there is no
  recovery if you lose it.
- Export asks for a key of its own, separate from the local one. With a key the
  file is encrypted and carries the saved passwords; without one it is plain
  JSON and the passwords are stripped out of it.
- Backup and restore run the `mysqldump` and `mysql` binaries already on your
  machine. Credentials are passed in a 0600 temp file, never on the command
  line where every process listing would see them.
- The speed test writes to the database you select. It creates one `conntest_*`
  table and drops it when finished. Point it at a scratch database.

## Credits

Built by [Sambo Chea](https://github.com/sombochea).

Copyright © 2026 [Cubis](https://cubis.tech).
