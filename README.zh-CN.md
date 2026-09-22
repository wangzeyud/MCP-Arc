[English](./README.md) · [中文](./README.zh-CN.md)

# MCP Arc

一个轻量的 MCP Proxy，位于 MCP client 与 server 之间。一行命令为任意 MCP 调用加上审计日志、参数脱敏、调用回放与限流——不侵入协议，不绑定厂商。

项目地址：https://github.com/wangzeyud/MCP-Arc

## 核心功能

只做四件事：

| | |
|---|---|
| **参数脱敏** | 落库前自动遮蔽 `tools/call` 参数（与返回）里的 PII / 密钥。正则 + 敏感字段名，YAML 配或运行时改。 |
| **调用审计** | 谁（`client_id`）、何时、调了哪个 tool、传了什么、返回什么、耗时、成败——落 SQLite（默认）或 PostgreSQL。 |
| **回放** | 把记录的一次 `tools/call` 原样重发上游，用于调试或合规重执行。 |
| **限流** | 按 `client_id` 令牌桶 QPS + 日配额。 |

编译进二进制的 Vue3 控制台只是上面四项的**观测 UI**（审计日志、脱敏规则、回放），不是第五个功能。规则热加载；审计记录支持 JSON / CSV 导出。

## 设计

- 工作在**传输层**（stdio / Streamable HTTP / SSE-legacy），不深入 protocol 内部。
- 唯一 MCP 知识：`tools/call` 方法名与 `params.{name,arguments}` 形状。
- 请求进来改写 JSON-RPC `id`、响应回来还原，以此关联路由——响应是**匹配**回来的，不是**解析**出来的。不需要处理的消息逐字节原样转发。

## 安装

单文件二进制。

### A. Docker
```bash
docker compose up --build                       # SQLite 后端，控制台在 :8080
docker compose --profile postgres up --build    # PostgreSQL 后端
```
构建后代理地址为 `http://localhost:8081/sse`（网关模式）。

### B. 预编译二进制（v1.0 提供）
一键安装（`brew` / `go install`）与跨平台预编译二进制已列入 **v1.0** 路线图。在此之前请用 Docker 或从源码构建。

### C. 从源码构建
需要 **Go 1.27**。SQLite 驱动纯 Go（`modernc.org/sqlite`），**不需要 CGO**。
```bash
cd web && npm install && npm run build && cd ..   # 构建内嵌控制台（在 web/ 目录）
go build -o mcp-arc ./cmd/mcp-arc                 # macOS / Linux
# go build -o mcp-arc.exe ./cmd/mcp-arc           # Windows
```
> ⚠️ `npm install` 在 `web/` 里跑，不要在仓库根目录（那里没有 `package.json`）。

## 快速开始
```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"message":"my email is a@b.com and pwd secret123"}}}' \
  | ./mcp-arc --upstream "node examples/echo-server/server.js"
```
邮箱和 `pwd` 字段在审计日志里以**脱敏**形式存储，真实请求仍原样到达上游。查看日志：
```bash
curl -H "Authorization: Bearer change-me" localhost:8080/api/logs
curl -H "Authorization: Bearer change-me" localhost:8080/api/stats
```

## 传输方式

| `transport.client` | `transport.upstream` | 场景 |
|---|---|---|
| `stdio`（默认） | `stdio`（默认） | 本地透明代理 |
| `sse` | `stdio` | **网关模式**：远端/多客户端经 HTTP 连 Arc，Arc 前端一个本地 stdio server（SSE 为 legacy，推荐用 Streamable HTTP） |
| `sse` | `sse` | 完全远程 |
| `stdio` | `sse` | 本地 client → 远程 SSE server |
| `streamable-http` | `streamable-http` | 客户端与 server 经单 POST 端点通信（MCP 2026-07-28） |
| `streamable-http` | `stdio` | 远端客户端经 Streamable HTTP → 本地 stdio server |

```yaml
server:
  upstream: ["node", "examples/mcp-server-demo/server.js"]
transport:
  client: sse
  listen: ":8081"
  upstream: stdio
```
客户端连 `http://host:8081/sse`。

命令行参数：`--config`、`--upstream`、`--client-transport`（`stdio`|`sse`|`streamable-http`）、`--listen`、`--upstream-transport`（`stdio`|`sse`|`streamable-http`）、`--upstream-url`。

## 接入真实客户端

让**客户端**启动 MCP Arc 并和它对话，而不是直连 server。

### 本地（stdio）
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
Claude Desktop、Cursor、Cline、Windsurf、Zed 等通用。
- **Claude Desktop** —— `~/Library/Application Support/Claude/claude_desktop_config.json`（macOS）或 `%APPDATA%\Claude\claude_desktop_config.json`（Windows）
- **Cursor** —— `.cursor/mcp.json`
- **Cherry Studio / Trae / 5ire / Lingma** —— 设置 → MCP → 添加本地 server

改完重启客户端。

### 远端（SSE 网关）
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

## 配置

详见 `config.yaml`。

| key | 含义 |
|---|---|
| `server.client_id` | 写入每条审计记录的标签（或环境变量 `MCP_ARC_CLIENT_ID`） |
| `server.upstream` | 写在配置里、替代 `--upstream` 的命令 + 参数 |
| `transport` | `client` / `upstream`（`stdio`\|`sse`）、`listen`、`upstream_url` |
| `audit` | `enabled`、`driver: sqlite`（默认）或 `postgres`、`dsn` |
| `masking` | `enabled` + `rules`（正则 `patterns` 和/或 `fields`）+ 可选 `presets` |
| `llm` | 可选 LLM 辅助脱敏：`enabled`、`endpoint`、`api_key`、`model`、`timeout_ms`、`max_bytes`、`cache_ttl_seconds`、`apply_to_result` |
| `rate_limit` | `enabled`、`qps`、`daily_quota`（按 client_id） |
| `admin` | `enabled`、`port`、`token`（控制台 Bearer Token） |
| `alerting` | v0.7 新增的出站 webhook 告警：`enabled`、`webhook.url`、`webhook.secret`（HMAC-SHA256 密钥）、`events`（事件名子集，留空=全部）、`timeout_ms` |

## 配置热加载

多数运行期配置——`rate_limit`、`audit.retention`、`alerting`、以及 `masking` 规则——**无需重启即生效**。mcp-arc 监听配置文件（按 mtime，约每 5s 轮询），并提供手动触发入口：

```bash
curl -X POST -H "Authorization: Bearer change-me" localhost:8080/api/config/reload
```

每次热加载都会按新配置重新应用限流器、脱敏器、保留清理 worker 与告警发送器。但 **需要重启才能生效** 的字段——`transport`（client/upstream/listen）、`admin.port`、`audit` 的 driver / dsn、以及 `server.upstream` 命令——无法在运行时变更；这类变更的热加载会触发 `restart_required` 告警（见下），提醒你重启进程。

## 告警（webhook）

mcp-arc 可以把运维事件以 HTTP webhook 形式 POST 出去（v0.7）。在 `config.yaml` 里配置：

```yaml
alerting:
  enabled: true
  timeout_ms: 5000
  # events：留空或省略 = 全部五个
  events: [sensitive_detected, rate_limited, audit_error]
  webhook:
    url: "https://my.example.com/mcp-arc-alerts"
    secret: "your-hmac-secret"   # 留空 = 不签名
```

每次投递都是带 `X-MCPArc-Signature: sha256=<hmac>` 签名的 JSON body（对原始 body 做 HMAC-SHA256，hex 编码）。投递 **best-effort 且 fail-open**：端点故障或超时绝不会阻塞请求或审计写入。

触发的事件：

| 事件 | 触发时机 |
|---|---|
| `config_reloaded` | 一次热加载（文件监听或手动触发）成功应用 |
| `restart_required` | 热加载触碰了需要重启才生效的字段 |
| `sensitive_detected` | 脱敏层检出了仍需遮蔽的敏感数据（例如 LLM 第二遍抓到静态规则漏网的） |
| `rate_limited` | 请求被限流器拒绝 |
| `audit_error` | 审计记录写入失败 |

`events` 留空时，上述五个事件全部发送。

## 脱敏规则

规则存在库里（`mask_rules`）；首次启动 `config.yaml` 里的规则会**种子化**写入（`source: config`），之后归控制台管——新建、编辑、启停、删除，且**每次写入热加载**，下一条调用即生效，无需重启。

正则在保存时编译校验，写错会被当场拒绝，不会悄悄让脱敏失效。

### 预置脱敏模板
在 `masking.presets` 列出名字即可启用：

| 模板 | 匹配 | 脱敏为 |
|---|---|---|
| `phone_cn` | 中国大陆手机号 | `[PHONE]` |
| `ip` | IPv4 | `[IP]` |
| `bank_card_cn` | 银联卡（以 62 开头） | `[BANKCARD]` |
| `passport` | 护照号 | `[PASSPORT]` |
| `mac` | MAC 地址 | `[MAC]` |

姓名、住址等自由文本 PII 故意不内置正则（误伤大），请用 LLM 辅助脱敏第二遍。

```bash
curl -H "Authorization: Bearer change-me" localhost:8080/api/rules
curl -X POST -H "Authorization: Bearer change-me" -H "Content-Type: application/json" \
  -d '{"name":"phone_cn","patterns":["\\b1[3-9]\\d{9}\\b"],"mask_char":"[PHONE]"}' \
  localhost:8080/api/rules
```

## LLM 辅助脱敏

可选**第二遍**，补静态规则抓不到的自由文本 PII。模型只拿到**已脱敏**的 payload，只被问"还有哪些路径仍敏感"；已抹掉的值不离开进程，幻觉路径直接忽略。全程 **fail-open**：出错、超时、或超 `max_bytes`，沿用静态脱敏结果，调用照常继续。

```yaml
llm:
  enabled: true
  endpoint: "https://api.openai.com/v1"   # 任意 OpenAI 兼容端点
  model: "gpt-4o-mini"
  timeout_ms: 3000
  max_bytes: 8192
  apply_to_result: false
```

## 导出
```bash
curl -H "Authorization: Bearer change-me" "localhost:8080/api/export?format=csv"
curl -H "Authorization: Bearer change-me" "localhost:8080/api/export?format=json&limit=5000"
curl -H "Authorization: Bearer change-me" "localhost:8080/api/export?format=json&raw=1"  # 含未脱敏值
```
支持按 `client_id` / `tool` / `limit` 过滤（`limit` 上限 10000）。导出为**已脱敏**参数与结果；`raw_params` / `raw_result` 仅显式带 `raw=1` 才带上。

## Web 控制台
Vue3 控制台**编译进二进制**（`//go:embed`），由管理 HTTP 服务托管，无需单独前端进程。
```bash
./mcp-arc --config config.dev.yaml
# 打开 http://localhost:8080  →  仪表盘 / 调用日志 / 规则
```

控制台 **Dashboard** 展示实时调用量、错误率，以及延迟 **分位数（P50 / P95 / P99，单位 µs）**，外加按 tool / client 的拆分与每日调用时序。

## 回放
每次 `tools/call` 都记录原始（未脱敏）请求参数与上游原始响应。
```bash
curl -H "Authorization: Bearer change-me" localhost:8080/api/logs        # 找调用 id
curl -X POST -H "Authorization: Bearer change-me" -H "Content-Type: application/json" \
     -d '{"call_id": 1}' localhost:8080/api/replay                       # 回放
```
复用与实时调用相同的 id 改写 / 响应关联机制，stdio 与 SSE 上游都适用。

## 更新日志

### v0.7 —— 交付增强（配置热加载 + 告警 webhook + 统计）
- **配置热加载** —— `rate_limit`、`audit.retention`、`alerting`、以及 `masking` 规则现在支持运行期生效：监听配置文件（mtime，约每 5s 轮询），`POST /api/config/reload` 手动触发。限流器、脱敏器、保留清理 worker、告警发送器都会按新配置重新应用。若热加载触碰了需要重启才生效的字段（`transport`、`admin.port`、`audit` driver/dsn、`server.upstream`），不再静默 no-op，而是触发 `restart_required` 告警。
- **告警 webhook** —— 把运维事件（`config_reloaded`、`restart_required`、`sensitive_detected`、`rate_limited`、`audit_error`）以 HTTP 形式出站。JSON body 带 `X-MCPArc-Signature: sha256=<hmac>`（HMAC-SHA256）签名；投递 best-effort 且 fail-open，端点故障绝不会阻塞请求或审计写入。
- **统计增强** —— 控制台 Dashboard 现在展示延迟 **分位数（P50 / P95 / P99）**（取代原先恒为 0 的均值），外加错误率、按 `client_id` / `tool` 的拆分与每日调用时序，全部由服务端基于审计库实时聚合。

### v0.6 —— 四件套打磨（回放 + 审计保留）
- **批量回放** —— `POST /api/replay/batch` 一次性重发多条已记录的 `tools/call`，支持显式 `call_ids` 列表或用 `filter`（tool / client_id / since / until / limit）解析。顺序执行，复用同一套 id 改写/关联机制；单条失败只记录、不中断其余。
- **回放 Diff** —— 带 `diff: true` 时，把每条重放响应与原始记录的 `raw_result` 做**精确 JSON 比对**，用于验证上游是否仍返回一致结果。单条与批量回放均支持。
- **审计保留** —— `audit.retention`（`max_age_days` / `max_rows`，默认关闭）限制审计库增长。启动时裁剪 + 周期后台清理 worker（SQLite / PostgreSQL / memory），fail-open：清理出错绝不阻塞主流程。
- **控制台** —— 调用日志页支持多选回放 + Diff 开关 + 逐条结果面板。

### v0.5 —— 规范对齐（MCP 2026-07-28）
- **Streamable HTTP 传输** —— 客户端与上游均为单 POST 端点（`transport.client` / `transport.upstream: streamable-http`），符合 2026-07-28 规范。旧的 HTTP+SSE 适配器保留为 `sse` 并标记 **legacy**；新部署推荐 Streamable HTTP。
- **方法无关透传** —— 原样转发所有 JSON-RPC 方法（tools/*、resources/*、prompts/*、sampling、elicitation、roots、subscriptions、MRTR、progress、cancellation 等）；仅对 `tools/call`（及 sampling/elicitation）入参做脱敏/审计解析。
- **通知作用域与取消传播（T4–T5）** —— 流内通知路由回所属请求而非广播给所有客户端；客户端断开会主动取消在途上游请求。已由代理级 E2E 测试 `TestE2EStreamableHTTPProxyCancelOnClientDisconnect` 验证。
- **`InputRequiredResult` 审计** —— 审计且不嵌套脱敏。
- 传输单测 + soak 变体；SSE 标记 legacy。

### v0.4 —— 稳定性 / soak 测试
- **soak 稳定性测试** —— `TestProxySoakStability` 驱动 2 万次代理往返，断言无 pending 请求泄漏、审计计数一致、goroutine 数量有界。
- **race 验证** —— 全量 `go test -race ./...` 在 proxy / mask / audit / admin / llm 各包均通过，无数据竞争。
- 纯内部质量，不新增产品能力。

### v0.3 —— 稳定性与生产可用性
- **预置脱敏模板** —— `phone_cn` / `ip` / `bank_card_cn` / `passport` / `mac` 五个模板，按名引用即可。
- **审计写入异步化 + 超时** —— 缓冲队列 + 后台 worker，带单条写入墙钟超时；数据库慢/卡死绝不阻塞响应路径（fail-open）。
- **上游韧性** —— 上游崩溃/退出自动重连重启；审计写入失败返回明确 `-32002`，而非静默挂起。
- **回放 UI** —— 可在控制台查看并回放历史 `tools/call`。
- **优雅关闭** —— 收到 `SIGINT`/`SIGTERM`，先排干审计缓冲队列（受 `audit.shutdown_flush_timeout_ms` 约束，默认 5s）再关库，在途请求下发明确 `upstream disconnected` 错误。

## 路线图

- **v0.1** ✅ stdio / SSE 传输、审计（SQLite + PostgreSQL）、脱敏、限流、控制台、回放。
- **v0.2** ✅ LLM 辅助脱敏、控制台内规则增删改、JSON / CSV 导出。
- **v0.3** ✅ 稳定性与生产可用性：预置脱敏模板、审计写入异步化+超时降级、上游重连、回放 UI、优雅关闭 flush。
- **v0.4** ✅——稳定性 / soak 测试 + race 验证（内部质量，非新功能）。
- **v0.5** ✅——规范对齐（MCP 2026-07-28）：Streamable HTTP 传输、方法无关透传、通知作用域与取消传播（客户端断开取消在途上游）、`InputRequiredResult` 审计；SSE 标记 legacy。
- **v0.6** ✅——四件套打磨：批量回放 + 回放 Diff（`/api/replay/batch`，与记录结果精确 JSON 比对）、审计保留（`audit.retention` 的 max_age_days/max_rows，启动裁剪 + 周期 worker）、控制台多选回放 UI。
- **v0.7** ✅——交付增强：配置热加载（文件监听 + `POST /api/config/reload`；rate_limit/retention/alerting/masking 热应用；需重启字段改了会告警）、告警 webhook（HMAC 签名、fail-open、5 类事件）、统计增强（延迟 P50/P95/P99、错误率、按 client/tool、每日时序）。

## License
MIT —— 见 [LICENSE](./LICENSE)。
