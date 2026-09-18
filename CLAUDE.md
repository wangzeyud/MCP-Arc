# MCP Arc — AI 协作开发指引

## 项目本质（不可动摇）

MCP Arc 是一个轻量的 MCP Proxy / Sidecar，位于 MCP client 和 MCP server 之间。
一行命令为任意 MCP 调用加上审计日志、参数脱敏和调用回放。
不侵入协议，不绑定厂商。

核心形容词：轻量、透明、稳定、安全、好装。

**范围边界（已锁定，不可动摇）**：MCP Arc 只做四件事——① 参数脱敏（含 LLM 兜底）② 调用审计 ③ 回放 / 重放 ④ 限流。嵌入式 Web 控制台仅是这四项的**观测 UI**（看审计、改规则、做回放），不是第五个功能；Docker / 安装脚本 / SSE TLS 属于交付与安全加固，不是产品功能。任何超出此四项的诉求（多租户、策略引擎、分布式部署、告警 webhook、统计面板增强、配置热加载等）均视为偏离，必须先用一句话说明原因并征求确认，不得擅自加入。

## 架构红线（违反任何一条都算偏离）

1. **协议解耦**：只工作在 transport 层（stdio / SSE），只识别 `tools/call` 方法名和参数形状。不深入解析 MCP payload 内部结构。MCP spec 变更时核心逻辑不受影响。
2. **零侵入**：client 和 server 都不需要修改。对 client 来说 mcp-arc 就是 server，对 server 来说 mcp-arc 就是 client。
3. **低依赖**：默认 SQLite，可选 PostgreSQL。单二进制分发（Vue3 面板通过 //go:embed 打包）。
4. **配置驱动**：行为由 YAML 控制，不硬编码业务逻辑。

## 技术栈

- Go 1.27（proxy 核心 + admin REST API，pure-Go SQLite：modernc.org/sqlite，无 CGO）
- Vue3 + Element Plus（嵌入二进制）
- 存储：SQLite（默认）→ PostgreSQL（可选）
- 配置：YAML（自研 `config.Load()` 加载，不引入 Viper 自动绑定）
- CLI：Cobra（仅用于命令行参数解析，解析后传给业务逻辑；不引入 Viper）

## 当前进度

| 版本 | 状态 | 内容 |
|------|------|------|
| v0.1 | ✅ 完成 | stdio/SSE 传输、审计（SQLite+PG）、脱敏、限流、控制台、回放 |
| v0.2 | ✅ 完成 | LLM 辅助脱敏、控制台规则 CRUD、JSON/CSV 导出 |
| v0.3 | ✅ 完成 | 上游异常优雅处理（重连/重启）、审计写入异步化+超时降级、预置脱敏规则模板、审计回放 UI、优雅关闭 flush（不丢审计/不挂客户端）；配置热加载/告警 webhook/统计增强/7×24 soak 移至 v0.4 |
| v0.4 | 📋 待启动 | 稳定性 / 7×24 soak 测试（内部质量，非新功能） |

## 开发纪律

1. **不做超出当前版本范围的事**。v0.3 就做 v0.3 的事。如果改动涉及 v2.0 才该做的（多租户、策略引擎、集群），明确告知我并建议推迟。
2. **每个改动只做一件事**。不要顺手重构不相关的模块。
3. **保持轻量**。新功能如果让二进制增加 >5MB 或引入新的运行时依赖，必须说明理由。
4. **向后兼容**。配置格式变更必须支持旧配置自动迁移，不能让用户升级后启动失败。
5. **测试覆盖**。涉及 proxy 核心（消息拦截、转发、脱敏、回放）的改动，必须有单元测试覆盖 JSON-RPC 边界情况。
6. **优雅降级**。任何非核心功能（审计写入失败、上游异常、webhook 超时）都不能阻塞主流程（请求转发）。

## 代码风格

- Go：标准库优先，显式错误处理，不忽略 error
- Vue3：Composition API
- 日志：结构化，级别分明（debug/info/warn/error）
- 导出的函数/方法必须有注释

## Go 代码规范（JetBrains Modern Go Guidelines）

所有新写的 Go 代码必须遵循 JetBrains 现代 Go 指南：
https://github.com/JetBrains/go-modern-guidelines

### 核心原则
- 以 `go.mod` 的 `go` 指令为准（当前 `go 1.27.0`），只使用该版本及之前引入的语言/标准库特性，不超前。
- 写代码时优先采用现代惯用法，而非旧模式。
- 这些写法与 Go 团队官方 `modernize` 分析器目标一致；若环境可安装，优先用 `modernize` 自动改写。本机当前网络无法拉取该工具，故采用手动应用。

### 现代惯用法速查（按引入版本）
- 1.18+：`any` 代替 `interface{}`；`strings.Cut` / `bytes.Cut` 代替 `Index`+手动切片
- 1.19+：`fmt.Appendf`；类型化原子 `atomic.Bool/Int64` 代替 `int32`+`atomic.StoreInt32`
- 1.20+：`strings.CutPrefix`/`CutSuffix`；`errors.Join` 代替 `fmt.Errorf("%v; %w", ...)`；`context.WithCancelCause`
- 1.21+：内建 `min`/`max`；`clear()`；`slices.Contains/Index/Sort/SortFunc/Max/Min/Reverse/Clone/Compact/Clip`；`maps.Clone/Copy/DeleteFunc`；`sync.OnceFunc`/`OnceValue`；`cmp.Or`；`reflect.TypeFor[T]()`；`context.AfterFunc`
- 1.22+：`for i := range n` 代替 `for i := 0; i < n; i++`；闭包/goroutine 内无需 `v := v` 拷贝；`http.ServeMux` 模式 `GET /api/{id}` + `r.PathValue`
- 1.23+：`maps.Keys/Values` 直接迭代；`slices.Collect`/`slices.Sorted`；`strings.SplitSeq`/`bytes.FieldsSeq`
- 1.24+：测试用 `t.Context()`；`json` 字段 `omitzero` 代替 `omitempty`（bool/数字/struct/time）；`testing.B.Loop()`
- 1.25+：`sync.WaitGroup.Go(func())`
- 1.26+：`new(42)` 代替取地址辅助函数；`errors.AsType[*T](err)`
- 1.27+：泛型方法 `func (s Set[T]) Map[U any](...)`；`strings.CutLast`/`bytes.CutLast`；标准库 `uuid` 代替 `github.com/google/uuid`；`url.Clone()`

### 注意事项
- `encoding/json/v2` 会改变序列化语义（nil slice/map 输出 `[]`/`{}` 而非 `null`），影响审计 API 的 JSON 输出，**仅在确认无兼容问题或全新代码时采用**。
- `generic_methods`、`stdlib_uuid` 属结构性改动，应用前需评估影响。
- 当前代码已对齐：`any`、`errors.Is` 哨兵比较、`range` 计数循环、无 `sort.` 直接用 `slices`/`maps`。

## 品牌与命名

- 项目名：MCP Arc
- Repo：mcp-arc
- CLI 命令：mcp-arc
- 二进制：mcp-arc
- 所有旧名称（MCPGW / mcpgw）必须清除，新代码不得出现

## 当我请求帮助时

- 如果我的请求偏离核心定位（轻量、透明、稳定、安全、好装），直接告诉我。
- 如果某个功能应该推迟到后续版本，说明原因并建议版本号。
- 如果发现现有代码违反架构红线，指出并给出重构方案。
- 生成的代码应该能直接编译运行，不留 TODO（除非我明确要求）。
- 如果我在 v0.3 阶段要求你做 v1.0 的安装脚本或 TLS，提醒我当前优先级。

## 仓库

https://github.com/wangzeyud/MCP-Arc
