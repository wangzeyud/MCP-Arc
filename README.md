[English](./README.md) · [中文](./README.zh-CN.md)

# MCP Arc

A **lightweight MCP proxy** that sits between an MCP client and an MCP server.
One command adds audit logging, parameter masking and call replay to any MCP
call — without touching the protocol, and without locking you to a vendor.

```
MCP client  ──────▶  MCP Arc  ──────▶  MCP server
(Claude Desktop /     │  governance    (node / python / …)
 Cherry Studio /      │
 Cursor …)            └─ audit ─▶ SQLite / PostgreSQL
```

**Source:** https://github.com/dodoyu-sama/MCP-Arc

## What it does

| | |
|---|---|
| **Parameter masking** | Redacts PII / secrets in `tools/call` arguments (and results) before anything is persisted. Regex patterns + sensitive field names, configured in YAML or edited at runtime. |
| **Call audit** | Who (`client_id`), when, which tool, what params, what result, how long, success or error — persisted to SQLite (default) or PostgreSQL. |
| **Replay** | Re-issue a recorded `tools/call` to the upstream verbatim, for debugging flaky tools and for compliance re-execution. |

Plus the small stuff that makes it usable: per-`client_id` rate limiting (token
bucket QPS + daily quota), runtime-editable masking rules (no restart), JSON /
CSV export, and an embedded Vue3 console served by one binary.

## Design

- It works at the **transport layer** (stdio / SSE), not inside the protocol.
- The only MCP knowledge it relies on is the `tools/call` method name and the
  `params.{name,arguments}` shape.
- Requests are correlated by rewriting the JSON-RPC `id` and restoring it on the
  way back; responses are **matched**, not parsed. Everything else is
  protocol-agnostic plumbing, so spec changes don't force a rewrite.
- Messages it does not need to touch are forwarded byte-for-byte.

## Install

MCP Arc ships as a single binary. Pick the path that fits you:

### A. Docker (easiest — no Go or Node needed)

Only Docker is required; the container builds the binary **and** the web console
for you, so your machine stays clean.

```bash
docker compose up --build                       # SQLite backend, console on :8080
docker compose --profile postgres up --build    # PostgreSQL backend
```

The proxy is then reachable at `http://localhost:8081/sse` (gateway mode). Skip
ahead to [Wiring into a real client](#wiring-into-a-real-client).

### B. Prebuilt binary (planned for v1.0)

One-command install (script / `brew` / `go install`) and ready-made binaries for
Windows / macOS / Linux are on the roadmap for **v1.0**. Until then, use Docker
(Option A) or build from source (Option C).

### C. Build from source (developers)

Requires **Go 1.22+** and a C compiler (CGO) for the SQLite driver.

```bash
# 1) build the embedded web console — its files live in web/, NOT the repo root
cd web && npm install && npm run build && cd ..

# 2) build the binary
go build -o mcp-arc ./cmd/mcp-arc          # macOS / Linux
# go build -o mcp-arc.exe ./cmd/mcp-arc   # Windows
```

> ⚠️ If you run `npm install` from the repo root you'll see
> `ENOENT ... package.json`: the frontend project is in the **`web/`** folder.
> (The `Makefile`'s `make build` runs both steps, but `make` isn't bundled with
> Windows — the commands above work on every OS.)

## Quick start

With the `mcp-arc` binary (or the Docker container) ready, pipe two JSON-RPC
messages through the proxy to watch masking + audit work:

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"message":"my email is a@b.com and pwd secret123"}}}' \
  | ./mcp-arc --upstream "node examples/echo-server/server.js"
```

The upstream responses print out and `mcp-arc.db` is created. In the audit log the
email and the `pwd` field are stored **masked**, while the real request still
reaches the upstream untouched:

```bash
curl -H "Authorization: Bearer change-me" localhost:8080/api/logs
curl -H "Authorization: Bearer change-me" localhost:8080/api/stats
```

## Transports

Client side and upstream side are configured independently; both support `stdio`
and `sse`.

| `transport.client` | `transport.upstream` | scenario |
|---|---|---|
| `stdio` (default) | `stdio` (default) | local transparent proxy — the client spawns MCP Arc directly |
| `sse` | `stdio` | **gateway mode** — remote / multiple clients over HTTP, Arc fronts a local stdio server |
| `sse` | `sse` | fully remote — Arc governs in the middle, HTTP on both sides |
| `stdio` | `sse` | forward a local client's calls to a remote SSE server |

```yaml
server:
  upstream: ["node", "examples/mcp-server-demo/server.js"]
transport:
  client: sse
  listen: ":8081"
  upstream: stdio
```

Clients then connect to `http://host:8081/sse` (GET for the event stream, POST
`/messages?sessionId=...` to send).

### CLI flags

`--config`, `--upstream`, `--client-transport` (`stdio`|`sse`), `--listen`,
`--upstream-transport` (`stdio`|`sse`), `--upstream-url`.

```bash
./mcp-arc --client-transport sse --listen :8081 \
          --upstream-transport sse --upstream-url https://host/mcp/sse
```

## Wiring into a real client

You don't change your MCP server at all — you just tell your **client** to launch
MCP Arc and talk to it instead of the server directly.

### Local (stdio, default)

Point the client at the `mcp-arc` binary. This `mcpServers` entry works in
Claude Desktop, Cursor, Cline, Windsurf, Zed, and most other MCP clients:

```json
{
  "mcpServers": {
    "my-server-via-mcp-arc": {
      "command": "/path/to/mcp-arc",
      "args": ["--upstream", "node", "/path/to/your-server.js"]
    }
  }
}
```

Where to paste it:
- **Claude Desktop** — `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows)
- **Cursor** — `.cursor/mcp.json` in your project (or `~/.cursor/mcp.json`)
- **Cherry Studio / Trae / 5ire / Lingma** — Settings → MCP → "add local server" → fill the command + args above

Restart the client afterwards; it spawns `mcp-arc`, which in turn spawns your real
server. That's the whole setup.

### Remote (SSE gateway)

If you'd rather run Arc as a shared gateway (e.g. the Docker container, or on a
server), run it in gateway mode and point the client at its SSE URL instead of a
command:

```yaml
# config.yaml
transport:
  client: sse
  listen: ":8081"
  upstream: stdio
server:
  upstream: ["node", "/path/to/your-server.js"]
```

```json
{
  "mcpServers": {
    "my-server-via-mcp-arc": {
      "type": "sse",
      "url": "http://host:8081/sse"
    }
  }
}
```

## Configuration

See `config.yaml`. Key sections:

| key | meaning |
|---|---|
| `server.client_id` | label stored on every audit record (or `MCP_ARC_CLIENT_ID`) |
| `server.upstream` | upstream command + args written in config, instead of `--upstream` |
| `transport` | `client` / `upstream` (`stdio`\|`sse`), `listen` (SSE bind addr), `upstream_url` |
| `audit` | `enabled`, `driver: sqlite` (default) or `postgres`, `dsn` |
| `masking` | `enabled` + `rules` (regex `patterns` and/or `fields`) |
| `llm` | optional LLM-assisted masking: `enabled`, `endpoint`, `api_key`, `model`, `timeout_ms`, `max_bytes`, `cache_ttl_seconds`, `apply_to_result` |
| `rate_limit` | `enabled`, `qps`, `daily_quota` (per `client_id`) |
| `admin` | `enabled`, `port`, `token` (Bearer token for the console) |

## Masking rules

Rules live in the database (`mask_rules` table), not just in YAML: on first run
the rules from `config.yaml` are **seeded** in (`source: config`), and from then
on the console owns them — create, edit, enable/disable and delete at runtime.
Every write **hot-reloads** the masker, so the next tool call uses the new rules
without a restart.

A rule needs a `name` plus at least one `pattern` or `field`; regexes are
compile-checked on save, so a bad pattern is rejected instead of silently
disabling masking.

```bash
curl -H "Authorization: Bearer change-me" localhost:8080/api/rules

curl -X POST -H "Authorization: Bearer change-me" -H "Content-Type: application/json" \
  -d '{"name":"phone_cn","patterns":["\\b1[3-9]\\d{9}\\b"],"mask_char":"[PHONE]"}' \
  localhost:8080/api/rules
```

## LLM-assisted masking

An optional second pass for free-text PII that static rules miss. The model only
sees the **already-masked** payload and returns the paths still worth redacting;
values already redacted never leave the process, and hallucinated paths are
ignored. It **fails open**: on error, timeout, or a payload over `max_bytes`, the
static result stands and the call proceeds.

```yaml
llm:
  enabled: true
  endpoint: "https://api.openai.com/v1"   # any OpenAI-compatible endpoint
  # api_key: prefer MCP_ARC_LLM_API_KEY
  model: "gpt-4o-mini"
  timeout_ms: 3000
  max_bytes: 8192
  apply_to_result: false                  # also scan upstream results?
```

## Export

```bash
curl -H "Authorization: Bearer change-me" "localhost:8080/api/export?format=csv"
curl -H "Authorization: Bearer change-me" "localhost:8080/api/export?format=json&limit=5000"
curl -H "Authorization: Bearer change-me" "localhost:8080/api/export?format=json&raw=1"  # includes unmasked values
```

Filterable by `client_id` / `tool` / `limit` (max 10000). Exports contain the
**masked** params and results; unmasked `raw_params` / `raw_result` are only
included with an explicit `raw=1`. The console's Call Logs page wires Export
JSON / CSV to the same endpoint.

## Web console

The Vue3 console is compiled into the binary (`//go:embed`) and served by the
admin HTTP server — no separate frontend process.

```bash
./mcp-arc --config config.dev.yaml
# open http://localhost:8080  →  Dashboard / Call Logs / Rules
```

## Replay

Every `tools/call` is recorded with its original (unmasked) request params and
the raw upstream response:

```bash
# 1) find a call id
curl -H "Authorization: Bearer change-me" localhost:8080/api/logs

# 2) replay it (returns the upstream's raw JSON-RPC response)
curl -X POST -H "Authorization: Bearer change-me" -H "Content-Type: application/json" \
     -d '{"call_id": 1}' localhost:8080/api/replay
```

Replay rides the same id-rewrite / response-correlation machinery as a live
call, so it works for stdio and SSE upstreams alike.

## Roadmap

- **v0.1** ✅ stdio / SSE transports, audit (SQLite + PostgreSQL), masking, rate limit, console, replay.
- **v0.2** ✅ LLM-assisted masking, rule CRUD in the console, JSON / CSV export.
- **v0.3** (in progress) — stability and production readiness: config hot reload, graceful upstream exit, audit write degradation, alert webhooks, richer stats, more PII presets, 7×24 soak test.
- **v1.0** — production ready: complete docs, one-command install (script / brew / `go install` / Docker), TLS for SSE.
- **v2.0+** — multi-tenancy, policy engine, clustered deployment. No timeline.

## License

MIT — see [LICENSE](./LICENSE).
