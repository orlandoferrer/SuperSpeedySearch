# Super Speedy Search Plan

## 1. Product Goal

Build a low-resource file search system for multiple computers and Synology NAS devices on a home network.

Each machine or NAS container runs a local search node. A separate GUI discovers those nodes, sends searches to them, and displays combined results. Nodes keep their own local index and search only the files they are configured to expose.

The first version should optimize for:

- Low idle CPU and memory use.
- Fast filename/path search.
- Practical live content search without indexing every file body.
- Simple deployment on macOS, Linux, and Synology Docker.
- A clean architecture that can later support remote nodes, authentication, and richer content indexing.

## 2. Core Architecture

### Components

1. **Search Node**
   - Written in Go.
   - Runs as a daemon on computers or as a Docker container on Synology NAS.
   - Scans configured filesystem roots.
   - Stores file metadata in local SQLite.
   - Exposes an HTTP API for status, metadata search, live content search, config, and scan control.
   - Advertises itself on the LAN using mDNS.

2. **GUI Coordinator**
   - Desktop or local web GUI.
   - Discovers nodes via mDNS.
   - Allows manual node URLs for future remote/VPN/server use.
   - Sends searches to all selected nodes.
   - Aggregates, ranks, filters, and displays results.
   - Provides actions like copy path, reveal/open location when possible, and later download/open through a node.

3. **Local SQLite Database Per Node**
   - Stores metadata only by default.
   - Optionally supports SQLite FTS5 later for content indexing of cheap text formats.
   - Does not centralize all file contents.

### High-Level Flow

1. Node starts.
2. Node loads config.
3. Node opens SQLite database.
4. Node runs a low-priority reconciliation scan on configured roots.
5. Node optionally watches roots for filesystem changes where supported.
6. GUI discovers or manually connects to nodes.
7. User searches.
8. GUI sends query to nodes.
9. Nodes return metadata matches quickly.
10. If deep search is requested, nodes perform live content search and stream additional matches.

## 3. Recommended V1 Decisions

### Language

Use Go for the node.

Reasons:

- Single static-ish binary.
- Good filesystem and networking support.
- Works well in Docker.
- Good fit for low resource daemons.
- Easy to expose HTTP APIs.

### Node API

Use HTTP JSON for v1.

Reasons:

- Easy to debug with curl.
- Simple for any GUI stack to consume.
- Easier than gRPC for early iteration.
- Can later support TLS, auth headers, reverse proxies, and remote nodes.

### Database

Use SQLite per node.

Reasons:

- Embedded and simple.
- Good enough for millions of metadata rows if indexed properly.
- No separate service on NAS.
- Mature Go drivers.

Go driver decision: use `modernc.org/sqlite` (pure Go, no CGO, cross-compiles cleanly for Synology Docker). It includes FTS5, so FTS5 support is not a reason to prefer CGO — it is only somewhat slower than CGO SQLite, which is acceptable at the expected scale (under ~2M rows per device). Switch to `github.com/mattn/go-sqlite3` only if measured performance demands it.

Concurrency rules (the scanner writes while the API reads):

- Enable WAL mode and a `busy_timeout` (e.g. 10s) at open.
- Keep a single writer goroutine; batch scan upserts into transactions of a few hundred rows. Per-file transactions would make scans orders of magnitude slower.
- Readers (search API) run concurrently under WAL without blocking the writer.

### Discovery

Use mDNS for LAN discovery, plus manual node URLs.

Reasons:

- mDNS makes home network setup easy.
- Manual URLs make VPN, remote servers, and non-mDNS networks possible later.

Advertise service:

```text
_superspeedysearch._tcp.local
```

### Content Search

V1 should use live content search by default.

Content indexing should be designed as an optional later feature, not required for v1.

Reasons:

- Keeps NAS resource use low.
- Avoids surprise database growth.
- Avoids expensive PDF extraction during background indexing.
- Keeps first implementation simpler.

Add optional FTS5 later for cheap formats:

- `.txt`
- `.md`
- `.csv`
- `.json`
- source code files
- small HTML/XML files

Keep PDF content search live-only at first, unless explicitly enabled per node/root.

## 4. FTS5 Position

SQLite FTS5 is a full-text search extension built into SQLite builds that include it. It creates specialized search indexes for text, allowing fast word, phrase, prefix, and ranked search.

FTS5 is not a place to store the original files. It stores searchable text/index structures derived from extracted text.

Potential use:

```sql
CREATE VIRTUAL TABLE file_content_fts USING fts5(
  path,
  title,
  body,
  content='',
  tokenize='unicode61'
);
```

For this project, FTS5 should be an acceleration option, not the foundation of v1.

Suggested future modes:

- `metadata_only`: default.
- `live_content`: scan contents on request.
- `indexed_light_text`: index configured lightweight text formats into FTS5.
- `indexed_pdf`: opt-in only, disabled by default.

Controls needed before enabling FTS5 broadly:

- Max indexed file size.
- Allowed file extensions.
- Excluded roots.
- Background indexing schedule.
- Max index bytes per root.
- Worker count.
- Idle-only mode.
- Rebuild and vacuum controls.

## 5. Node Responsibilities

### Filesystem Scanning

Each node scans configured roots and records metadata.

V1 includes filesystem watching (freshness target: minutes, not hours), but correctness must not depend on watchers alone — the periodic reconciliation scan remains authoritative.

Reasons:

- File watchers can miss events.
- Docker bind mounts may behave differently.
- NAS filesystems and network shares can be quirky.
- Sleep/wake cycles can lose events.

Recommended approach:

- Filesystem watchers (fsnotify) enabled by default for near-real-time updates.
- Watchers have a per-root watched-directory budget (inotify needs one watch per directory; kqueue on macOS consumes a file descriptor per watched item). If a root exceeds the budget, watching is disabled for that root and the node falls back to periodic scans.
- Periodic reconciliation scan remains authoritative and repairs anything watchers missed.
- Track `last_seen_scan_id` to detect deletes/missing files.
- Change detection: a file is considered changed when its size or mtime differs. Without inode tracking, a rename appears as delete + add — acceptable for v1.
- Throttle scanning to avoid high CPU/disk use.
- Purge tombstones (`is_deleted = 1` rows) after a retention window (default 30 days) so the database does not grow forever.

### Metadata Extraction

Store per file (keep it minimal — everything else is derived):

- Root ID.
- Relative path within the root.
- Filename and lowercase filename for search.
- Extension.
- Size.
- Modified time.
- Created time if available.
- Last seen time and scan ID.
- Deleted/missing flag.
- Error state if file is inaccessible.

Do NOT store absolute path, display path, or open URIs per row. They are all derivable at response time from the root's `path`, `display_prefix`, and `open_uri_prefix` plus the relative path. Storing them per row would multiply database size roughly 5x at millions of rows and make root reconfiguration require rewriting every row.

Avoid hashing all files in v1 because it is expensive. Add optional hashing later for duplicate detection.

### Content Search

Live content search should:

- Select candidate files from the metadata index (SQL filter on extension, size, and root), never by re-walking the filesystem. This is the main payoff of having the metadata database — it avoids re-paying directory traversal on slow NAS disks for every search.
- Respect filetype allowlist.
- Respect max file size.
- Respect ignored paths.
- Stream results incrementally.
- Include context snippets.
- Be cancellable.
- Use low worker counts by default.
- Apply strict timeouts.
- Report skipped files and errors as summary counts.

Suggested v1 content formats:

- Plain text.
- Markdown.
- CSV/TSV.
- JSON/YAML/TOML/XML.
- Common source code files.
- PDF if a reliable extractor is available and enabled.

PDF extraction should be plugin-like internally because dependency choices matter. Options include:

- Call an external `pdftotext` binary if installed.
- Use a Go PDF extraction library.
- Disable PDF content search unless configured.

For Synology containers, external `pdftotext` may be easiest if included in the image.

## 6. GUI Responsibilities

### Core Screens

1. **Search**
   - Query input.
   - Search mode selector: filename/path, content, both.
   - Node selector.
   - Filters for extension, node, root, date, size.
   - Results grouped by node or sorted globally.
   - Incremental loading state for live content searches.

2. **Nodes**
   - Discovered nodes.
   - Manually configured nodes.
   - Node health.
   - Last scan time.
   - Indexed file count.
   - Search capabilities.

3. **Node Config**
   - Scan roots.
   - Exclude paths.
   - Exclude file extensions.
   - Content search allowlist.
   - Resource limits.
   - Rescan button.

4. **Result Actions**
   - Copy path.
   - Copy display path.
   - Open/reveal location if available.
   - Open SMB URI if available.
   - Later: download/open via node.

### GUI Stack Options

Recommended candidates:

1. **Tauri**
   - Good desktop app feel.
   - Smaller than Electron.
   - Can call OS-specific open/reveal commands.
   - Rust backend adds complexity, but frontend can stay web-based.

2. **Electron**
   - Easiest desktop integrations.
   - Heavier runtime.
   - Probably acceptable if developer speed matters more than footprint.

3. **Local Web App**
   - Simplest architecture.
   - Can run in browser.
   - Harder to implement "open in Finder" securely and portably.

Recommendation: choose Tauri if the GUI is meant to feel like a real desktop utility. Choose local web app if you want fastest iteration first.

## 7. Open/Reveal Behavior

Result paths are tricky because a result may point to:

- A file on the same Mac as the GUI.
- A file on another computer.
- A file inside a NAS Docker container path.
- A file available through SMB.
- A future remote-only server path.

The node should return structured location data instead of assuming every path is locally openable.

Example:

```json
{
  "node_id": "synology-main",
  "path": "/volume1/documents/taxes/2024.pdf",
  "display_path": "synology-main:/volume1/documents/taxes/2024.pdf",
  "open_uri": "smb://synology-main/documents/taxes/2024.pdf",
  "parent_open_uri": "smb://synology-main/documents/taxes/",
  "capabilities": ["metadata_search", "live_content_search", "open_uri"]
}
```

The GUI should only show actions supported by the result/node.

For V1:

- Mac node results can support "Reveal in Finder" locally.
- Synology results can support SMB open URI if configured.
- Other remote results can default to copy path.

## 8. Security Model

Even for home-only v1, design the API boundary with future security in mind.

V1 minimum:

- Token auth on by default: if no token is configured, the node generates a random token on first run, persists it next to the database, and logs where to find it. Auth can be explicitly disabled (`auth_required: false`) for trusted networks.
- Token passed in `Authorization: Bearer <token>` on every endpoint, including scan control.
- `GET /v1/config` must redact the auth token.
- Bind node to LAN interface or configured host.
- Do not expose node API to the internet.
- Do not allow arbitrary path reads through the API.
- Only search configured roots.

Rationale for default-on auth: mDNS advertisement plus an open API would let any device on the LAN (guest phones, IoT devices) enumerate every filename on the NAS, and trigger scans that burn NAS CPU. Filenames alone are sensitive.

V2-ready design:

- Node identity.
- GUI trust store.
- TLS support.
- Pairing flow.
- Per-node capabilities.
- Remote nodes entered manually by URL.
- Optional reverse proxy compatibility.

Avoid putting mDNS discovery at the center of the architecture. Treat it as one way to discover nodes.

## 9. Configuration

Use a config file per node.

Recommended location:

- macOS daemon: `~/Library/Application Support/SuperSpeedySearch/config.yaml`
- Linux: `~/.config/super-speedy-search/config.yaml` or `/etc/super-speedy-search/config.yaml`
- Docker: `/config/config.yaml`

Example:

```yaml
node:
  id: "macbook-pro"
  name: "MacBook Pro"
  listen_addr: "0.0.0.0:37373"
  advertise: true
  # Auth is on by default. Leave auth_token empty to have the node generate
  # a token on first run and persist it next to the database.
  auth_required: true
  auth_token: ""

database:
  path: "/data/index.db"

scan:
  interval: "6h"
  worker_count: 2
  follow_symlinks: false
  tombstone_retention_days: 30
  watch:
    enabled: true
    max_watched_dirs: 50000
    debounce_ms: 500

roots:
  - id: "home-documents"
    path: "/Users/orlando/Documents"
    display_prefix: "MacBook Pro:Documents"
    open_uri_prefix: "file:///Users/orlando/Documents"
    enabled: true
    excludes:
      paths:
        - ".git"
        - "node_modules"
      extensions:
        - ".tmp"
        - ".cache"
    content_search:
      enabled: true
      max_file_size_mb: 25
      include_extensions:
        - ".txt"
        - ".md"
        # Add ".pdf" only on roots where PDF extraction is deliberately enabled.

content:
  pdf:
    enabled: false
    pdftotext_path: ""  # auto-detect on PATH when enabled

resource_limits:
  max_parallel_content_searches: 2
  max_search_seconds: 60
  max_results_per_query: 500
```

For Docker/Synology:

```yaml
database:
  path: "/data/index.db"

roots:
  - id: "nas-documents"
    path: "/mnt/documents"
    display_prefix: "Synology:Documents"
    open_uri_prefix: "smb://synology.local/documents"
```

## 10. SQLite Schema Draft

### `nodes`

Mostly useful if the GUI has its own local cache. Not required in node DB.

### `scan_roots`

```sql
CREATE TABLE scan_roots (
  id TEXT PRIMARY KEY,
  path TEXT NOT NULL,
  display_prefix TEXT NOT NULL,
  open_uri_prefix TEXT,
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
```

All timestamps are INTEGER unix epoch seconds — cheaper to index and compare than TEXT.

### `files`

Absolute path, display path, and open URIs are not stored per row — they are derived at response time from `scan_roots` plus `relative_path` (see section 5).

```sql
CREATE TABLE files (
  id INTEGER PRIMARY KEY,
  root_id TEXT NOT NULL,
  relative_path TEXT NOT NULL,
  filename TEXT NOT NULL,
  filename_lower TEXT NOT NULL,
  extension TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  modified_at INTEGER,
  created_at INTEGER,
  indexed_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  last_seen_scan_id TEXT,
  is_deleted INTEGER NOT NULL DEFAULT 0,
  deleted_at INTEGER,
  is_dir INTEGER NOT NULL DEFAULT 0,
  error TEXT,
  UNIQUE(root_id, relative_path)
);
```

Indexes — partial, because every search filters `is_deleted = 0` and a boolean index alone is near-useless:

```sql
CREATE INDEX idx_files_filename_lower ON files(filename_lower) WHERE is_deleted = 0;
CREATE INDEX idx_files_extension ON files(extension) WHERE is_deleted = 0;
CREATE INDEX idx_files_root_id ON files(root_id) WHERE is_deleted = 0;
CREATE INDEX idx_files_modified_at ON files(modified_at) WHERE is_deleted = 0;
CREATE INDEX idx_files_last_seen_scan_id ON files(last_seen_scan_id);
```

Substring filename search in v1 is `LIKE '%query%'`, which is a full table scan — the `filename_lower` index cannot help with a leading wildcard; it only serves exact/prefix lookups. At the expected scale (under ~2M rows per device) a scan costs at most hundreds of milliseconds, which is acceptable. Path-term matching uses `LOWER(relative_path) LIKE ...` in the same scan. If this proves too slow, move filenames/paths into an FTS5 table with the trigram tokenizer (SQLite >= 3.34, available in `modernc.org/sqlite`).

### `scan_runs`

```sql
CREATE TABLE scan_runs (
  id TEXT PRIMARY KEY,
  root_id TEXT,  -- NULL = all roots; per-root scans come from the rescan button
  started_at INTEGER NOT NULL,
  finished_at INTEGER,
  status TEXT NOT NULL,
  files_seen INTEGER NOT NULL DEFAULT 0,
  files_updated INTEGER NOT NULL DEFAULT 0,
  files_deleted INTEGER NOT NULL DEFAULT 0,
  errors INTEGER NOT NULL DEFAULT 0
);
```

### Future `file_content_fts`

Add only when implementing indexed content search.

```sql
CREATE VIRTUAL TABLE file_content_fts USING fts5(
  file_id UNINDEXED,
  path,
  title,
  body,
  tokenize='unicode61'
);
```

## 11. Node HTTP API Draft

### Health

```http
GET /v1/status
```

Response:

```json
{
  "node_id": "macbook-pro",
  "name": "MacBook Pro",
  "version": "0.1.0",
  "started_at": "2026-07-06T12:00:00Z",
  "capabilities": ["metadata_search", "live_content_search", "mdns", "open_uri"],
  "indexed_files": 123456,
  "last_scan_finished_at": "2026-07-06T13:00:00Z"
}
```

### Roots

```http
GET /v1/roots
```

### Metadata Search

```http
POST /v1/search/metadata
```

Request:

```json
{
  "query": "tax 2024",
  "limit": 100,
  "extensions": [".pdf", ".xlsx"],
  "root_ids": ["nas-documents"]
}
```

Response:

```json
{
  "results": [
    {
      "node_id": "synology-main",
      "root_id": "nas-documents",
      "path": "/mnt/documents/taxes/2024.pdf",
      "display_path": "Synology:Documents/taxes/2024.pdf",
      "open_uri": "smb://synology.local/documents/taxes/2024.pdf",
      "parent_open_uri": "smb://synology.local/documents/taxes/",
      "filename": "2024.pdf",
      "extension": ".pdf",
      "size_bytes": 12345,
      "modified_at": "2026-01-01T00:00:00Z",
      "match_type": "metadata"
    }
  ]
}
```

### Live Content Search

```http
POST /v1/search/content
```

Decision: stream newline-delimited JSON (NDJSON) over the POST response. SSE was considered, but browser `EventSource` only supports GET, which conflicts with the POST search body; NDJSON is simpler on both ends and cancellation is just closing the connection.

Request:

```json
{
  "query": "property tax",
  "limit": 100,
  "extensions": [".txt", ".md", ".pdf"],
  "root_ids": ["nas-documents"],
  "max_seconds": 60
}
```

Streaming result event:

```json
{
  "type": "result",
  "result": {
    "node_id": "synology-main",
    "path": "/mnt/documents/taxes/2024.pdf",
    "display_path": "Synology:Documents/taxes/2024.pdf",
    "match_type": "content",
    "snippet": "...property tax statement...",
    "line": 42
  }
}
```

Summary event:

```json
{
  "type": "summary",
  "searched_files": 1200,
  "skipped_files": 80,
  "errors": 3,
  "timed_out": false
}
```

### Scan Control

```http
POST /v1/scan
GET /v1/scan/current
GET /v1/scan/history
```

### Config

V1 can start read-only through API:

```http
GET /v1/config
```

The config response must redact `auth_token`.

Add writes later:

```http
PUT /v1/config
```

## 12. Search Semantics

### Metadata Search

Start simple:

- Case-insensitive filename and path substring matching.
- Split query into terms.
- Match all terms by default.
- Rank filename matches above path-only matches.
- Rank exact filename matches highest.
- Rank recent files slightly higher.

Example ranking:

1. Exact filename match.
2. Filename contains all terms.
3. Relative path contains all terms.
4. Extension/type filter match.
5. Recently modified tie-breaker.

Cross-node ranking: nodes return raw match signals (`match_type`, filename-vs-path hit, modified time) rather than an opaque score. The GUI ranks the combined result set globally, so results from different nodes are comparable and each node does not need to invent a compatible scoring scheme.

### Content Search

Start simple:

- Case-insensitive substring search.
- For text files, stream line by line.
- For PDFs, extract text then search extracted text.
- Return snippets.
- Stop at configured result/time limits.

Avoid regex search in v1 unless needed. Regex can come later with careful timeouts.

## 13. Resource Management

Defaults should be conservative:

- Scan worker count: 2.
- Content search worker count: 1 or 2.
- No full-file hashing by default.
- No PDF background indexing by default.
- Scan interval: 6-24 hours.
- Content search timeout: 60 seconds.
- Max live content file size: 25 MB initially.
- Tombstone retention: 30 days.
- Exclude heavy directories by default, such as `.git`, `node_modules`, caches, system folders, app bundles, and backup directories.

Use context cancellation throughout:

- API request cancellation should stop searches.
- GUI should be able to cancel deep searches.
- Node shutdown should stop scans gracefully.

## 14. Docker And Synology Plan

### Container Layout

```text
/app/super-speedy-search-node
/config/config.yaml
/data/index.db
/mnt/<mounted-share>
```

### Example Compose

```yaml
services:
  search-node:
    image: super-speedy-search-node:latest
    container_name: super-speedy-search-node
    restart: unless-stopped
    network_mode: host
    volumes:
      - ./config:/config
      - ./data:/data
      - /volume1/documents:/mnt/documents:ro
    environment:
      - SSS_CONFIG=/config/config.yaml
```

Use `network_mode: host` if mDNS discovery is important. If host networking is not desirable, support manual node URL configuration.

### Synology Notes

- Prefer read-only mounts for search roots.
- Persist `/data` so the index survives container upgrades.
- Persist `/config` for node settings.
- Include PDF tooling only if PDF live search is enabled.
- Keep CPU and worker defaults low.

## 15. Implementation Milestones

### Milestone 0: Repo Bootstrap

Deliverables:

- Go module.
- Basic folder structure.
- Node command skeleton.
- Config loader.
- Logging.
- Build/test scripts.

Suggested structure:

```text
cmd/sss-node/
internal/config/
internal/db/
internal/scanner/
internal/search/
internal/api/
internal/discovery/
internal/content/
internal/openuri/
```

### Milestone 1: Metadata Indexer

Deliverables:

- SQLite schema migrations.
- Configured scan roots.
- Recursive scanner.
- Exclusion rules.
- Insert/update file metadata.
- Mark missing files deleted after reconciliation.
- Filesystem watcher (fsnotify) with per-root watch budget and fallback to periodic scans.
- CLI command to run a scan.
- Basic tests for exclusion and path mapping.

Success criteria:

- Can scan a test directory.
- Can detect added, changed, and deleted files.
- Does not follow excluded paths.
- Database survives restart.

### Milestone 2: Node HTTP API

Deliverables:

- `GET /v1/status`.
- `GET /v1/roots`.
- `POST /v1/search/metadata`.
- `POST /v1/scan`.
- Token auth middleware, optional by config.
- Request logging.

Success criteria:

- Can query a node with curl.
- Search returns useful metadata results.
- Scan can be triggered remotely.

### Milestone 3: LAN Discovery

Deliverables:

- mDNS advertisement.
- Node service metadata.
- Config flag to enable/disable advertisement.
- Simple discovery client command for testing.
- Minimal CLI search client (discover + fan-out metadata search) so the system is usable end-to-end months before the GUI exists.

Success criteria:

- GUI or CLI can discover nodes on the same network.
- Manual node URLs still work if discovery fails.

### Milestone 4: Live Content Search

Deliverables:

- Content search API.
- Text file extraction.
- Markdown/plain text/code search.
- NDJSON or SSE streaming.
- Cancellation.
- Limits and timeouts.
- Result snippets.
- Summary event.

Success criteria:

- Deep search returns incremental results.
- Large searches can be cancelled.
- Node remains responsive during content search.

### Milestone 5: PDF Search

Deliverables:

- PDF extraction adapter.
- Config option to enable PDF search.
- Docker image variant or dependency installation path.
- Tests with sample PDFs.

Success criteria:

- PDF search works when enabled.
- PDF failures are reported without breaking search.
- NAS defaults remain conservative.

### Milestone 6: GUI Prototype

Deliverables:

- Node discovery/manual node list.
- Search screen.
- Combined metadata results.
- Deep search progress/results.
- Result actions: copy path, open URI where supported.
- Node status screen.

Success criteria:

- A user can search across multiple nodes.
- Results are clear about which node/root they came from.
- Long deep searches show progress and can be cancelled.

### Milestone 7: Packaging

Deliverables:

- macOS node binary.
- Linux node binary.
- Docker image.
- Example Synology config/compose.
- Basic installation docs.

Success criteria:

- Node can run on a Mac.
- Node can run in Docker on Synology.
- GUI can find or manually connect to both.

### Milestone 8: Optional FTS5 Indexing

Deliverables:

- FTS5 schema.
- Lightweight content indexer.
- Configurable filetype allowlist.
- Size limits.
- Indexed content search endpoint or mode.
- Rebuild/vacuum controls.

Success criteria:

- Text/markdown searches are much faster for indexed roots.
- Index size is observable.
- Feature can be disabled per node/root.

## 16. Testing Strategy

### Unit Tests

- Config parsing.
- Exclude matching.
- Path to display/open URI mapping.
- Query term parsing.
- Metadata ranking.
- Content snippet generation.

### Integration Tests

- Temporary directory scan.
- Add/update/delete detection.
- SQLite migration.
- Metadata search API.
- Content search streaming.
- Cancellation behavior.

### Manual Tests

- macOS local folder.
- Synology Docker mounted folder.
- mDNS discovery.
- Manual node URL.
- SMB open URI.
- PDF extraction if enabled.

## 17. Risks And Mitigations

### Risk: Content Search Is Too Slow

Mitigation:

- Keep metadata search fast and separate.
- Stream content results.
- Add filetype and size limits.
- Add FTS5 later for selected text formats.

### Risk: SQLite Gets Too Large

Mitigation:

- Store metadata only by default.
- Do not store raw file contents.
- Make FTS5 opt-in.
- Track database size in status.
- Add cleanup/vacuum commands.

### Risk: NAS CPU Usage Is Too High

Mitigation:

- Conservative workers.
- Live search only when requested.
- No background PDF indexing by default.
- Read-only mounts.
- Scan intervals measured in hours, not seconds.

### Risk: File Watchers Miss Events

Mitigation:

- Treat watchers as acceleration only.
- Periodic reconciliation scan remains authoritative.

### Risk: Paths Are Not Openable From GUI

Mitigation:

- Return structured location data.
- Support `open_uri` and `parent_open_uri`.
- Show actions only when supported.
- Keep copy path always available.

### Risk: V1 Home API Becomes Unsafe Later

Mitigation:

- Add optional token auth in v1.
- Keep API scoped to configured roots.
- Do not expose arbitrary file read.
- Design manual node URLs and node identity early.

## 18. Initial Build Recommendation

Start with the node, not the GUI.

The first useful vertical slice should be:

1. Go node loads config.
2. Node scans one configured root.
3. Node writes metadata to SQLite.
4. Node exposes `GET /v1/status`.
5. Node exposes `POST /v1/search/metadata`.
6. Search can be tested with curl.

After that, add discovery and live content search. Then build the GUI once the node API has proven shape.

This avoids building a polished UI around unstable backend behavior.

## 19. Decisions

Resolved:

- SQLite driver: `modernc.org/sqlite` (pure Go; includes FTS5).
- Content search streaming: NDJSON over POST.
- Filesystem watching: in v1, on by default with a per-root watch budget; the reconciliation scan stays authoritative. Note that fsnotify on macOS uses kqueue, which consumes a file descriptor per watched item — budgets matter most there. FSEvents support can come later if needed.
- Metadata filename search: plain SQL `LIKE` scan for v1. Confirmed scale is under ~2M files per device (computers 1–2 TB; NASes ~50 TB but mostly large media files, so file counts stay moderate).
- Auth: shared token, generated automatically on first run; pairing later.
- PDF: external `pdftotext` in Docker, optional on desktop.

Still open:

- GUI stack: Tauri, Electron, or local web app. The GUI is an ephemeral orchestrator — it may run on zero, one, or several devices at once, holds no authoritative state, and nodes must not assume a single coordinator. A local web app served by any node is worth considering for that reason.
- Exact auth/pairing UX for V2.
- Windows nodes: none on the network today, but likely later. Keep path handling behind `path/filepath`, and design `open_uri` so UNC paths (`\\server\share`) can be represented alongside `smb://`.

