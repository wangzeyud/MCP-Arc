# MCP Arc — AI 协作开发指引

MCP Arc 是轻量的 MCP Proxy / Sidecar，位于 MCP client 与 server 之间，一行命令为任意 MCP 调用加上审计日志、参数脱敏与调用回放。核心形容词：**轻量、透明、稳定、安全、好装**。

> 目标对齐 MCP 规范版本 **2026-07-28**（传输层：stdio + Streamable HTTP；旧的 HTTP+SSE 适配器保留为 legacy，新部署推荐 Streamable HTTP；方法无关透传）。详细架构红线见规则 `architecture`。

> 本文件是**导航索引**。所有硬约束与详细规范已拆分为 CodeBuddy 项目规则，见 `.codebuddy/rules/`（规则仅在会话开始时注入，修改后需开新会话生效）。

## 规则索引

| 主题 | 规则文件 | 加载模式 |
|------|----------|----------|
| 项目本质、范围边界、架构红线（含 transport 层方法无关透传） | `.codebuddy/rules/architecture/RULE.mdc` | always |
| 开发纪律（单职责 / 轻量 / 兼容 / 测试 / 降级） | `.codebuddy/rules/dev-discipline/RULE.mdc` | always |
| 技术栈与依赖红线（Go 1.27、pure-Go SQLite、禁 Viper/CGO） | `.codebuddy/rules/tech-stack/RULE.mdc` | always |
| 代码风格与 JetBrains 现代 Go 指南 | `.codebuddy/rules/code-style/RULE.mdc` | agent-requested |
| 版本进度（v0.1–v0.5） | `.codebuddy/rules/progress/RULE.mdc` | agent-requested |
| 品牌命名、仓库、协作提醒策略 | `.codebuddy/rules/project-meta/RULE.mdc` | agent-requested |

## 仓库

https://github.com/wangzeyud/MCP-Arc
