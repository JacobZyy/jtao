# 记忆与持久化

2026-11-21 归档 · 2026-11-22 更新记忆模块视图 · 2026-11-24 按 AIOS `a9951600` 复核细节（记忆核心代码与 `375ab47` 一致；主仓固定的 Rex Harness 指针已更新为 `6563c827`，工单验证基线为 `f143b9f`）。

[打开 AIOS 记忆模块内部与接口图](./architecture/aios-memory/aios-memory.html) · [Archify 可编辑源文件](./architecture/aios-memory/candidate.json)

[AIOS 记忆系统实现详解](./aios-memory-implementation.md) 展开写入授权、时效、索引重建、上下文包和召回反馈。

## 记忆模块对外接口

| 入口 | 内部处理 |
| --- | --- |
| Memo 写入 `memory_write`、`memory_checkpoint`，也可用 `aios memo add / checkpoint` | 按工作区定位。MCP `memory_write` 与 CLI `memo add` 默认均无 `runtimeIdentity`，按 `local_manual` 落为 `verified` 并加锁分配序号；置顶写入另计算呈现窗口回执（当前无启动自动注入）。 [MCP 写入](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/memory-mcp-server.mjs#L115-L161) · [CLI 默认身份](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/cli/core/run.mjs#L43-L48) |
| 统一召回 `memory_recall`、`aios search`、提问 Hook | Memo 读取带进程内 `stat` 缓存，过滤候选/失效/归档与 30 天私有 TTL 后按 BM25、实体扩散与反馈排序（分差 `< 0.2` 较新优先），并与置顶全文及 ContextDB JSONL 索引合并截取。MCP 仅返回片段与分数（不含 `eventId`）。 [召回实现](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/search/unified-search.mjs#L306-L406) · [Memo 排序](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/storage/views/query.mjs#L304-L349) |
| ContextDB CLI `session:new`、`event:add`、`checkpoint`、`search`、`context:pack` | 事件和检查点写原始会话文件并维护索引。原生检索走 SQLite，上下文包则按会话 ID 读取会话文件。 [命令分发](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/shell/cli/handlers.ts#L193-L225) |

## 跨会话接续的四种记录

跨会话接续要回答四个问题。项目有哪些已知约定，上一轮发生了什么，工单现在做到哪一步，以及这一步有什么证据。AIOS 分别用 Memo、ContextDB、计划状态和 Rex 激活与回执来回答。`jtao` 可以沿用这四种职责，用更少的存储部件做出第一版。

| 记录 | 谁写入 | 默认位置 | 如何读回 |
| --- | --- | --- | --- |
| 项目知识与约定 | Memo CLI 或 `memory_write` 追加事件，`memory_checkpoint` 追加置顶 Memo。 | `.aios/memo/`。默认 `file/events.jsonl` 和 `file/pinned/<space>.md`，也支持 `split`。 | Memo 查询或跨来源召回。 [默认存储](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/memo/storage/base/constants.mjs#L1-L6) · [文件布局](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/memo/storage/base/paths.mjs#L20-L38) · [MCP 写入](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/memory-mcp-server.mjs#L115-L161) |
| 会话轨迹与检查点 | ContextDB 写入事件、会话状态、检查点和连续性摘要。会话入口、Team 与 Solo 根据各自运行条件调用它。 | `.aios/context-db/sessions/<session_id>/`，包含 `meta.json`、`state.json`、`l0-summary.md`、`l1-checkpoints.jsonl`、`l2-events.jsonl` 和连续性摘要等。 | 按会话 ID 打包上下文，或查询事件和检查点索引。 [会话文件](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/store/core.ts#L1091-L1122) · [写入事件](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/store/core.ts#L1463-L1500) · [检查点](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/store/core.ts#L1522-L1585) |
| 工单、计划与任务进度 | AIOS 计划模块创建计划正文，更新活动状态与按工单保存的副本。 | `docs/plans/`、`.aios/planning/active.json`、`.aios/planning/plans/<work-item-id>.json`。 | 按工单 ID 打开记录，或读当前活动状态。 [路径和身份](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/contract/contract.mjs#L19-L42) · [写活动状态](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/contract/contract.mjs#L188-L198) · [恢复工单](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/contract/contract.mjs#L308-L329) |
| 能力状态与执行回执 | AIOS 适配器向 Rex 提交当前命令的证据。Rex 的 standalone store 保存工作流，回执入口记录实际执行。 | `.rex-harness/`，下有工作流、激活、证据、回执与结算记录。 | 按 `workItemKey` 或工作流 ID 找到当前命令，按 `receipt:` 引用取回回执。 [适配器](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/workflows/adapters/rex-activation-store.mjs#L281-L309) · [存储路径](https://github.com/rexleimo/rex-harness/blob/f143b9f6bb53461cf6f7fe1e3fb05a5568d6cfa3/src/standalone/store.mjs#L23-L40) · [按标识取回](https://github.com/rexleimo/rex-harness/blob/f143b9f6bb53461cf6f7fe1e3fb05a5568d6cfa3/src/standalone/store.mjs#L328-L367) |

ContextDB 和 Memo 的路径支持 `AIOS_PROJECT_STATE_DIR`。ContextDB 读取路径也能在相应兼容分支使用旧 `memory/context-db`。计划模块与 Rex 各自定义其工单目录。 [工作区状态根](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/environment/aios/state-root.mjs#L25-L55) · [ContextDB 兼容路径](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/query/paths.ts#L20-L45) · [AIOS 计划根](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/contract/contract.mjs#L19-L42)

## 写入边界

### Memo 记录值得复用的项目知识

显式 `memory_write` 解析工作区和当前存储方式，将条目写入默认空间的项目共享 Memo。写入器在锁内分配序号并追加到 `file`，或在 `split` 模式下按事件写文件。底层事件还能携带来源引用、有效时间及替代关系。 [MCP 入口](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/memory-mcp-server.mjs#L115-L132) · [事件追加](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/memo/storage/events/events-write.mjs#L159-L215)

`memory_checkpoint` 追加的是置顶 Memo。回执同时区分**已经写入**和**已经进入置顶呈现窗口**。但复核源码可见两处现存断层：一是 `memory_checkpoint`（MCP 与 CLI）只写 `.aios/memo/<storage>/pinned/default.md`，未镜像到 `index.json` 注册的旧路径 `sessions/workspace-memory--default/pinned.md`（仅 `memo pin set/add` 会镜像）；二是 `renderPinnedBlock()` 目前只服务于回执、`memo pin status`、`memo report` 和 `memo hygiene`，尚无会话启动自动渲染注入链路。 [MCP 置顶回执](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/memory-mcp-server.mjs#L141-L162) · [窗口判定](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/storage/govern/pinned.mjs#L162-L174) · [旧注册路径](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/contextdb/registry/context-registry.mjs#L24-L30)

隔离试验中，一条超出置顶窗口的记录返回 `written but NOT served`，之后仍被同工作区的 `memory_recall` 找到。主动召回直接匹配置顶文件全文（绕过窗口裁剪）。写入完成、被窗口选中、向某个宿主实际送达是三个需要分别验证的状态。 [主动匹配置顶文件](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/search/unified-search.mjs#L224-L236)

会话结束还有候选笔记路径。它从会话最后一段助手文本与变更文件产生候选文件，保存到对应会话目录并标为 `candidate`。共享记忆的晋升有单独的判定入口。 [会话结束候选](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/lifecycle/run/session-hooks/close.mjs#L79-L123) · [候选判定](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/memo/storage/govern/candidates.mjs#L283-L316)

### ContextDB 记录发生过的事和续跑线索

会话入口在需要时登记会话并刷新 `index.json`，它是可载入资料的注册表。事件和检查点先写会话目录，再更新跨会话 JSONL 索引。SQLite `index/context.db` 是可重建的检索索引。ContextDB 原生查询使用它，并在缺失或可恢复错误时尝试同步或重建。 [会话登记](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/lifecycle/run/session-hooks/start-session.mjs#L34-L75) · [注册表](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/contextdb/registry/context-registry.mjs#L8-L29) · [索引重建](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/store/core.ts#L2371-L2399)

**ContextDB `checkpoint`** 保存会话进度、下一步、产物引用和可选验证摘要，并刷新连续性文件。Team 在支持的执行模式和会话下先写 dispatch 产物，再记事件和检查点。Solo 写轮次日志与连续性摘要，满足会话条件时也生成 ContextDB 检查点。 [检查点接口](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/shell/cli/handlers.ts#L88-L113) · [Team 调度证据](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/harness/orchestration/orchestrator-evidence/persist.mjs#L18-L54) · [事件和检查点](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/harness/orchestration/orchestrator-evidence/persist.mjs#L90-L133) · [Solo 保存轮次](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/harness/solo/solo-runtime/support/state.mjs#L56-L89) · [Solo 检查点条件](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/harness/solo/solo-runtime/gate/checkpoint.mjs#L45-L83)

### 计划进度与 Rex 证据各有推进规则

AIOS 计划管理把 Markdown 计划与结构化状态一起保存，活动计划可写回任务和证据引用。正常标记 `done` 的门检查任务均为 `done` 或 `skipped`，以及计划已附证据。记录一条计划证据时，代码校验其字段和值，所以一条计划附件和一份实际执行回执具有各自的含义。 [计划完成门](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/contract/contract.mjs#L375-L394) · [完成条件](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/contract/schema.mjs#L285-L299) · [附加计划证据](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/contract/contract.mjs#L448-L461)

Rex 的能力证据绑定到当前 `activationId` 与命令令牌。指定的测试证据要求解析真实 `receipt:` 引用，校验预期退出码，并在声明了场景命令时核对命令。回执由运行时执行命令后记录，包含命令和参数、工作目录、退出码、输出哈希和观察时间。通过当前阶段的证据校验后，运行时持久化新的状态。 [命令令牌校验](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/workflows/adapters/rex-capability-runtime.mjs#L346-L372) · [回执条件](https://github.com/rexleimo/rex-harness/blob/f143b9f6bb53461cf6f7fe1e3fb05a5568d6cfa3/src/application/validate-command-evidence.mjs#L20-L54) · [实跑回执](https://github.com/rexleimo/rex-harness/blob/f143b9f6bb53461cf6f7fe1e3fb05a5568d6cfa3/src/standalone/store.mjs#L200-L240) · [证据推进](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/workflows/adapters/rex-activation-store.mjs#L281-L309)

## 读取与跨会话恢复

1. **项目召回按查询读。** `memory_recall` 调用 `searchAiosProject`。它可以合并 Memo、ContextDB、计划、文档和代码的片段。统一召回的 ContextDB 源直接读取 `index/events.jsonl`、`index/checkpoints.jsonl`。MCP 输出面向模型的来源、分数与片段（**不含 `eventId`**）。回合 Hook（`UserPromptSubmit`）调用 `collectTurnRecall()`，在非 `noop` 轮次**即使无命中**也会把 `## AIOS RECALL` 状态块与 `## AIOS MEMORY DECLARATION` 声明指令写入 `additionalContext`（Codex 无后置消费 Hook）；真正仅在有命中时注入的是 `ctx-agent` 单轮入口。 [MCP 召回](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/memory-mcp-server.mjs#L88-L107) · [JSONL 召回](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/search/unified-search.mjs#L306-L329) · [来源和排序](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/search/unified-search.mjs#L350-L406) · [回合召回与 block/text 分流](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/foundation/planning/flow/turn-recall.mjs#L271-L314) · [Hook 接入](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/foundation/planning/flow/user-prompt-submit.mjs#L29-L46)
2. **会话上下文按会话 ID 读。** ContextDB 从指定会话的元数据、摘要、事件、检查点和连续性拼装上下文包。调用方可以按事件条数、类型、引用和 token 预算限定输出。它用于理解上一轮发生了什么。 [读取会话文件](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/store/core.ts#L1651-L1674) · [上下文包参数](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/shell/cli/handlers.ts#L116-L140)
3. **工单恢复按工单 ID 读。** AIOS 精确定位结构化计划，复用时查找相关 Rex 激活。Rex 返回持久化的当前命令及工作流状态。已验证与待验证项目以当前状态和证据为准，相关 Memo 供补充业务背景。 [工单计划读取](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/contract/contract.mjs#L308-L329) · [复用时查激活](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/flow/auto-gate.mjs#L215-L232) · [恢复 Rex 命令](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/workflows/adapters/rex-activation-store.mjs#L341-L353)

一个接续场景是 Codex 已完成实现并提交了单元测试回执，浏览器验收仍待执行。下一会话先用工单 ID 得到这两项任务状态和对应证据，再根据当前目标召回相关业务约定。之前某轮的完成措辞只能作为会话背景。

## `jtao` 第一版设计草案

**目标是让新会话凭证据继续同一个工单。** 第一版覆盖显式记录与召回、按工单恢复进度、保存验证回执。下列接口与存储形态供后续设计确认。

| 职责 | 第一版可以保存的信息 |
| --- | --- |
| 项目知识 | 约定或结论、所属工作区、来源引用。新结论可带替代关系，旧记录可追溯。 |
| 会话检查点 | 客户端与会话 ID、关联工单、简短上下文、已触及文件和建议下一步。可使用同一份读取接口供不同客户端取回。 |
| 工单状态 | 工单 ID、验收条目、当前状态、下一步和版本。按工单 ID 读取，每次推进核对预期版本。 |
| 验证回执 | 工单及验收条目、实际命令、退出码、提交或内容版本和产物引用。回执在提交后可追溯，根据当前验收条件判断是否推进。 |

- **工作区身份由适配层传入。** 存储按工作区隔离，工单 ID 在工作区内定位。客户端会话 ID 只用于追踪执行来源。可保留运行时安装目录与项目目录两个字段。AIOS 的 Codex 托管写法会为 `aios-memory` 固定 `AIOS_WORKSPACE_ROOT`，而 MCP 进程优先使用这个值。 [Codex 托管配置](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/clients/native/distribution/emitters/codex-config.mjs#L93-L104) · [MCP 根解析](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/memory-mcp-server.mjs#L12-L16)
- **工单状态与证据一起推进。** 写入源可以先用每个工作区的一份 SQLite，工单进度、会话检查点与 Memo 分别建表，原始产物通过引用关联。接受回执和更新工单状态放在同一事务，重复提交同一回执得到同一结果。这个物理存储选择仍可讨论。
- **召回接口返回结构化引用，避开 AIOS MCP 的断层。** 返回来源、记录 ID（`eventId`）和片段，并配套暴露修订（`supersede`）与反馈工具（AIOS 的 MCP `memory_recall` 丢弃了 `eventId` 且缺这两类工具）；Hook 只在命中时注入上下文，不在无后置消费方的宿主里注入声明指令；置顶内容保持单一事实源。找不到相关背景时仍能按工单恢复。回执缺失或与当前版本不符时，维持现有工单状态并报告缺口。旧的 Memo 或会话摘要可提供背景，任务完成状态由当前工单和验收规则管理。

### 第一条验收场景

1. 在工作区 A 记录一条业务约定，开启一个含两项验收的工单。提交第一项的实际验证回执，保存当前会话检查点。
2. 新开 Codex 会话，按工单 ID 恢复。能拿到第一项的状态和回执、第二项仍待验证的原因、下一条动作，以及可查询的业务约定。
3. 用同一份适配契约从第二个客户端读同一工单。重试同一回执，或提交针对旧版本的回执，工单状态符合预期。切到工作区 B 查询时保持隔离。

这条验收既检查状态能否读回，也检查 Agent 有没有漏掉待办、把缺证据的事项报成已完成。

## 本轮验证

- **2026-11-24 复核更新（AIOS `a9951600`, v8.1.1）**：确认记忆核心源码与 `375ab47` 无差异；修正了文档与两张记忆图（`aios-memory`、`aios-memo-flow`）中的 Hook 无命中注入、置顶路径与启动注入断层、`memo add` CLI 身份归类、Memo 排序与缓存细节，并通过 Archify 源码引用、浏览器校验与截图目检。
- 三张早期 Archify 图均通过结构、源码引用与浏览器校验。记忆详图另核对了浏览器截图。
- Memo 存储与置顶窗口通过 22 项库级测试。独立 MCP 试验确认窗口外的条目仍可主动召回。AIOS 顶层的两项 `memo pin status` 测试在当前检出中止于缺少 `commander`。
- ContextDB 通过 4 项会话持久化与查询测试。通过已有 `tsx` 直调 ContextDB CLI，实测了会话写入以及 SQLite 侧车缺失后的重建。
- 计划合同隔离探针验证了凭工单 ID 恢复完成状态及其附件。固定版本的 Rex 通过 6 项独立 CLI 与回执验证测试。
