[English](./README.md) · [中文](./README.zh-CN.md)

# MCP Arc

A lightweight MCP proxy between an MCP client and server. One command adds audit
logging, parameter masking, call replay, and per-client rate limiting — without
touching the protocol or locking you to a vendor.

Source: https://github.com/wangzeyud/MCP-Arc

## What it does

Exactly four things:

| | |
|---|---|
| **Parameter masking** | Redacts PII / secrets in `tools/call` args (and results) before persisting. Regex patterns + sensitive field names, configured in YAML or edited at runtime. |
| **Call audit** | Who (`client_id`), when, which tool, what params/result, duration, success/error — to SQLite (default) or PostgreSQL. |
| **Replay** | Re-issue a recorded `tools/call` to the upstream verbatim, for debugging or compliance re-execution. |
| **Rate limiting** | Per-`client_id` token-bucket QPS + daily quota. |

The embedded Vue3 console is the **observability UI** for the four capabilities
above — not a fifth feature. Rules are hot-reloaded; audit records exportable as JSON / CSV.

## Design

- Works at the **transport layer** (stdio / Streamable HTTP / SSE-legacy), not inside the protocol.
- Only MCP knowledge used: the `tools/call` method and the `params.{name,arguments}` shape.
- Requests are correlated by rewriting the JSON-RPC `id` and restoring it on the
  way back; responses are **matched**, not parsed. Untouched messages forward byte-for-byte.

## Install

Ships as a single binary.

### A. Docker
```bash
docker compose up --build                       # SQLite backend, console on :8080
docker compose --profile postgres up --build    # PostgreSQL backend
```
Proxy reachable at `http://localhost:8081/sse`.

### B. Prebuilt binary (planned for v1.0)
One-command install (`brew` / `go install`) and ready-made binaries are on the
roadmap for **v1.0**. Until then use Docker or build from source.

### C. Build from source
Requires **Go 1.27**. SQLite driver is pure-Go (`modernc.org/sqlite`) — **no CGO needed**.
```bash
cd web && npm install && npm run build && cd ..   # build the embedded console (lives in web/)
go build -o mcp-arc ./cmd/mcp-arc                 # macOS / Linux
# go build -o mcp-arc.exe ./cmd/mcp-arc           # Windows
```
> ⚠️ Run `npm install` from `web/`, not the repo root (no `package.json` there).

## Quick start
```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"message":"my email is a@b.com and pwd secret123"}}}' \
  | ./mcp-arc --upstream "node examples/echo-server/server.js"
```
Email and `pwd` are stored **masked**; the real request reaches the upstream untouched. View logs:
```bash
curl -H "Authorization: Bearer change-me" localhost:8080/api/logs
curl -H "Authorization: Bearer change-me" localhost:8080/api/stats
```

## Transports

| `transport.client` | `transport.upstream` | scenario |
|---|---|---|
| `stdio` (default) | `stdio` (default) | local transparent proxy |
| `sse` | `stdio` | **gateway mode** — remote clients over HTTP, Arc fronts a local stdio server (SSE is legacy; Streamable HTTP preferred) |
| `sse` | `sse` | fully remote |
| `stdio` | `sse` | local client → remote SSE server |
| `streamable-http` | `streamable-http` | remote client & server over a single POST endpoint (MCP 2026-07-28) |
| `streamable-http` | `stdio` | remote clients over Streamable HTTP → local stdio server |

```yaml
server:
  upstream: ["node", "examples/mcp-server-demo/server.js"]
transport:
  client: sse
  listen: ":8081"
  upstream: stdio
```
Clients connect to `http://host:8081/sse`.

CLI flags: `--config`, `--upstream`, `--client-transport` (`stdio`|`sse`|`streamable-http`), `--listen`, `--upstream-transport` (`stdio`|`sse`|`streamable-http`), `--upstream-url`.

## Wiring into a real client

Point the **client** at `mcp-arc` instead of the server directly.

### Local (stdio)
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
Works in Claude Desktop, Cursor, Cline, Windsurf, Zed, etc.
- **Claude Desktop** — `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) / `%APPDATA%\Claude\claude_desktop_config.json` (Windows)
- **Cursor** — `.cursor/mcp.json`
- **Cherry Studio / Trae / 5ire / Lingma** — Settings → MCP → add local server

Restart the client afterwards.

### Remote (SSE gateway)
```yaml
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
    "my-server-via-mcp-arc": { "type": "sse", "url": "http://host:8081/sse" }
  }
}
```

## Configuration

See `config.yaml`.

| key | meaning |
|---|---|
| `server.client_id` | label stored on every audit record (or `MCP_ARC_CLIENT_ID`) |
| `server.upstream` | upstream command + args in config, instead of `--upstream` |
| `transport` | `client` / `upstream` (`stdio`\|`sse`), `listen`, `upstream_url` |
| `audit` | `enabled`, `driver: sqlite` (default) or `postgres`, `dsn` |
| `masking` | `enabled` + `rules` (regex `patterns` and/or `fields`) + optional `presets` |
| `llm` | optional LLM-assisted masking: `enabled`, `endpoint`, `api_key`, `model`, `timeout_ms`, `max_bytes`, `cache_ttl_seconds`, `apply_to_result` |
| `rate_limit` | `enabled`, `qps`, `daily_quota` (per `client_id`) |
| `admin` | `enabled`, `port`, `token` (console Bearer token) |

## Masking rules

Rules live in the database (`mask_rules`); on first run, `config.yaml` rules are
**seeded** (`source: config`), then owned by the console (create / edit / enable /
disable / delete at runtime). Every write **hot-reloads** the masker — next call
uses the new rules without restart.

Regexes are compile-checked on save, so a bad pattern is rejected instead of silently disabling masking.

### Preset templates
Enabled by listing names under `masking.presets`:

| preset | matches | mask |
|---|---|---|
| `phone_cn` | mainland China mobile | `[PHONE]` |
| `ip` | IPv4 | `[IP]` |
| `bank_card_cn` | UnionPay (start 62) | `[BANKCARD]` |
| `passport` | passport numbers | `[PASSPORT]` |
| `mac` | MAC addresses | `[MAC]` |

Names/addresses (free-text PII) are deliberately excluded — use the LLM second pass.

```bash
curl -H "Authorization: Bearer change-me" localhost:8080/api/rules
curl -X POST -H "Authorization: Bearer change-me" -H "Content-Type: application/json" \
  -d '{"name":"phone_cn","patterns":["\\b1[3-9]\\d{9}\\b"],"mask_char":"[PHONE]"}' \
  localhost:8080/api/rules
```

## LLM-assisted masking

Optional second pass for free-text PII static rules miss. The model sees only the
**already-masked** payload and returns paths still worth redacting; redacted values
never leave the process, hallucinated paths are ignored. **Fails open**: on error,
timeout, or `max_bytes` exceeded, the static result stands and the call proceeds.

```yaml
llm:
  enabled: true
  endpoint: "https://api.openai.com/v1"   # any OpenAI-compatible endpoint
  model: "gpt-4o-mini"
  timeout_ms: 3000
  max_bytes: 8192
  apply_to_result: false
```

## Export
```bash
curl -H "Authorization: Bearer change-me" "localhost:8080/api/export?format=csv"
curl -H "Authorization: Bearer change-me" "localhost:8080/api/export?format=json&limit=5000"
curl -H "Authorization: Bearer change-me" "localhost:8080/api/export?format=json&raw=1"  # includes unmasked values
```
Filterable by `client_id` / `tool` / `limit` (max 10000). Exports contain **masked** params/results; unmasked `raw_params` / `raw_result` only with `raw=1`.

## Web console
Vue3 console compiled into the binary (`//go:embed`), served by the admin HTTP server — no separate frontend process.
```bash
./mcp-arc --config config.dev.yaml
# open http://localhost:8080  →  Dashboard / Call Logs / Rules
```

## Replay
Every `tools/call` is recorded with its original (unmasked) request params and raw upstream response.
```bash
curl -H "Authorization: Bearer change-me" localhost:8080/api/logs        # find a call id
curl -X POST -H "Authorization: Bearer change-me" -H "Content-Type: application/json" \
     -d '{"call_id": 1}' localhost:8080/api/replay                       # replay
```
Rides the same id-rewrite / response-correlation machinery as a live call (stdio + SSE upstreams).

## Changelog

### v0.6 — four-pillar polish (replay + audit retention)
- **Batch replay** — `POST /api/replay/batch` re-issues many recorded `tools/call`s at once, either by explicit `call_ids` or by a `filter` (tool / client_id / since / until / limit). Replays run sequentially reusing the same id-rewrite / correlation path; a single failure is recorded and the rest continue.
- **Replay diff** — with `diff: true`, each replayed response is compared (exact JSON equality) against the originally recorded `raw_result`, so operators can verify the upstream still behaves. Available on both single and batch replay.
- **Audit retention** — `audit.retention` (`max_age_days` / `max_rows`, default off) bounds how large the audit log may grow. Records are trimmed at startup and on a periodic background worker (SQLite / PostgreSQL / memory), fail-open so a prune error never blocks the request path.
- **Console** — the Call Logs page supports multi-select replay with a diff toggle and a per-call results panel.

### v0.5 — spec alignment (MCP 2026-07-28)
- **Streamable HTTP transport** — single POST endpoint for both client and upstream (`transport.client` / `transport.upstream: streamable-http`), matching the 2026-07-28 spec. The older HTTP+SSE adapter is retained as `sse` and marked **legacy**; Streamable HTTP is recommended for new deployments.
- **Method-agnostic passthrough** — forwards all JSON-RPC methods (tools/*, resources/*, prompts/*, sampling, elicitation, roots, subscriptions, MRTR, progress, cancellation, …) unchanged; only `tools/call` (and sampling/elicitation) params are inspected for masking/audit.
- **Notification scoping & cancellation propagation (T4–T5)** — stream-scoped notifications route to the owning request instead of being broadcast to every client; a client disconnect now actively cancels the in-flight upstream request. Verified by the proxy-level E2E test `TestE2EStreamableHTTPProxyCancelOnClientDisconnect`.
- **`InputRequiredResult` audit** — audited without nested masking.
- Transport unit tests + soak variant; SSE marked legacy.

### v0.4 — stability / soak testing
- **Soak stability test** — `TestProxySoakStability` drives 20k proxy round-trips, asserting zero pending-request leaks, audit-count consistency, and bounded goroutine growth.
- **Race verification** — full `go test -race ./...` passes with no data races across proxy / mask / audit / admin / llm.
- Internal quality only; no new product capabilities.

### v0.3 — stability & production readiness
- **Preset masking templates** — `phone_cn` / `ip` / `bank_card_cn` / `passport` / `mac`; reference by name under `masking.presets`.
- **Async, timeout-bounded audit writes** — buffered queue + background worker with per-insert timeout; a slow DB never stalls the request path (fail-open).
- **Upstream resilience** — auto reconnect/restart on upstream crash; failed audit write returns explicit `-32002` instead of a silent hang.
- **Replay UI** — inspect and replay past `tools/call` from the console.
- **Graceful shutdown** — on `SIGINT`/`SIGTERM`, drains the buffered audit queue (bounded by `audit.shutdown_flush_timeout_ms`, default 5s) before closing the store; in-flight requests fail with a clear `upstream disconnected` error.

## Roadmap

- **v0.1** ✅ stdio / SSE transports, audit (SQLite + PostgreSQL), masking, rate limit, console, replay.
- **v0.2** ✅ LLM-assisted masking, rule CRUD in the console, JSON / CSV export.
- **v0.3** ✅ stability and production readiness: preset templates, async/timeout-bounded audit writes, upstream reconnect, replay UI, graceful shutdown.
- **v0.4** ✅ — stability / soak testing + race verification (internal quality; no new features).
- **v0.5** ✅ — spec alignment (MCP 2026-07-28): Streamable HTTP transport, method-agnostic passthrough, notification scoping & cancellation propagation (client disconnect cancels in-flight upstream), `InputRequiredResult` audit; SSE marked legacy.
- **v0.6** ✅ — four-pillar polish: batch replay + replay diff (`/api/replay/batch`, exact JSON comparison vs recorded result), audit retention (`audit.retention` max_age_days/max_rows with startup trim + periodic worker), console multi-select replay UI.

## License
MIT — see [LICENSE](./LICENSE).
