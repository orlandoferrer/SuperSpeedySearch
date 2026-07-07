# Super Speedy Search

Low-resource file search across multiple computers and Synology NAS devices on
a home network. Each machine runs a **search node** that indexes file metadata
into local SQLite and answers searches over a small HTTP API; nodes are
discovered over mDNS. See [PLAN.md](PLAN.md) for the full architecture and
the **[User Manual](docs/USER_MANUAL.md)** for detailed setup on macOS,
Linux, and Synology, the full CLI/API reference, and troubleshooting.

Status: node implemented (metadata index, filesystem watching, live content
search, mDNS discovery, CLI client). GUI not started yet.

## Quick start (macOS/Linux)

```sh
go build -o sss-node ./cmd/sss-node

cp config.example.yaml config.yaml   # edit roots/paths
./sss-node run -config config.yaml
```

On first run the node generates an auth token and logs it (also stored in an
`auth_token` file next to the database). Use it with `Authorization: Bearer`.

```sh
TOKEN=<token from the startup log>

curl -s -H "Authorization: Bearer $TOKEN" localhost:37373/v1/status | jq

curl -s -H "Authorization: Bearer $TOKEN" localhost:37373/v1/search/metadata \
  -d '{"query": "tax 2024", "limit": 20}' | jq

# live content search streams NDJSON
curl -sN -H "Authorization: Bearer $TOKEN" localhost:37373/v1/search/content \
  -d '{"query": "property tax", "extensions": [".txt", ".md"]}'
```

## CLI

```sh
sss-node run                      # run the daemon (default command)
sss-node scan                     # one-shot scan, then exit
sss-node discover                 # list nodes advertised on the LAN
SSS_TOKEN=... sss-node search tax # fan-out search across discovered nodes
sss-node search -node http://synology.local:37373 -token T -ext .pdf tax 2024
```

## Synology / Docker

```sh
docker build -t super-speedy-search-node .
cd deploy/synology && docker compose up -d
```

Edit `deploy/synology/config/config.yaml` and the compose volume mounts to
match your shares. Host networking is used so mDNS discovery works; without
it, add the node to clients manually by URL.

## API

| Endpoint | Description |
| --- | --- |
| `GET /v1/status` | Node identity, capabilities, index size |
| `GET /v1/roots` | Configured scan roots |
| `POST /v1/search/metadata` | Filename/path search (JSON) |
| `POST /v1/search/content` | Live content search (streams NDJSON) |
| `POST /v1/scan` | Trigger a scan (optional `{"root_id": ...}`) |
| `GET /v1/scan/current` | Scan progress |
| `GET /v1/scan/history` | Recent scan runs |
| `GET /v1/config` | Effective config (token redacted) |

All endpoints require `Authorization: Bearer <token>` unless
`auth_required: false`.

## Development

```sh
go test ./...
go vet ./...
```

Layout: `cmd/sss-node` is the entrypoint; `internal/` packages are `config`,
`db` (SQLite via modernc.org/sqlite), `scanner` (reconciliation scans),
`watcher` (fsnotify acceleration), `search` (metadata ranking), `content`
(live content search + pdftotext adapter), `api`, and `discovery` (mDNS).
