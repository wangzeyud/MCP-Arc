# Contributing to MCP Arc

Thanks for your interest in improving MCP Arc! This guide covers local setup,
the build/test workflow, and how the repo is laid out.

## Prerequisites

- **Go 1.27+** with a C compiler (CGO) — required by the `mattn/go-sqlite3` driver.
  - macOS: `brew install go && xcode-select --install`
  - Linux: install `gcc`/`build-essential`.
  - Windows: supported — build the GUI binary with `go build -ldflags "-H=windowsgui" ./cmd/mcp-arc`.
- **Node 20+** — only needed to rebuild the embedded Vue3 console (`web/`).

## Build & test

```bash
make build     # builds web/dist, then compiles ./mcp-arc
make run       # build + run the dev gateway (config.dev.yaml, SSE on :8081)
make test      # go test ./...
make clean     # remove the binary and web/dist
```

Tests cover the parts that are easy to get subtly wrong and hard to see in a
live session: masking path resolution (`internal/mask`), LLM response parsing,
caching and the fail-open breaker (`internal/llm`), rule seeding / reload /
validation (`internal/proxy`), and rule-list encoding (`internal/audit`).
Transports are exercised manually — they are the thin, protocol-specific edge.

Run the proxy without rebuilding the frontend (console is embedded):

```bash
./mcp-arc --config config.dev.yaml
# open http://localhost:8080
```

Live frontend development (Vite HMR on :5173, proxies `/api` → `:8080`):

```bash
./mcp-arc --config config.dev.yaml &   # proxy + admin API
cd web && npm run dev
```

## Docker

```bash
docker compose up --build                              # SQLite (default), :8080
docker compose --profile postgres up --build           # PostgreSQL, proxy :8081
```

## Code layout

```
cmd/mcp-arc/       entrypoint (flag parsing, wiring)
internal/transport/  stdio / SSE transports — the ONLY place that knows wire formats
internal/proxy/      transport-agnostic relay: session routing, id rewrite, correlation
                     protocol.go = the only MCP-specific knowledge in the governance layer
                     rules.go    = rule seeding, hot reload, CRUD
internal/audit/      store (SQLite + PostgreSQL): calls + mask_rules tables, migrate()
internal/mask/       masking engine: static rules, hot reload, Detector hook
internal/llm/        optional second-pass detector (OpenAI-compatible), cache + fail-open
internal/ratelimit/  per-client_id token bucket + daily quota
internal/admin/      REST API + embedded web console (web/embed.go)
internal/config/     YAML config + env overrides + defaults
web/                 Vue3 dashboard (built into the binary via //go:embed)
```

Masking rules and audit records share one `audit.Store`, so both SQLite and
PostgreSQL backends implement call *and* rule persistence over a single
connection. New backends must satisfy the full `Store` interface (there are
compile-time assertions in both `sqlite.go` and `postgres.go`).

## Design constraint: stay above the protocol

MCP Arc's core governance logic must remain **decoupled from MCP protocol
internals**. It is built on top of the transport layer (stdio / SSE) and only
knows the `tools/call` method name and its params shape.

When adding features:

- Put wire-format knowledge in `internal/transport/`, never in `proxy`/`audit`/`mask`.
- Match and correlate responses; don't parse protocol payloads to make decisions.
- If a feature seems to require understanding MCP semantics, that's a signal to
  push it down to transport or express it as a config rule instead.

## Configuration

All behaviour is driven by a YAML config (see `config.yaml` for the full schema
and defaults). `config.dev.yaml` / `config.postgres.yaml` / `config.sse-upstream.yaml`
are ready-to-run examples. Local overrides can live in `config.local.yaml` (gitignored).

Audit backend: set `audit.driver: sqlite` (default, `dsn` = file path) or
`audit.driver: postgres` (`dsn` = Postgres connection URL).

## Guidelines

- Run `gofmt -w` (or `go fmt ./...`) and `go vet ./...` before opening a PR.
- Keep code modern: prefer current Go idioms (see [JetBrains/go-modern-guidelines](https://github.com/JetBrains/go-modern-guidelines)). The `modernize` analyzer can suggest updates — `go run golang.org/x/tools/go/analysis/passes/modernize/cmd/modernize@latest ./...`. Disable `omitzero` (`-omitzero=false`) so it does not rewrite JSON struct tags (audit records are serialized as JSON).
- Keep the single-binary, zero-external-dependency runtime promise: the web
  console is embedded, and the proxy runs with just a config file.
- Add/extend masking rules in `config.yaml` (`masking.rules`) rather than
  hardcoding them in callers.
- Open an issue before large changes so we can align on design.

## License

By contributing you agree that your contributions will be licensed under the
[MIT License](./LICENSE).
