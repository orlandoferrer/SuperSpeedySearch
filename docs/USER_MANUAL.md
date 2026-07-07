# Super Speedy Search — User Manual

This manual covers installing, configuring, running, and troubleshooting a
search node on every supported device type, plus the CLI and HTTP API.

Contents:

1. [How it works](#1-how-it-works)
2. [Building the binary](#2-building-the-binary)
3. [Configuration reference](#3-configuration-reference)
4. [Running on macOS](#4-running-on-macos)
5. [Running on Linux](#5-running-on-linux)
6. [Running on Synology NAS (Docker)](#6-running-on-synology-nas-docker)
7. [CLI reference](#7-cli-reference)
8. [Searching](#8-searching)
9. [HTTP API reference](#9-http-api-reference)
10. [The desktop GUI](#10-the-desktop-gui)
11. [Troubleshooting](#11-troubleshooting)
12. [Resetting and uninstalling](#12-resetting-and-uninstalling)

---

## 1. How it works

Every device that should be searchable runs one **node** (`sss-node run`).
A node:

- scans the folders ("roots") you configure and stores file **metadata**
  (names, paths, sizes, dates) in a local SQLite database — file *contents*
  are never copied anywhere;
- watches those folders for changes, so new/renamed/deleted files show up in
  search within seconds, with a full re-scan every 6 hours as a safety net;
- answers searches over a small HTTP API on port **37373**;
- announces itself on your network via mDNS/Bonjour so clients can find it
  without any configuration.

Searches fan out from a client (the `sss-node search` CLI today, a GUI later)
to every node, and results are merged. There is no central server; each node
is independent, and any number of clients can be open at once.

**Auth:** every node generates a random token on first run and requires it as
a `Bearer` token on every request. The token is printed in the startup log
and saved to a file named `auth_token` next to the database.

---

## 2. Building the binary

Requires Go 1.24+ (https://go.dev/dl). From the repository root:

```sh
go build -o sss-node ./cmd/sss-node
```

Cross-compile from any machine (no CGO needed):

```sh
GOOS=darwin  GOARCH=arm64 go build -o dist/sss-node-mac-arm64  ./cmd/sss-node  # Apple Silicon
GOOS=darwin  GOARCH=amd64 go build -o dist/sss-node-mac-intel  ./cmd/sss-node  # Intel Mac
GOOS=linux   GOARCH=amd64 go build -o dist/sss-node-linux      ./cmd/sss-node  # Linux/most NAS
GOOS=linux   GOARCH=arm64 go build -o dist/sss-node-linux-arm  ./cmd/sss-node  # ARM NAS/Pi
```

For Synology you normally build the Docker image instead (section 6).

---

## 3. Configuration reference

Each node reads one YAML file. The node looks for it in this order:

1. `-config /path/to/config.yaml` command-line flag
2. `SSS_CONFIG` environment variable
3. `/config/config.yaml` (Docker convention)
4. macOS: `~/Library/Application Support/SuperSpeedySearch/config.yaml`
5. `~/.config/super-speedy-search/config.yaml`
6. `/etc/super-speedy-search/config.yaml`
7. `./config.yaml`

Start from [config.example.yaml](../config.example.yaml). Every key:

| Key | Default | Meaning |
| --- | --- | --- |
| `node.id` | hostname | Unique machine identifier shown in results. Lowercase letters/digits/dashes. |
| `node.name` | same as id | Human-friendly display name. |
| `node.listen_addr` | `0.0.0.0:37373` | Address the API binds to. Use `192.168.x.x:37373` to bind one interface only. |
| `node.advertise` | `true` | Announce on mDNS. Set `false` for hidden nodes (reach them by URL). |
| `node.auth_required` | `true` | Require a bearer token on every request. |
| `node.auth_token` | `""` | Fixed token. Leave empty to auto-generate on first run (recommended). |
| `database.path` | `data/index.db` | Where the SQLite index lives. `~` is expanded. The `auth_token` file is stored beside it. |
| `scan.interval` | `6h` | Full reconciliation scan interval (`30m`, `6h`, `24h`...). Minimum `1m`. |
| `scan.follow_symlinks` | `false` | Whether scans descend into symlinks. Keep off to avoid loops. |
| `scan.tombstone_retention_days` | `30` | How long deleted files stay in the DB (marked deleted) before being purged. |
| `scan.watch.enabled` | `true` | Watch the filesystem for near-real-time updates. |
| `scan.watch.max_watched_dirs` | `50000` | Budget of watched directories per node. If a root exceeds it, watching turns off for that root and periodic scans take over. |
| `scan.watch.debounce_ms` | `500` | Delay before processing a burst of file events. |
| `roots[].id` | required | Unique id for the folder tree. |
| `roots[].path` | required | Absolute path to scan (`~` allowed). In Docker this is the *container* path, e.g. `/mnt/documents`. |
| `roots[].display_prefix` | `<name>:<id>` | Prefix shown in results, e.g. `Synology:Documents`. |
| `roots[].open_uri_prefix` | none | URI prefix for "open" actions, e.g. `smb://nas.local/documents` or `file:///Users/you/Documents`. |
| `roots[].enabled` | `true` | Disable a root without deleting its config. |
| `roots[].excludes.paths` | `.git`, `node_modules`, caches... | Names (match any folder anywhere) or root-relative prefixes (`sub/dir`) to skip. Setting this replaces the defaults. |
| `roots[].excludes.extensions` | none | File extensions to skip entirely, e.g. `[".tmp", ".part"]`. |
| `roots[].content_search.enabled` | `false` | Allow live content ("deep") search inside this root. |
| `roots[].content_search.max_file_size_mb` | `25` | Skip files bigger than this during content search. |
| `roots[].content_search.include_extensions` | none | Only these types are content-searched, e.g. `[".txt", ".md", ".csv", ".json"]`. |
| `content.pdf.enabled` | `false` | Enable PDF text extraction (needs `pdftotext`; see section 11). |
| `content.pdf.pdftotext_path` | auto | Explicit path to `pdftotext` if not on `PATH`. |
| `resource_limits.max_parallel_content_searches` | `2` | Concurrent deep searches the node accepts (extras get HTTP 429). |
| `resource_limits.max_search_seconds` | `60` | Hard time limit for one content search. |
| `resource_limits.max_results_per_query` | `500` | Hard cap on results per query. |

Config changes require a node restart.

---

## 4. Running on macOS

### First run

```sh
mkdir -p ~/Library/Application\ Support/SuperSpeedySearch
cp config.example.yaml ~/Library/Application\ Support/SuperSpeedySearch/config.yaml
# edit it: set node.id, node.name, your roots
sss-node run
```

You'll see something like:

```
level=INFO msg="generated auth token" token=27c8c1c3... persisted_to=.../auth_token
level=INFO msg="api listening" addr=0.0.0.0:37373
level=INFO msg="filesystem watcher started" watched_dirs=1234
level=INFO msg="advertising on mdns" service=_superspeedysearch._tcp instance=macbook-pro
level=INFO msg="scan finished" ... seen=48211 took=41s
```

Copy the token — clients need it. It stays available in the `auth_token`
file next to the database.

### Grant Full Disk Access (usually needed)

macOS blocks background processes from reading Documents, Desktop, Downloads,
and external volumes. If the log shows `operation not permitted` errors or
the scan finds suspiciously few files:

*System Settings → Privacy & Security → Full Disk Access* → add your terminal
app (for manual runs) or the `sss-node` binary (for launchd runs).

### Start automatically at login

```sh
sudo cp sss-node /usr/local/bin/
cp deploy/macos/com.superspeedysearch.node.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.superspeedysearch.node.plist
tail -f /tmp/sss-node.log     # watch it come up
```

To stop: `launchctl unload ~/Library/LaunchAgents/com.superspeedysearch.node.plist`.

### Firewall

If the macOS firewall is on, allow incoming connections for `sss-node` when
prompted (or under *System Settings → Network → Firewall → Options*),
otherwise other devices cannot query this node.

---

## 5. Running on Linux

### First run

```sh
mkdir -p ~/.config/super-speedy-search
cp config.example.yaml ~/.config/super-speedy-search/config.yaml
# edit it, then:
./sss-node run
```

### Run as a systemd service

```sh
sudo cp sss-node /usr/local/bin/
sudo mkdir -p /etc/super-speedy-search
sudo cp config.yaml /etc/super-speedy-search/
sudo cp deploy/linux/sss-node.service /etc/systemd/system/
# edit the User= line in the service file, then:
sudo systemctl daemon-reload
sudo systemctl enable --now sss-node
journalctl -u sss-node -f
```

### Raise the inotify limit for big roots

The watcher needs one inotify watch per directory. If the log says
`watching disabled for root ... budget exceeded` or `no space left on device`
(inotify's confusing error for "out of watches"):

```sh
# check current limit
cat /proc/sys/fs/inotify/max_user_watches
# raise it persistently
echo fs.inotify.max_user_watches=524288 | sudo tee /etc/sysctl.d/99-sss.conf
sudo sysctl --system
```

Also raise `scan.watch.max_watched_dirs` in the node config to match.

---

## 6. Running on Synology NAS (Docker)

### Build (or load) the image

On any machine with Docker:

```sh
docker build -t super-speedy-search-node .
# to move it to the NAS without a registry:
docker save super-speedy-search-node | gzip > sss-node.tar.gz
# upload, then on the NAS: docker load < sss-node.tar.gz
# (in Container Manager: Image → Add → From file)
```

### Set up the folders

Create a share/folder on the NAS, e.g. `/volume1/docker/sss/`, containing:

```
/volume1/docker/sss/config/config.yaml   <- start from deploy/synology/config/config.yaml
/volume1/docker/sss/data/                <- empty; index + token live here
```

Edit `config.yaml`: set `node.id`/`node.name`, and one root per share you
mount. **Root paths are container paths** (`/mnt/documents`), not NAS paths.

### Start with docker compose

Copy [deploy/synology/docker-compose.yml](../deploy/synology/docker-compose.yml)
next to those folders, adjust the volume mounts (one line per share, always
`:ro` read-only), then via SSH:

```sh
cd /volume1/docker/sss && docker compose up -d
docker logs super-speedy-search-node     # find the generated token here
```

Or in **Container Manager**: Project → Create → paste the compose file.

Notes:

- `network_mode: host` is required for mDNS discovery. If you remove it,
  map port 37373 and add the node to clients manually by URL.
- The token is also at `/volume1/docker/sss/data/auth_token`.
- Mount shares read-only (`:ro`); the node never needs to write to them.
- Exclude Synology system dirs — the example config already skips
  `#recycle` and `@eaDir`.
- Upgrades: rebuild the image, `docker compose up -d` again. The index and
  token survive because `/data` is a mounted volume.

---

## 7. CLI reference

One binary, four commands. Global pattern: `sss-node <command> [flags]`.

### `sss-node run` (default)

Runs the daemon: API server, mDNS advertisement, watcher, periodic scans.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-config path` | auto-detected (section 3) | Config file to use. |

### `sss-node scan`

Runs one reconciliation scan and exits. Useful for cron jobs, pre-warming an
index, or testing config changes. Don't run it while a daemon is already
scanning the same database.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-config path` | auto-detected | Config file to use. |
| `-root id` | all roots | Scan only this root. |

### `sss-node discover`

Lists nodes advertising on the LAN, with their URLs and metadata.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-timeout duration` | `3s` | How long to listen for announcements. |

Example output:

```
macbook-pro     http://192.168.1.25:37373  id=macbook-pro name=MacBook Pro version=0.1.0 auth=true
synology-main   http://192.168.1.40:37373  id=synology-main name=Synology version=0.1.0 auth=true
```

### `sss-node search`

Fans a metadata search out to nodes and prints merged, ranked results.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-node URL` | discover via mDNS | Query this node (repeat the flag for several). |
| `-token T` | `$SSS_TOKEN` | Bearer token. With multiple nodes, easiest if they share a token (set the same `auth_token` in each config). |
| `-ext .pdf` | all types | Filter by extension (repeatable). |
| `-limit N` | `50` | Max results per node. |
| `-timeout duration` | `3s` | mDNS discovery timeout (ignored with `-node`). |

Everything after the flags is the query; multiple words must all match
(AND semantics, case-insensitive, matches filenames and paths).

### Environment variables

| Variable | Used by | Meaning |
| --- | --- | --- |
| `SSS_CONFIG` | `run`, `scan` | Config file path (overridden by `-config`). |
| `SSS_TOKEN` | `search` | Default bearer token (overridden by `-token`). |

---

## 8. Searching

```sh
export SSS_TOKEN=<your token>

sss-node search tax 2024                # all nodes, all file types
sss-node search -ext .pdf -ext .xlsx tax    # only PDFs and spreadsheets
sss-node search -node http://192.168.1.40:37373 -limit 100 invoice
```

Ranking: exact filename matches first, then filenames containing all terms,
then path-only matches; ties broken by most recently modified.

Content ("deep") search is available over the API today (section 9) and in
the upcoming GUI; it greps inside files of the types allowed by each root's
`content_search` config, live, without a content index.

---

## 9. HTTP API reference

All endpoints need `Authorization: Bearer <token>` unless the node has
`auth_required: false`. Base URL: `http://<node>:37373`.

### `GET /v1/status`

```sh
curl -s -H "Authorization: Bearer $SSS_TOKEN" http://nas:37373/v1/status
```

Returns node id/name/version, capabilities, `indexed_files`, and
`last_scan_finished_at`.

### `GET /v1/roots`

Configured roots with paths, display prefixes, and whether content search is
enabled.

### `POST /v1/search/metadata`

```sh
curl -s -H "Authorization: Bearer $SSS_TOKEN" http://nas:37373/v1/search/metadata -d '{
  "query": "tax 2024",
  "limit": 100,
  "extensions": [".pdf", ".xlsx"],
  "root_ids": ["nas-documents"],
  "include_dirs": false
}'
```

All fields except `query` are optional. Results include `path`,
`display_path`, `open_uri`, `size_bytes`, `modified_at`, and `match_type`
(`filename_exact` | `filename` | `path`).

### `POST /v1/search/content`

Streams NDJSON — one JSON object per line: `result` events as matches are
found, then exactly one `summary`:

```sh
curl -sN -H "Authorization: Bearer $SSS_TOKEN" http://nas:37373/v1/search/content -d '{
  "query": "property tax",
  "extensions": [".txt", ".md"],
  "root_ids": ["nas-documents"],
  "limit": 100,
  "max_seconds": 30
}'
{"type":"result","result":{"display_path":"Synology:Documents/taxes/note.txt","snippet":"...the property tax statement...","line":12,...}}
{"type":"summary","summary":{"searched_files":1200,"skipped_files":8,"errors":0,"timed_out":false,"truncated":false}}
```

Close the connection to cancel. Returns HTTP 429 when
`max_parallel_content_searches` is exceeded.

### Scan control

```sh
curl -s -X POST -H "Authorization: Bearer $SSS_TOKEN" http://nas:37373/v1/scan            # all roots
curl -s -X POST -H "Authorization: Bearer $SSS_TOKEN" http://nas:37373/v1/scan -d '{"root_id":"nas-documents"}'
curl -s -H "Authorization: Bearer $SSS_TOKEN" http://nas:37373/v1/scan/current            # progress
curl -s -H "Authorization: Bearer $SSS_TOKEN" http://nas:37373/v1/scan/history            # last 20 runs
```

`POST /v1/scan` returns 202 immediately (409 if a scan is already running).

### `GET /v1/config`

The node's effective configuration with the auth token redacted.

---

## 10. The desktop GUI

The `gui/` directory contains a desktop app built with
[Wails](https://wails.io) (Go backend, system webview — no Electron).

### Building

```sh
go install github.com/wailsapp/wails/v2/cmd/wails@latest
cd gui
wails build        # → gui/build/bin/SuperSpeedySearch.app (macOS)
wails dev          # development mode with live reload
```

No npm needed: the frontend is plain HTML/CSS/JS with no build step. A plain
Go binary also works without the wails CLI:

```sh
go build -tags desktop,production -o SuperSpeedySearch ./gui
```

### Using it

- **Nodes sidebar** — nodes found via mDNS appear automatically; use **+** to
  add one by URL (VPN/other subnets). Checkboxes choose which nodes a search
  fans out to. Hover a node for actions: set its token (🔑), trigger a rescan
  (↻), remove a manual node (✕).
- **Tokens** — a node showing *needs token* in red rejected the GUI. Paste
  its token via 🔑, or use *Set default token…* (bottom left) if all your
  nodes share one token.
- **Names mode** — filename/path search across the selected nodes, globally
  ranked, with type filters and a result limit.
- **Contents mode** — live deep search; matches stream in as they're found,
  with highlighted snippets and line numbers. Esc or **Cancel** stops it. The
  bar at the bottom reports files searched/skipped per the nodes' summaries.
- **Row actions** (hover a result) — *Copy* the full path, *Open* the result
  URI (`smb://` mounts the share; `file://` opens locally), *Reveal* shows a
  local file in Finder.
- ⌘F focuses the search box.

GUI settings (manual nodes, tokens) live in
`~/Library/Application Support/SuperSpeedySearch/gui.json` on macOS,
`~/.config/SuperSpeedySearch/gui.json` on Linux. The file is user-readable
only (0600) since it contains tokens.

## 11. Troubleshooting

### "401 unauthorized" / missing or invalid bearer token

You didn't send the token, or sent the wrong one. Find it in the node's
startup log or in the `auth_token` file next to the database
(`/data/auth_token` in Docker). Send it as `Authorization: Bearer <token>`.
To rotate it, delete the `auth_token` file and restart the node.

### `sss-node discover` finds nothing

- The nodes must be on the **same LAN/VLAN**; mDNS does not cross subnets or
  VPNs. For remote nodes use `-node http://host:37373` instead — discovery
  is a convenience, never a requirement.
- Docker nodes: mDNS requires `network_mode: host` in the compose file.
- Check the node's log for `advertising on mdns`. If it's absent, the node
  has `advertise: false` or registration failed (logged as a warning).
- Verify from a Mac with the system browser:
  `dns-sd -B _superspeedysearch._tcp local.` — if this sees the node but
  `sss-node discover` doesn't, please file a bug (include OS and network).
- Some routers/APs have "multicast filtering" or "IGMP snooping" settings
  that block mDNS between wired and Wi-Fi — check those.

### Node is discovered but connections are refused

The node advertised its LAN IP but is bound elsewhere. Ensure
`listen_addr` is `0.0.0.0:37373` (all interfaces) or the LAN interface's IP —
not `127.0.0.1`.

### Scan finds far fewer files than expected (macOS)

Grant Full Disk Access (section 4). The scan doesn't fail — macOS silently
returns permission errors for protected folders, which show up as `errors`
in `/v1/scan/history`.

### "watching disabled for root ... budget exceeded"

The root has more directories than `scan.watch.max_watched_dirs` (or, on
macOS, the process ran out of file descriptors — kqueue uses one per watched
item). The node still works; freshness just falls back to the periodic scan.
Options:

- raise `scan.watch.max_watched_dirs` (Linux: also raise the inotify sysctl,
  section 5; macOS: raise `ulimit -n`),
- add excludes for huge subtrees you don't care about,
- or lower `scan.interval` (e.g. `30m`) and accept scan-based freshness.

### New files don't show up in search

1. Watcher disabled or budget-exceeded? Check the startup log.
2. Docker: events only fire for changes made *on the NAS itself*. Changes
   made over SMB from another machine usually do fire inotify on the NAS,
   but some filesystems/shares don't — the periodic scan is the guarantee.
3. Force it: `curl -X POST .../v1/scan` or `sss-node scan`.

### Content search returns nothing

- The root needs `content_search.enabled: true` **and** the file's extension
  in `include_extensions`.
- Files over `max_file_size_mb` are skipped, as are binary files.
- Check the `summary` event: `skipped_files` and `errors` tell you what was
  passed over.

### PDF search doesn't work

Three switches must be on: `content.pdf.enabled: true`, `.pdf` listed in the
root's `include_extensions`, and `pdftotext` installed (`brew install
poppler` on macOS, `apt install poppler-utils` on Linux, already in the
Docker image). The startup log warns if `pdftotext` can't be found.

### "a scan is already running" (HTTP 409)

Only one scan runs at a time. Wait, or check progress at `/v1/scan/current`.

### Port 37373 already in use

Another node (or an old process) is running. `pgrep -fl sss-node`, kill it,
or change `listen_addr` to another port. Nodes on different ports advertise
correctly over mDNS.

### Node is slow / NAS CPU spikes

Scans are throttled and workers are conservative by default, but you can:
raise `scan.interval`, add excludes, lower
`resource_limits.max_parallel_content_searches` to 1, and reduce
`content_search.max_file_size_mb`.

### Database file grows

Metadata is ~150–300 bytes per file — 1M files is roughly 200–300 MB
including indexes. Deleted files are kept (tombstoned) for
`tombstone_retention_days`, then purged automatically after a scan. The
`*.db-wal` file is normal SQLite write-ahead-log churn.

---

## 12. Resetting and uninstalling

**Reset the index** (e.g. after big config changes): stop the node, delete
`index.db`, `index.db-wal`, `index.db-shm` (keep `auth_token` to keep the
token), start the node — it rescans from scratch.

**Uninstall (macOS):**

```sh
launchctl unload ~/Library/LaunchAgents/com.superspeedysearch.node.plist 2>/dev/null
rm -f ~/Library/LaunchAgents/com.superspeedysearch.node.plist /usr/local/bin/sss-node
rm -rf ~/Library/Application\ Support/SuperSpeedySearch
```

**Uninstall (Linux):**

```sh
sudo systemctl disable --now sss-node
sudo rm -f /etc/systemd/system/sss-node.service /usr/local/bin/sss-node
sudo rm -rf /etc/super-speedy-search
```

**Uninstall (Synology):** `docker compose down`, delete the container/image
in Container Manager, remove `/volume1/docker/sss/`.
