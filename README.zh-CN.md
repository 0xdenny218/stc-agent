# stc-agent

[English](README.md) | **简体中文**

**万物皆插件的最小 CLI 对话 agent**——基于
[stc-go](https://github.com/0xdenny218/stc-go)（时空可组合性范式的 Go 实现）。

> 状态：**M0 脚手架**。尚不可用——里程碑见下。

## 是什么

- CLI 对话 agent（stdin/stdout），带工具调用循环。
- 每个能力都是 fiber：模型客户端、工具、斜杠命令、会话、REPL 本身。
  装配完全由范式机制（`Load`/`Provide`/`Inject`/`Effect`）承载，
  `main` 只负责列出组件清单。
- 差异化演示：**对话进行中热替换 WASM guest 工具——不重启、不丢会话。**
  换模型经"重提供 → 级联重载"生效，而会话历史存活——存活是依赖图
  本身的推论，不需要专门机制。
- stc-agent 是 stc-go 的第一个真实消费者：API 摩擦点与卫星包
  （事件派发、config schema、host→guest 带参调用）由这里的真实需求
  拉出，不靠推测。

## 不做

- 不做 UI/TUI/web；不做框架抽象——模式在 agent 里重复出现后再抽取。
- 不是 [dsh](https://github.com/deepseek-ai/deepseek-harness) 竞品：
  不做流式输出、provider 抽象层、MCP/skills/subagent、权限审批流。

## 里程碑

- [ ] M0 脚手架（仓库、CI、双语 README、main 骨架）
- [ ] M1 最小对话闭环（config/model/session/cli fiber，/model 级联）
- [ ] M2 工具系统 + agent 循环（toolset 稳定服务、静态 Go 工具）
- [ ] M3 WASM guest 工具 + 热重载（hmr）：对话中途换工具
- [ ] M4 发布 + 回流评审（哪些模式回流入 stc-go）

## 开发

标准对齐 stc-go：`gofmt`、`go vet`、`go test -race` 全绿。

```sh
go test -race ./...
```

## 许可证

[MIT](LICENSE)
