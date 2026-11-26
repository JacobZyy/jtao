# jtao 设计文档

`jtao` 是为 Codex 等已有 Coding Agent 提供工程工作流、跨会话项目记忆与验证证据的工具。

## 架构图

| 图 | 查看 |
| --- | --- |
| AIOS 仓库总览 | [交互图](./architecture/aios-overview/aios-overview.html) · [可编辑源](./architecture/aios-overview/candidate.json) · [源码分析归档](./architecture/aios-overview/notes.md) |
| AIOS Harness | [交互图](./architecture/aios-harness/aios-harness.html) · [可编辑源](./architecture/aios-harness/candidate.json) |
| AIOS Harness 流程控制 | [Graph 与 Agent 执行闭环](./architecture/aios-harness-flow/aios-harness-flow.html) · [可编辑源](./architecture/aios-harness-flow/candidate.json) · [流程说明](./aios-harness-flow.md) |
| AIOS 记忆模块内部与接口 | [交互详图](./architecture/aios-memory/aios-memory.html) · [可编辑源](./architecture/aios-memory/candidate.json) · [参考与设计草案](./memory-and-persistence.md) · [实现详解](./aios-memory-implementation.md) |
| AIOS 记忆调用与 Memo 流转 | [流程图](./architecture/aios-memo-flow/aios-memo-flow.html) · [可编辑源](./architecture/aios-memo-flow/candidate.json) |

以上总览、Harness 及 Harness 流程控制图采用 AIOS `375ab47` 源码基线；记忆模块图、Memo 流程图及两篇记忆文档已于 2026-11-24 按 AIOS `a9951600`（记忆核心代码与 `375ab47` 一致）完成复核并修正细节。HTML 可直接打开。Archify 源文件中的输出路径相对于 `jtao` 根目录。

## Harness 的四块职责

- **客户端适配**提供 CLI、MCP、Hooks 与能力投影，向外连接现有 Coding Agent。
- **记忆与持久化**保存项目知识、会话、工单状态及证据，为工作流和执行提供跨会话接续。
- **调度执行**在需要时运行和恢复 Team / Solo 任务，日常工作也可以由当前编码客户端执行。
- **工作流与规划**决定下一步和验收条件。AIOS 中由宿主工作流策略与 Rex Harness 共同承担。

## `jtao` 落地设计规范（Go 1.23+ 单文件静态二进制 · 纯 Go SQLite）

| 模块 | 文档 | 状态 |
| --- | --- | --- |
| 记忆与持久化（Memory Core） | [`jtao` 记忆系统技术设计规范](./jtao-memory-design.md) · [参考与调研](./memory-and-persistence.md) | 已拍板定案 |
| 工作流与调度执行（Workflow · Runner · Scheduler） | [`jtao` 工作流与调度执行系统技术设计规范](./jtao-workflow-harness-design.md) · [AIOS 流程复核](./aios-harness-flow.md) | 已拍板定案 |
| 客户端适配与 Go 工程骨架（CLI · MCP · Hooks · Skills） | [`jtao` 客户端适配层与 Go 单二进制工程规范](./jtao-client-adapters-design.md) | 已拍板定案 |

