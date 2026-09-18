[English](./README.md) · [中文](./README.zh-CN.md)

# MCP Arc

一个**轻量的 MCP Proxy**，位于 MCP client 与 MCP server 之间。
一行命令，为任意 MCP 调用加上审计日志、参数脱敏、调用回放与限流——不侵入协议，不绑定厂商。

```
MCP client  ──────▶  MCP Arc  ──────▶  MCP server
(Claude Desktop /    │  治理层          (node / python / …)
 Cherry Studio /     │
 Cursor …)           └─ 审计 ─▶ SQLite / PostgreSQL
```

**项目地址：** https://github.com/wangzeyud/MCP-Arc

## 核心功能

MCP Arc 只做四件事，不多也不少：

| | |
|---|---|
| **参数脱敏** | 自动识别并遮蔽 `tools/call` 参数（与返回）里的 PII / 密钥，再落库。正则 + 敏感字段名，YAML 里配，或运行时改。 |
| **调用审计** | 谁（`client_id`）、何时、调了哪个 tool、传了什么参数、返回了什么、耗时多少、成功还是失败——落 SQLite（默认）或 PostgreSQL。 |
| **回放** | 把记录过的一次 `tools/call` 原样重新发往上游，用于调试不稳定的 tool，以及合规性的重新执行。 |
| **限流** | 按 `client_id` 的令牌桶 QPS + 日配额，避免某个客户端把上游打爆。 |

编译进单个二进制的 Vue3 控制台，只是上面四项能力的**观测 UI**（审计日志、脱敏规则、回放），不是第五个功能。脱敏规则可运行时修改（热加载、无需重启），审计记录支持 JSON / CSV 导出。

## 设计

- 工作在**传输层**（stdio / SSE）之上，不深入 protocol 内部。
- 唯一依赖的 MCP 知识，是 `tools/call` 这个方法名和 `params.{name,arguments}` 的参数形状。
- 请求进来时把 JSON-RPC 的 `id` 改写成内部 id，响应回来时还原，以此做关联与路由——
  响应是**匹配**回来的，不是**解析**出来的。其余部分都是与协议无关的管道，spec 演进
  不需要重写治理层。
- 不需要处理的消息，逐字节原样转发。

## 安装

MCP Arc 是一个单文件二进制。按你的情况选一条路：

### A. Docker（最省事——本机不需要 Go 或 Node）

只需装好 Docker；容器会替你把二进制**和** Web 控制台一起构建好，本机干干净净。

```bash
docker compose up --build                       # SQLite 后端，控制台在 :8080
docker compose --profile postgres up --build    # PostgreSQL 后端
```

构建后代理地址为 `http://localhost:8081/sse`（网关模式）。直接跳到
[接入真实客户端](#接入真实客户端)。

### B. 预编译二进制（v1.0 提供）

一键安装（脚本 / `brew` / `go install`）以及 Windows / macOS / Linux 的预编译二进制，
已列入 **v1.0** 路线图。在此之前请用 Docker（方案 A）或从源码构建（方案 C）。

### C. 从源码构建（开发者）

需要 **Go 1.27**。SQLite 驱动是纯 Go 实现（`modernc.org/sqlite`），**不需要 C 编译器（CGO）**。

```bash
# 1) 构建内嵌的 Web 控制台——前端代码在 web/，不在仓库根目录
cd web && npm install && npm run build && cd ..

# 2) 编译二进制
go build -o mcp-arc ./cmd/mcp-arc          # macOS / Linux
# go build -o mcp-arc.exe ./cmd/mcp-arc   # Windows
```

> ⚠️ 如果在仓库根目录直接跑 `npm install`，会看到
> `ENOENT ... package.json`：前端工程在 **`web/`** 目录里。
> （`Makefile` 里的 `make build` 会一次跑完两步，但 Windows 不自带 `make`——
> 上面的命令在任何系统都能用。）

## 快速开始

拿到 `mcp-arc` 二进制（或跑起 Docker 容器）后，通过代理管道发两条 JSON-RPC 消息，
就能看到脱敏 + 审计生效：

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"message":"my email is a@b.com and pwd secret123"}}}' \
  | ./mcp-arc --upstream "node examples/echo-server/server.js"
```

你会看到上游的响应被打印出来，同时生成 `mcp-arc.db`。上面的邮箱和 `pwd` 字段在审计
日志里以**脱敏**形式存储，而真实请求仍然原样到达了上游：

```bash
curl -H "Authorization: Bearer change-me" localhost:8080/api/logs
curl -H "Authorization: Bearer change-me" localhost:8080/api/stats
```

## 传输方式

客户端侧与上游侧独立配置，都支持 `stdio` 与 `sse`。

| `transport.client` | `transport.upstream` | 场景 |
|---|---|---|
| `stdio`（默认） | `stdio`（默认） | 本地透明代理，client 直接 spawn MCP Arc |
| `sse` | `stdio` | **网关模式**：远端 / 多客户端经 HTTP 连 MCP Arc，Arc 前端一个本地 stdio server |
| `sse` | `sse` | 完全远程：Arc 在中间做治理，两侧都是 HTTP |
| `stdio` | `sse` | 把本地 client 的调用转发到远程 SSE server |

```yaml
server:
  upstream: ["node", "examples/mcp-server-demo/server.js"]
transport:
  client: sse
  listen: ":8081"
  upstream: stdio
```

运行后 MCP 客户端连 `http://host:8081/sse`（GET 建立事件流，POST
`/messages?sessionId=...` 发送请求）。

### 命令行参数

`--config`、`--upstream`、`--client-transport`（`stdio`|`sse`）、`--listen`、
`--upstream-transport`（`stdio`|`sse`）、`--upstream-url`。

```bash
./mcp-arc --client-transport sse --listen :8081 \
          --upstream-transport sse --upstream-url https://host/mcp/sse
```

## 接入真实客户端

你完全不用改自己的 MCP server——只要让**客户端**去启动 MCP Arc、并和它对话，
而不是直接连 server。

### 本地（stdio，默认）

把客户端指向 `mcp-arc` 二进制。下面这段 `mcpServers` 配置在 Claude Desktop、
Cursor、Cline、Windsurf、Zed 等绝大多数 MCP 客户端里都能直接用：

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

配置文件放在哪：
- **Claude Desktop** —— `~/Library/Application Support/Claude/claude_desktop_config.json`（macOS）或 `%APPDATA%\Claude\claude_desktop_config.json`（Windows）
- **Cursor** —— 项目里的 `.cursor/mcp.json`（或 `~/.cursor/mcp.json`）
- **Cherry Studio / Trae / 5ire / Lingma** —— 设置 → MCP →「添加本地 server」→ 填入上面的 command 与 args

改完重启客户端即可；它会拉起 `mcp-arc`，`mcp-arc` 再去拉起你真正的 server。整个接入就这一步。

### 远端（SSE 网关）

如果你希望把 Arc 作为一个共享网关来跑（例如用 Docker 容器，或跑在一台服务器上），
就让它跑在网关模式，然后让客户端填它的 SSE 地址，而不是本地命令：

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

## 配置

详见 `config.yaml`。关键配置项：

| key | 含义 |
|---|---|
| `server.client_id` | 写入每条审计记录的标签（或环境变量 `MCP_ARC_CLIENT_ID`） |
| `server.upstream` | 写在配置里、替代 `--upstream` 的命令 + 参数 |
| `transport` | `client` / `upstream`（`stdio`\|`sse`）、`listen`（SSE 监听地址）、`upstream_url` |
| `audit` | `enabled`、`driver: sqlite`（默认）或 `postgres`、`dsn` |
| `masking` | `enabled` + `rules`（正则 `patterns` 和 / 或 `fields`）+ 可选 `presets`（命名模板列表） |
| `llm` | 可选的 LLM 辅助脱敏：`enabled`、`endpoint`、`api_key`、`model`、`timeout_ms`、`max_bytes`、`cache_ttl_seconds`、`apply_to_result` |
| `rate_limit` | `enabled`、`qps`、`daily_quota`（按 client_id） |
| `admin` | `enabled`、`port`、`token`（控制台的 Bearer Token） |

## 脱敏规则

规则存在库里（`mask_rules` 表），不再只活在 YAML 中：首次启动时 `config.yaml`
里的规则会被**种子化**写入表中（`source: config`），之后规则归控制台管——可新建、
编辑、启停、删除，**每次写入都会热加载**，下一条 tool 调用就用新规则，不用重启。

### 预置脱敏模板

常见的 PII 模式已作为命名模板内置，省得你自己重写正则。在 `config.yaml` 的
`masking.presets` 里列出名字即可启用：

| 模板 | 匹配 | 脱敏为 |
|---|---|---|
| `phone_cn` | 中国大陆手机号 | `[PHONE]` |
| `ip` | IPv4 地址 | `[IP]` |
| `bank_card_cn` | 银联卡（以 62 开头） | `[BANKCARD]` |
| `passport` | 护照号 | `[PASSPORT]` |
| `mac` | MAC 地址 | `[MAC]` |

它们会像普通配置规则一样种子化进规则表，控制台仍可编辑或停用。姓名、住址等
自由文本 PII 故意不内置正则（误伤太大），这类请用 LLM 辅助脱敏第二遍。

一条规则必须有 `name`，且至少有 `pattern` 或 `field` 之一；正则在保存时会做编译校验，
写错的正则会被当场拒绝，而不是悄悄让脱敏失效。

```bash
curl -H "Authorization: Bearer change-me" localhost:8080/api/rules

curl -X POST -H "Authorization: Bearer change-me" -H "Content-Type: application/json" \
  -d '{"name":"phone_cn","patterns":["\\b1[3-9]\\d{9}\\b"],"mask_char":"[PHONE]"}' \
  localhost:8080/api/rules
```

## LLM 辅助脱敏

可选的**第二遍**，补静态规则抓不到的自由文本 PII。模型只拿到**已脱敏**的 payload，
只被问"还有哪些路径仍然敏感"；已被抹掉的值不会离开进程，幻觉出来的路径直接忽略。
全程 **fail-open**：出错、超时、或 payload 超过 `max_bytes`，就沿用静态脱敏的结果，
调用照常继续。

```yaml
llm:
  enabled: true
  endpoint: "https://api.openai.com/v1"   # 任意 OpenAI 兼容端点
  # api_key 建议用环境变量 MCP_ARC_LLM_API_KEY
  model: "gpt-4o-mini"
  timeout_ms: 3000
  max_bytes: 8192
  apply_to_result: false                  # 是否也扫上游返回
```

## 导出

```bash
curl -H "Authorization: Bearer change-me" "localhost:8080/api/export?format=csv"
curl -H "Authorization: Bearer change-me" "localhost:8080/api/export?format=json&limit=5000"
curl -H "Authorization: Bearer change-me" "localhost:8080/api/export?format=json&raw=1"  # 含未脱敏值
```

支持按 `client_id` / `tool` / `limit` 过滤（`limit` 上限 10000）。导出内容是**已脱敏**
的参数与结果；未脱敏的 `raw_params` / `raw_result` 只有显式带 `raw=1` 才会带上。
控制台「调用日志」页的 Export JSON / CSV 按钮走的是同一个接口。

## Web 控制台

Vue3 控制台被**编译进二进制**（`//go:embed`），由管理 HTTP 服务直接托管，无需单独的
前端进程。

```bash
./mcp-arc --config config.dev.yaml
# 打开 http://localhost:8080  →  仪表盘 / 调用日志 / 规则
```

## 回放

每一次 `tools/call` 都会记录其原始（未脱敏）请求参数与上游原始响应：

```bash
# 1) 找到调用 id
curl -H "Authorization: Bearer change-me" localhost:8080/api/logs

# 2) 回放（返回上游的原始 JSON-RPC 响应）
curl -X POST -H "Authorization: Bearer change-me" -H "Content-Type: application/json" \
     -d '{"call_id": 1}' localhost:8080/api/replay
```

回放复用和实时调用相同的 id 改写 / 响应关联机制，因此 stdio 与 SSE 上游都适用。

## 更新日志

### v0.3 —— 稳定性与生产可用性
- **预置脱敏模板** —— 内置 `phone_cn` / `ip` / `bank_card_cn` / `passport` / `mac` 五个模板；在 `masking.presets` 按名引用即可。
- **审计写入异步化 + 超时** —— 审计写入卸载到缓冲队列 + 后台 worker，并带单条写入的墙钟超时；数据库慢/卡死也绝不阻塞响应路径（best-effort，失败放开）。
- **上游韧性** —— 上游崩溃/退出自动重连重启；审计写入失败时客户端收到明确的 `-32002` 错误，而非静默挂起。
- **回放 UI** —— 可在 Web 控制台（Dashboard / Call Logs / Replay）查看并回放历史 `tools/call` 调用。
- **优雅关闭** —— 收到 `SIGINT`/`SIGTERM` 时，先排干审计缓冲队列（受 `audit.shutdown_flush_timeout_ms` 约束，默认 5s）再关库，并给在途请求下发明确的 `upstream disconnected` 错误，不再让客户端挂死。

## 路线图

- **v0.1** ✅ stdio / SSE 传输、审计（SQLite + PostgreSQL）、脱敏、限流、控制台、回放。
- **v0.2** ✅ LLM 辅助脱敏、控制台内规则增删改、JSON / CSV 导出。
- **v0.3** ✅ 稳定性与生产可用性：预置脱敏模板、审计写入异步化+超时降级、上游重连、回放 UI、优雅关闭 flush。
- **v0.4** 📋（计划中）——稳定性 / 7×24 soak 测试（内部质量，非新功能）。

## License

MIT —— 见 [LICENSE](./LICENSE)。
