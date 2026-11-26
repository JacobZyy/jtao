# AIOS 仓库架构归档

2026-11-21 · 第一版总览 · [打开交互图](./aios-overview.html) · [Archify 可编辑源文件](./candidate.json)

**基线**　AIOS `375ab47`，主仓记录的 Rex Harness `f143b9f`。生成时本地子模块实际检出 `8011956`，子模块内工作区干净。主图引用主仓提交，子模块内部证据单独引用主仓所固定的版本。

AIOS 把已有编码客户端接到本机工作流、执行编排、项目记忆和验证证据上。Rex Harness 决定工程能力及推进条件，AIOS 宿主负责把决定接进客户端和项目。Console 是管理入口。模型网关提供经客户端配置启用的请求路径。 [项目入口与子模块边界](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/README-zh.md#L98-L122) · [Console 产品面](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/apps/shell/README.md#L1-L10) · [网关接入](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/tooling/model-router/entry/command.mjs#L247-L272)

## 模块与职责

| 模块 | 功能与边界 | 主要入口和证据 |
| --- | --- | --- |
| 客户端接入与能力分发 | 检测和适配 Codex、Claude、Gemini、OpenCode、Hermes、Grok、WorkBuddy、Pi、ZCode、Qoder。按客户端能力写项目指引、钩子和 MCP 配置，同步 Skills、Subagents 与 Rex 工作流入口。具体接入和无人值守执行能力因客户端而异。 | [客户端范围](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/README-zh.md#L150-L156) · [init 落点](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/aios-init.mjs#L191-L223) · [原生同步](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/clients/native/synchronize/sync/run.mjs#L44-L95) |
| CLI 与 MCP | `scripts/aios.mjs` 解析 CLI 和工作区参数，分发 `init`、`doctor`、`plan`、`work`、`team`、`harness`、`memo`、`model-router` 等。MCP 为 Agent 提供计划和记忆操作入口。 | [CLI 装配](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/aios.mjs#L5-L27) · [工作流 MCP](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/aios-mcp-server.mjs#L730-L744) · [CLI 记忆与网关分发](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/tooling/cli/dispatch/dispatch.mjs#L432-L439) |
| 工作流策略与计划 | 选择直接、受控、计划等路径，决定使用单客户端、Team 或 Harness。维护活动计划和工单身份，把适用的 Rex 能力激活关联到工单或当前轮次。 | [策略计算](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/contract/workflow-policy.mjs#L317-L453) · [条件落盘](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/flow/auto-gate.mjs#L179-L261) |
| Rex Harness | 独立 Git 子模块。把显式意图与观察转换为事实，给出 capability、当前 command、provider 和所需证据。公开入口支持激活、推进和独立持久化，父仓适配器负责宿主绑定。 | [能力判定](https://github.com/rexleimo/rex-harness/blob/f143b9f6bb53461cf6f7fe1e3fb05a5568d6cfa3/src/application/evaluate-request.mjs#L5-L30) · [公开 API](https://github.com/rexleimo/rex-harness/blob/f143b9f6bb53461cf6f7fe1e3fb05a5568d6cfa3/src/index.mjs#L59-L90) · [宿主绑定](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/workflows/adapters/rex-harness-adapter.mjs#L85-L117) |
| Team / Solo 执行 | `work` 复用 orchestrator。Team 按任务和角色分派，执行器调用已支持的客户端。Solo 逐轮调用 Agent 并保存检查点、连续性和运行摘要，支持显式恢复。 | [work 入口](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/lifecycle/run/work.mjs#L4-L32) · [客户端执行门](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/harness/team/subagent-runtime/run/one-shot-runner.mjs#L40-L88) · [Solo 恢复](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/lifecycle/run/harness/execution/commands.mjs#L275-L333) |
| ContextDB / Memo | ContextDB 管会话、事件、检查点及索引。Memo 管可回用的项目记录及置顶条目。统一检索可查 memo、会话和计划，也可按选项纳入文档与代码。 | [事件存储](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/store/core.ts#L1470-L1500) · [Memo 写入](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/memory-mcp-server.mjs#L115-L151) · [统一检索](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/search/unified-search.mjs#L332-L404) |
| AIOS Console | Next.js 提供本机管理界面，Tauri 壳复用或启动服务。界面包含客户端、网关、记忆、MCP、Skills 和 Subagents 等能力，部分状态直接从工作区读取，管理动作复用 CLI。 | [桌面壳](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/apps/shell/src-tauri/src/lib.rs#L92-L118) · [事实采集](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/apps/shell/ui/src/lib/data/console-facts.mjs#L183-L198) · [管理命令](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/apps/shell/ui/src/app/api/model-routing/gateway/route.ts#L56-L65) |
| 模型网关 | `model-router gateway` 启动本机 HTTP 服务。接入客户端可走 Claude Messages、OpenAI Responses / Chat Completions、Gemini 等协议。网关按目标与策略选择上游、转换请求与响应，并写决策日志。 | [网关启动](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/tooling/model-router/entry/command.mjs#L221-L240) · [协议路由](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/tooling/model-router/gateway/compatibility/proxy.mjs#L445-L451) · [请求上游](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/tooling/model-router/gateway/compatibility/upstream.mjs#L125-L163) |

## 四条关键调用链

### 工程意图 → Rex 能力命令 → 证据推进

客户端通过 `aios plan auto-gate` 或 `aios_plan_auto_gate` 提交意图、工作区和会话信息。宿主 `evaluateWorkflowPolicy` 先处理显式直达、确认和恢复分支，工程请求经适配器进入 Rex。Rex 根据事实返回能力判定，宿主据此确定执行方式与是否持久化计划。 `planned` 按工单身份创建或复用计划，`guarded` 可以绑定能力激活。`dry-run` 跳过计划与激活写入。 [MCP 入口](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/aios-mcp-server.mjs#L730-L744) · [宿主决策](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/contract/workflow-policy.mjs#L317-L453) · [激活与计划落盘](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/flow/auto-gate.mjs#L179-L261)

Rex 当前命令携带指定 Provider 和证据要求。宿主在推进前检查提交关联的命令和激活身份，通过 Rex 校验证据与回执，然后持久化推进结果。阶段归属、新增需求判断和证据合同适合沿 Rex 公开入口继续下钻。 [绑定 Provider](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/workflows/adapters/rex-harness-adapter.mjs#L85-L117) · [证据推进](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/workflows/adapters/rex-activation-store.mjs#L271-L309) · [能力与工作流入口](https://github.com/rexleimo/rex-harness/blob/f143b9f6bb53461cf6f7fe1e3fb05a5568d6cfa3/src/index.mjs#L8-L14)

### 工作项 → Team / Solo → 运行产物

- **Team**　`work` 归一化参数后进入 orchestrator。一次性执行器检查客户端支持情况及可执行文件。编辑型任务带工单引用且有路径归属时，Rex 绑定失败会触发对应启动门。调度证据包含 dispatch 产物，具备适用模式与会话时写入 ContextDB 事件和检查点。 [work 参数传递](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/lifecycle/run/work.mjs#L4-L32) · [任务绑定门](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/harness/team/subagent-runtime/phases/phase-rex.mjs#L71-L97) · [调度证据](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/harness/orchestration/orchestrator-evidence/persist.mjs#L26-L132)
- **Solo**　`harness run` 和 `resume` 进入同一轮次循环。每轮调用 Agent，随后保存轮次与连续性摘要，并尝试向活动计划回写任务和证据。显式 `resume` 读取已有运行摘要，恢复已有会话及运行上下文。 [运行入口](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/lifecycle/run/harness/execution/commands.mjs#L205-L223) · [轮次执行](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/harness/solo/solo-runtime/loop/loop.mjs#L298-L318) · [落盘与计划回写](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/harness/solo/solo-runtime/loop/loop.mjs#L417-L440) · [恢复入口](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/lifecycle/run/harness/execution/commands.mjs#L275-L333)

### 会话与项目记忆 → 按需召回

`memory_recall` 调用统一搜索。`memory_write` 追加项目 memo，`memory_checkpoint` 追加置顶 memo。ContextDB 的检查点是另外一条写入路径，保存会话状态、摘要与连续性，SQLite 用作可重建的索引。调用方可以拿到跨日志和资料源的搜索结果，后续要验证的边界是各客户端何时触发召回、预算如何影响注入、哪些写入在下一轮可见。 [MCP 召回](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/memory-mcp-server.mjs#L88-L99) · [两种 Memo 写入](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/memory-mcp-server.mjs#L115-L154) · [ContextDB 检查点](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/store/core.ts#L1539-L1585)

### 控制台管理 → 客户端接入 → 网关请求

Console 用工作区 key 解析项目根，再采集这个工作区的会话、记忆和资源。网关管理走 Console 服务根上的 `model-router` 命令。启停及改写客户端配置要求显式确认，接入还核对监听端口和注册表版本。CLI 默认走就地接入，先保存备份和托管字段，再更新客户端文件，断开时按记录恢复。 [工作区选择](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/apps/shell/ui/src/lib/console-facts-request.mjs#L14-L31) · [管理接口](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/apps/shell/ui/src/app/api/model-routing/gateway/route.ts#L118-L153) · [就地接入写入](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/tooling/model-router/connect/inplace.mjs#L91-L113) · [断开还原](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/tooling/model-router/connect/inplace.mjs#L149-L165)

接入后，实际模型请求由客户端发往网关。网关使用明确目标或配置规则选路，对适用请求做意图分类及同任务续轮复用，再向配置的上游发请求。编排器启动哪个客户端、工程能力推进到哪一步、一次 HTTP 请求使用哪个模型，是三个独立的决定，分别落在执行宿主、Rex 和网关。 [网关候选链](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/tooling/model-router/gateway/router.mjs#L34-L113) · [意图与续轮](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/tooling/model-router/gateway/compatibility/proxy.mjs#L284-L333) · [上游 I/O](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/tooling/model-router/gateway/compatibility/upstream.mjs#L125-L163)

## 本地状态与写入者

| 状态 | 默认位置 | 主要写入者与用途 |
| --- | --- | --- |
| 计划进度 | `docs/plans/`、`.aios/planning/` | 计划合同写 Markdown 正文和活动状态，执行运行时可回写任务与证据。 [计划路径](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/contract/contract.mjs#L19-L29) · [写入计划](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/contract/contract.mjs#L265-L285) |
| Rex 工作流、激活和回执 | `.rex-harness/` | Rex standalone store 写能力状态和工单关联，通过公开接口供宿主读取和推进。 [Rex 路径](https://github.com/rexleimo/rex-harness/blob/f143b9f6bb53461cf6f7fe1e3fb05a5568d6cfa3/src/standalone/store.mjs#L23-L39) · [工作流写入](https://github.com/rexleimo/rex-harness/blob/f143b9f6bb53461cf6f7fe1e3fb05a5568d6cfa3/src/standalone/store.mjs#L107-L129) |
| ContextDB 会话与运行证据 | `.aios/context-db/` | ContextDB 保存事件和检查点，Team 调度产物及 Solo worker journal 在会话目录下。 [会话存储根](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/environment/aios/state-root.mjs#L34-L47) · [调度产物](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/harness/orchestration/orchestrator-evidence/shared.mjs#L25-L31) · [Solo journal](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/harness/solo/solo-journal/plumbing/paths.mjs#L18-L30) |
| 项目 memo | `.aios/memo/` | CLI / MCP 追加项目记录和置顶条目，可用于跨会话召回。 [Memo 根](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/environment/aios/state-root.mjs#L54-L59) · [MCP 写入](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/memory-mcp-server.mjs#L115-L151) |
| 网关注册表与运行状态 | 服务根 `memory/specs/model-registry.json`、本机 `~/.aios/gateway/` | 注册表保存模型配置，缺失时读内置模板。网关维护决策日志，接入命令维护客户端配置及备份。 [注册表](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/tooling/model-router/entry/registry.mjs#L12-L47) · [网关状态目录](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/tooling/model-router/entry/command.mjs#L221-L235) · [决策日志路径](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/tooling/model-router/session/decision-log.mjs#L10-L13) |

## 其他仓库组成

- **浏览器与调试**　`mcp-server/src/index.ts` 暴露 Playwright 浏览器兼容工具，`mcp-server/src/contextdb/` 同时存放 ContextDB 的 TypeScript 实现。`packages/debug-hub` 提供调试日志与结构化证据收集。 [浏览器 MCP 工具](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/index.ts#L26-L47) · [ContextDB 写入](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/store/core.ts#L1539-L1585) · [Debug Hub](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/packages/debug-hub/README.md#L1-L21)
- **Pi 原生扩展**　`packages/aios-pi` 将 AIOS 搜索和记忆包装成 Pi 工具，并对接 Pi 自身事件。 [Pi 工具与事件](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/packages/aios-pi/README.md#L32-L51)
- **学习实验、网站和发布**　`src/rl` 汇集学习子域的公开接口。`docs-site`、`blog-site` 分别构建文档站和博客。发布工作流递归取子模块并检查相关包。 [RL 入口](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/src/rl/index.mjs#L1-L17) · [文档站配置](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mkdocs.yml#L1-L8) · [博客配置](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mkdocs.blog.yml#L1-L8) · [发布检查](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/.github/workflows/release.yml#L114-L148)

## 已核实的接口错配

主仓 `aios standards` 和 `scripts/aios-standards.mjs` 都向 Rex 公开入口请求 `runStandardsArgs`。当前检出的 `8011956` 下运行 `node scripts/aios-standards.mjs --root . --rule directoryEntries`，在模块加载时报 `does not provide an export named 'runStandardsArgs'`，退出码为 `1`。主仓记录的 `f143b9f` 的公开入口也缺少这一导出。 [CLI 转发](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/tooling/cli/dispatch/dispatch.mjs#L424-L429) · [脚本转发](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/aios-standards.mjs#L11-L17) · [固定版本公开入口](https://github.com/rexleimo/rex-harness/blob/f143b9f6bb53461cf6f7fe1e3fb05a5568d6cfa3/src/index.mjs#L1-L90)

## 后续下钻索引

|主题|入口与要回答的问题|
|---|---|
|工程状态机|`auto-gate` → `rex-harness-adapter` → Rex workflow runtime。各 capability 接受什么事实，谁提供证据，哪个条件推进到下一阶段？|
|执行与恢复|`work` / `team`、`harness run` / `resume`。工单如何变成任务、子进程和检查点，异常后从哪里继续？|
|上下文与记忆|`session`、ContextDB store、Memo 及统一检索。从会话事件到下一轮注入，哪些数据按需取回？|
|网关与控制台|`model-router connect`、Console 管理 API、协议代理。配置写入、接入确认、请求选路与日志如何对应？|

