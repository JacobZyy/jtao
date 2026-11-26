# AIOS 记忆系统实现详解

2026-11-22 源码走读 · 2026-11-24 按 AIOS `a9951600`（记忆内核与 `375ab47` 一致）复核修正。[记忆模块内部与接口图](./architecture/aios-memory/aios-memory.html) · [记忆与持久化总览](./memory-and-persistence.md)

AIOS 让不同会话共享项目知识，同时保留每次执行的来龙去脉。Memo 存可以复用的结论和约定，ContextDB 存发生过的事件与恢复点，工单和执行回执按自己的标识持久化。召回时，系统按用途选择记录、控制上下文长度，并保留回查原始材料的路径。

## 做完一个功能，这条记忆怎样留到下次

[看记忆接入、调用和返回的完整流程图](./architecture/aios-memo-flow/aios-memo-flow.html)

假设我和 Codex 做完结账配置功能，确认**开发环境改了结账配置要重启服务**。

1. **自动记下后去哪儿。** AIOS 的 `ctx-agent` 执行单轮任务时，由完成这轮工作的 Codex 判断值不值得记，并在回复末尾提议。回复缺少肯定声明时，AIOS 跳过自动写入。成功轮次通过写入检查后，候选会写进本地 Memo 存储，默认查询会过滤它。
2. **共享池用来做什么。** 「共享池」指同一项目后续对话可以查询的正式 Memo。下次再改结账配置，可以复用这条已经确认的结论。
3. **怎样进共享池。** 可以由我说「把这条结论记成项目共享 Memo」，也可以由已接入工具的 Codex 主动发起。无论走 MCP `memory_write` 还是 CLI `aios memo add`，默认都没有 `runtimeIdentity`，都会按 `local_manual` 新建一条 `verified` 共享记录（注意：`memo/SKILL.md` 文档声称 `memo add` 也会落成 `candidate`，与实际 CLI 默认行为存在反差）。即使此前自动保存过同一句话，两次写入也各有自己的记录。
4. **怎样晋升。** 我可以对一条候选提交晋升申请，也可以让 Codex 代发。申请带候选 ID 和理由，由 AIOS 检查发布授权。当前版本会拒绝申请并写回执，候选继续待发布。代码中获准的分支会新建一条共享记录并关联原候选，当前授权入口无法到达该分支。
5. **什么时候查回来。** 下次在同一项目可以说「先查一下结账配置的记忆」。发起查询后，符合可见范围的记录参加关键词匹配、实体扩散和排序。共享 Memo 命中并进入返回结果时，Codex 才会在这轮拿到它。

### Codex 怎样获得并使用记忆

| 途径 | 调用与返回 |
| --- | --- |
| **提问 Hook** | 配置生效后，`UserPromptSubmit` 将问题交给 AIOS，Hook 调用 `collectTurnRecall()` 直接查记忆。**只要本轮决策不是 `noop`，即使无命中（`hits: 0`）或出错，Hook 也会返回 `## AIOS RECALL` 状态块（含 `ccrg` 状态）并拼接 `## AIOS MEMORY DECLARATION` 声明指令写入 `additionalContext`**。由于 Codex 仅配置了提问 Hook、没有回复后解析 Hook，原生 Codex 回复末尾输出的 `<!--memory: ... -->` 当前无人消费。 [Hook 配置](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/client-sources/native-base/codex/project/hooks.json#L1-L14) · [返回路径](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/foundation/planning/flow/user-prompt-submit.mjs#L34-L46) · [始终返回状态块](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/foundation/planning/flow/turn-recall.mjs#L308-L314) |
| **Skill、项目指令和工具说明** | `memo` Skill 约定五要素（`fact` / `entities` / `date` / `evidence_ref` / `confidence`）及 CLI 用法（文档称 `memo add` 也落候选，实际 CLI 默认直接落 `verified`）。`AGENTS.md` 约定开聊、继续工作或确认结论时的动作。已注册的 MCP 用 `tools/list` 提供工具名称、用途和参数。当轮 Codex 结合这些信息决定实际调用。 [Memo Skill](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/skill-sources/memo/SKILL.md#L19-L99) · [项目指令](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/AGENTS.md#L265-L278) · [工具列表及分发](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/memory-mcp-server.mjs#L174-L185) |
| **MCP 和 CLI** | Codex 通过本地 stdio MCP 调 `memory_recall`，收到格式化后的来源、分数和文本片段（**不含 `eventId`**，且 MCP 未暴露 `useful` / `supersede` 工具）。调 `memory_write` 只接收 `text`（和可选 `agent`），新建共享 Memo 并返回记录 ID。CLI `aios memo add` 额外校验五要素、输出相似旧记录 `--supersedes` 提示，并镜像到旧会话 `workspace-memory--default`；`aios memo search` 随 stdout 返回含 `eventId` 的文本行与体积统计。 [MCP 配置](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/clients/native/distribution/emitters/codex-config.mjs#L93-L103) · [查询响应](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/memory-mcp-server.mjs#L88-L107) · [CLI 读写](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/cli/commands/ops/events.mjs#L73-L120) · [CLI 输出](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/cli/commands/ops/events.mjs#L193-L226) |
| **AIOS 单轮执行** | 普通单轮的 `ctx-agent` 入口读取 `collectTurnRecallResult().text`，**仅在有命中时**拼进提示并附声明指令；当轮 Codex 返回肯定声明后，AIOS 解析声明并执行自动写入与 `useful` 反馈回写。 [仅命中文本](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/foundation/planning/flow/turn-recall.mjs#L271-L275) · [提示拼装](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/foundation/ctx-agent-core/entry/run.mjs#L367-L374) · [自动写入入口](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/foundation/ctx-agent-core/entry/run.mjs#L409-L440) |

图中追踪的是 Memo 记录。统一召回也可以从 ContextDB 独立返回历史对话片段，返回结果会标注来源。 [统一搜索](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/search/unified-search.mjs#L355-L375) · [结果格式](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/memory-mcp-server.mjs#L88-L107)

## 实现架构

| 职责 | 实现与持久化 | 常用读法 |
| --- | --- | --- |
| **项目知识** | Memo 默认追加到 `.aios/memo/file/events.jsonl`，也支持 `split` 后端按空间和事件分文件。每条事件有 ID、空间、来源、作用域和可选替代关系。置顶内容另存 `file/pinned/<space>.md`。 | Memo 根据身份和时间筛选事件，再做文本匹配和排序。主动召回也会检索完整的置顶文件。 [文件布局](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/memo/storage/base/paths.mjs#L12-L49) · [Memo 查询](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/memo/storage/views/query.mjs#L276-L357) |
| **会话轨迹** | ContextDB 按 `sessionId` 保存文件。L2 是事件流水，L1 是带状态、下一步、产物引用和验证信息的检查点，L0 是从最新检查点生成的 Markdown 摘要。另有会话元数据、当前状态和连续性摘要。 | 用会话 ID 导出上下文包，或者通过查询索引查跨会话事件与检查点。 [会话布局](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/store/core.ts#L1091-L1122) · [检查点写入](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/store/core.ts#L1520-L1585) |
| **查询索引** | ContextDB 同时维护 `index/{sessions,events,checkpoints}.jsonl` 和使用 Node `node:sqlite`、FTS5 的 `index/context.db`。根目录的 `index.json` 列出交接摘要、置顶视图等可供读取的项目上下文。 | 统一召回读 JSONL 中的事件和检查点。ContextDB 原生检索走 SQLite，可从会话文件追齐或重建。 [统一检索](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/search/unified-search.mjs#L306-L329) · [SQLite 表](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/sqlite/tables/schema.ts#L132-L142) · [上下文清单](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/contextdb/registry/context-registry.mjs#L8-L38) |
| **工单状态与回执** | 计划持久化到 `.aios/planning/active.json` 及 `.aios/planning/plans/<id>.json`。执行侧另存 Rex 激活、当前命令和对应证据。会话检查点可以附上与两者有关的产物引用。 | 按工单 ID 恢复计划，按激活和回执读取执行状态。计划的完成判断读取任务状态及已登记的 evidence。 [工单文件](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/contract/contract.mjs#L19-L43) · [计划完成条件](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/planning/contract/schema.mjs#L285-L299) · [执行恢复](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/workflows/adapters/rex-activation-store.mjs#L291-L353) |

表中路径为默认布局。MCP 通过 stdio JSON-RPC 暴露 `memory_recall`、`memory_write`、`memory_checkpoint`，使用 `AIOS_WORKSPACE_ROOT` 或进程工作目录。MCP 的 `memory_checkpoint` 写的是 **Memo 置顶条目**。ContextDB CLI 的 `checkpoint --session …` 写的是 **该会话的 L1 检查点**，并同步更新 L0 和连续性摘要。 [状态目录](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/environment/aios/state-root.mjs#L21-L55) · [MCP 接口](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/memory-mcp-server.mjs#L12-L16) · [MCP 置顶写入](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/memory-mcp-server.mjs#L141-L162) · [ContextDB 命令](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/mcp-server/src/contextdb/shell/cli/handlers.ts#L88-L140)

## 写入、发布与失效

### 模型判断值得记什么，写入入口决定可见性

AIOS 的 Memo Skill 把是否值得记交给当轮 Agent。已确认的根因、经过验证的修复和可复用的决策规则是它给模型的例子。模型可以写下轮末声明，也可以主动调用显式记忆工具。自动路径和 MCP 路径各自应用写入规则。 [判断依据](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/skill-sources/memo/SKILL.md#L47-L53) · [MCP 工具描述](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/memory-mcp-server.mjs#L58-L68)

`ctx-agent` 的单轮执行入口读取回答尾部的记忆声明。`verified=yes` 才触发自动写入，`conclusion` 提供待记的结论。自动路径以会话和轮次构造来源引用，检查内容，再按源引用在有限范围查重。成功轮次的共享写入带有运行时身份但缺少共享发布能力，因此落成 `candidate`。同一函数收到失败或阻塞结果时，默认选择 `agent_private`。会话关闭还可以在会话目录留一份待审草稿。 [轮次入口](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/ctx-agent-core/entry/run.mjs#L397-L440) · [自动写入](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/memo/flow/autopilot.mjs#L182-L275) · [会话关闭草稿](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/lifecycle/run/session-hooks/close.mjs#L79-L123)

当前提交有两种实际行为。候选管理入口可以列元数据；正常候选晋升请求会因 `trusted_authority_unavailable` 被拒绝并留下回执。显式 MCP `memory_write` 与 CLI `aios memo add`（`run.mjs` 默认 `runtimeIdentity = null`）则都沿用本地手工写入规则，记录为 `local_manual` 和 `verified`，可立即参与检索（与 `memo/SKILL.md` 中“ `memo add` 落为 `candidate`”的说明存在偏差）。此外，CLI `memo add` 支持五要素格式校验（`--date` 拒绝相对时间、`--evidence-ref` 拒绝数组）并在写入后提示可 `--supersedes` 的相似旧条目；MCP `memory_write` 仅接受 `text` 与可选 `agent`。因此读取结论时要结合来源字段，是否跑过测试、验收了哪个版本仍须查执行回执。 [候选授权](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/storage/govern/candidates.mjs#L181-L237) · [拒绝回执](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/storage/govern/candidates.mjs#L283-L325) · [MCP 写入](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/memory-mcp-server.mjs#L115-L132) · [CLI 默认身份与校验](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/cli/core/run.mjs#L43-L48) · [五要素规范化](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/storage/mind/extraction.mjs#L39-L90) · [来源归类](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/storage/govern/provenance.mjs#L47-L95)

### 一条知识可以过时，也可以按旧时间查回

写入持有工作区 Memo 锁，校验允许替代的目标、分配序号，再追加事件。新事件以 `supersedes` 指向旧 ID，读取时按新事件的 `validAt` 推导旧事件的 `invalidAt` 和 `supersededBy`。例如 16:00 生效的新约定能让 15:59 的查询读到旧约定，而 16:00 的查询已读到新约定。候选的替代关系暂时无效，跨空间和非法的私有替代会被拦下。普通查询过滤已失效事件，`asOf` 按有效时间查询，`includeInvalid` 可带回历史记录。 [写入与锁](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/memo/storage/events/events-write.mjs#L159-L215) · [有效期计算](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/memo/storage/govern/temporal.mjs#L69-L154)

这一点适合经常变动的项目约定。一条修订能指明替代了哪条约定、何时生效和依据在哪里。只有来源、作用域和时效都符合条件的内容才应进入当前视图。

## 三条读取路径

### 主动查询与轮次召回

1. **显式查项目。** `memory_recall` 和 `aios search` 调用统一检索，默认覆盖 Memo、ContextDB、计划、文档、代码。Memo 读取先走基于文件 `stat` 的进程内解析缓存，再过滤候选、私有范围、有效期、Dream 归档和默认 30 天的 `agent_private` 读时 TTL，随后按原文、分词、引用与实体匹配，用 BM25、实体直接匹配（最高 `+1.0`）与一跳共享实体扩散（最高 `+0.5`）加上反馈权重排序。分差小于 `0.2`（`MATCH_SCORE_EPSILON`）即按时间较新优先。跨来源再合并打分和截取。MCP 当前只输出 `[source] (score) text` 片段，**未返回 `eventId`**；底层与 CLI 结果会携带事件 ID 与引用。 [读时 TTL 与实体扩散](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/storage/views/query.mjs#L41-L60) · [Memo 排序](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/storage/views/query.mjs#L237-L357) · [跨来源查询](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/search/unified-search.mjs#L332-L406) · [MCP 返回](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/memory-mcp-server.mjs#L88-L107) · [CLI JSON](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/search/cli.mjs#L22-L39)
2. **有 Hook 或相应运行入口时自动查。** `turn-recall` 依次尝试原始问题、ICU 提取的内容词和含中文双字片段的词项，一层无结果才放宽。只查 Memo、ContextDB 和计划，最多取六条，单条展示进一步裁剪。这里有两条不同的注入行为：`UserPromptSubmit` Hook 调用 `collectTurnRecall()`，返回始终非空的 `result.block`——只要本轮决策不是 `noop`，**即使 `hits: 0` 也会写入 `additionalContext` 并附带 `## AIOS MEMORY DECLARATION` 指令**；而 `ctx-agent` 单轮入口调用 `collectTurnRecallResult()` 读取 `.text`，才是真正的**仅命中时注入**。查询失败时两者都放行用户轮次。 [查询降级](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/foundation/planning/flow/turn-recall.mjs#L151-L168) · [召回窗口与 block/text 分流](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/foundation/planning/flow/turn-recall.mjs#L271-L314) · [Hook 注入](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/foundation/planning/flow/user-prompt-submit.mjs#L29-L46)

这条主路径运行本地词法检索。Memo 库预留可注入的 embedding 接口，仓内可选实现 `hash-lexical` 是无模型的词项哈希向量，只用于扩充候选池。ContextDB 可选的 `semantic` 实现按词项重合重排。 [Memo 可选 embedding](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/storage/mind/embedding.mjs#L13-L45) · [ContextDB 可选重排](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/mcp-server/src/contextdb/query/semantic.ts#L76-L125)

### 置顶呈现

项目长期规则与里程碑都可以落在置顶文件。渲染时，先尝试安排人工长期规则，再用剩余空间从新到旧选一段连续的检查点，最终按文件顺序输出。写入回执会用同一套算法判断新条目是否完整进入窗口。放不进窗口的条目仍可用关键词查原始文件。

需要注意当前源码中的两处实现断层：一是 `memory_checkpoint`（MCP 与 CLI）只写入 `.aios/memo/<storage>/pinned/<space>.md`，**未同步镜像**到 `context-registry.mjs` 注册的旧路径 `{contextDb}/sessions/workspace-memory--{space}/pinned.md`（只有 `memo pin set/add` 会镜像）；二是 `renderPinnedBlock()` 目前只服务于写入回执、`memo pin status`、`memo report` 和 `memo hygiene`，**尚无 `SessionStart` / `UserPromptSubmit` / `ctx-agent` 启动自动渲染注入链路**，实际消费置顶内容依赖主动搜索全文（绕过窗口）、`memo pin show`、Console 面板和 Dream 分类。 [置顶选择与回执](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/storage/govern/pinned.mjs#L87-L174) · [检索完整置顶](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/search/unified-search.mjs#L224-L236) · [旧注册路径](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/contextdb/registry/context-registry.mjs#L24-L30) · [pin 镜像写](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/cli/commands/curate/pin.mjs#L64-L79)

### 按会话恢复

`context:pack` 根据会话 ID 组装 L0 摘要、最近检查点、后续动作、事件窗口和工具输出外置引用（`Offload References`）。默认 `smart` 策略保留近期事件，再从一段近期记录中挑与目标、检查点、下一步及错误有关的事件，最后按时间顺序呈现。它还可以从索引中找相关的历史检查点。 [事件选择](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/mcp-server/src/contextdb/store/core.ts#L1651-L1794)

收到 `--token-budget` 时，预算约束的是事件窗口。默认的平衡策略倾向于保留关键错误、动作和带引用的事件，并记录压缩、舍弃和截断了多少条。上下文包还可附大型材料的索引和 `Offload References` 引用，在需要时再读取原始内容。从上一次的“验收到哪里”钻到“哪条命令或产物支持”，通常只需沿检查点里的引用读取。 [事件预算算法](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/mcp-server/src/contextdb/store/core.ts#L746-L891) · [上下文包输出](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/mcp-server/src/contextdb/store/core.ts#L1848-L1882)

## 两处值得借鉴的实现取舍

### 检索索引可以丢，原始记录可以重放

ContextDB 写入事件时先按会话加锁，短时间内连续重复的事件会复用已有结果。随后追加会话日志、更新状态与 JSONL 索引，再尝试更新 SQLite。查询前可从新增会话或日志尾部做增量同步。SQLite 文件缺失或遇到可恢复错误时，搜索使用会话元数据及 L1、L2 重建索引。这种分工允许用文件审计历史，用数据库加速查询。Memo 读取侧已内置基于文件 `stat`（`size+mtimeMs+ctimeMs`）的进程内解析缓存（最多 50 个文件），长驻进程不必重复读盘和解析 JSONL，但过滤、BM25 和实体扩散仍是全量内存扫描，记录量增长时需要关注扫描开销。 [事件写入](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/mcp-server/src/contextdb/store/core.ts#L1424-L1501) · [增量同步与重建](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/mcp-server/src/contextdb/store/core.ts#L2318-L2450) · [进程内解析缓存](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/storage/events/events-read.mjs#L25-L89) · [Memo 查询](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/storage/views/query.mjs#L304-L349)

### 召回有命中与采纳两种信号

轮次召回对进入上下文的 Memo 事件记一条 `impression`。在能读取轮末声明的成功执行入口（`ctx-agent`），Agent 可以声明 `useful=<eventId>`，程序只接收本轮真实召回过的 Memo ID。日志按事件 ID 聚合。累计至少五次出现且零次被采纳，分数乘 `0.6`。每次明确采纳加 `1.5`，累计最多加 `4.5`。这是便宜且可解释的排序信号，反映 Agent 自报的采用情况。但需注意：显式 MCP `memory_recall` 当前不返回 `eventId`、也未提供 `memory_useful` 工具（Pi 扩展另暴露了 `aios_memory_useful`），而原生 Codex 对话也没有解析回复末尾 `useful=` 的后置 Hook，因此纯 MCP / 原生 Codex 路径当前无法闭合这条反馈链。 [曝光记录](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/foundation/planning/flow/turn-recall.mjs#L271-L293) · [采纳 ID 交集](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/foundation/planning/flow/turn-recall.mjs#L170-L186) · [成功执行回写](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/foundation/ctx-agent-core/entry/run.mjs#L455-L475) · [反馈与计分](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/scripts/lib/memory/memo/storage/mind/feedback.mjs#L56-L87) · [Pi 扩展工具](https://github.com/rexleimo/aios/blob/a995160096ffc9b1a81f318dc05195b6b6b68bb8/packages/aios-pi/lib/tools.mjs#L54-L64)

维护能力放在常规读写之外。Hygiene 检查陈旧会话、过长置顶和大型日志，给出清理项。可选的自动 Dream 默认关闭，开启后按会话关闭或空闲条件产出整理预览。 [Hygiene 检查](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/memo/care/hygiene.mjs#L133-L196) · [自动预览开关](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/memory/memo/flow/autodream-auto.mjs#L13-L70)

## 一次接续如何用到这些记录

以「调整支付重试策略」为例。上一个会话改完代码，跑过单元测试，浏览器验收待执行。

1. ContextDB 记录本轮事件，L1 检查点保存现阶段、下一步和单测产物引用，L0 为下一会话提供概览。
2. 工单以自己的 ID 持久化验收条目和已有回执。新会话打开同一工单时，以当前验收状态决定先做哪一步。
3. 「支付重试必须复用幂等键」可以进入项目 Memo，作为跨工单适用的约定。规则修订时追加带来源和生效时间的记录。
4. 新会话可以先主动召回约定，再按需取上个会话的上下文包或展开具体证据。当前代码版本下的测试结果仍由对应回执说明。

当 `ctx-agent` 在执行要求 Rex 证据的命令时，先从原始执行输出收集证据，再压缩用于呈现的内容。所需证据未被接收时，即使进程退出码为零，这一轮仍记为阻塞。这样压缩后的会话内容可以服务于阅读，推进依据仍能回到原始输出与结构化回执。 [证据采集顺序](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/foundation/ctx-agent-core/entry/run.mjs#L391-L408)

## 对 `jtao` 的直接启发

第一版可以先做三种可验证的读写。**项目结论按来源和有效时间查询，会话按 ID 接续，任务按工单 ID 和验收回执恢复。** 同一条内容可以同时有关联，但这三种读法各有自己的依据。

- **避开 AIOS 自身的四处断层**：
  1. 提问 Hook 只在有命中时注入上下文，不在没有后置消费方的宿主里硬塞 `<!--memory: ... -->` 声明指令；
  2. MCP 召回必须把 `eventId` 与来源引用透出给模型，并配套暴露 `supersede` 和 `useful` 工具，避免修订与反馈在 MCP 路径断裂；
  3. 置顶内容与启动注入只维护单一事实源，不留两套路径（新版 pinned 与旧 `workspace-memory`）各自演进；
  4. 不照搬「自动写入落候选 + 晋升授权永远拒绝」的半成品，第一版要么从显式可追溯写入起步，要么给草稿提供本地可走通的一键确认发布。
- 先让检查点说清做了什么、现在卡在哪里、下一步是什么和证据在哪。按需选取事件与证据，再衡量上下文包是否需要更复杂的排序。
- 本地存储可以从简单文件或一份带事务的 SQLite 起步。AIOS 的双索引、Tool Offload、Persona/Role 记忆、可选图检索和 Dream 整理流程适合在实际出现对应需求时再评估。物理存储和接口名仍是 [`jtao` 设计草案](./memory-and-persistence.md#jtao-第一版设计草案) 的待定项。

## 本轮验证

- **2026-11-24 全量源码复核（AIOS `a9951600`, v8.1.1）**：确认记忆内核代码与 `375ab47` 零差异；逐文件核实并修正了 Hook 无命中注入、置顶窗口与旧路径断层、`memo add` CLI 与 Skill 反差、Memo 排序平局阈值（`< 0.2`）、30 天私有 TTL、实体一跳扩散、进程内 stat 缓存及 MCP 缺 `eventId` 等细节。两张记忆架构图（[记忆模块内部与接口](./architecture/aios-memory/aios-memory.html)、[记忆调用与 Memo 流转](./architecture/aios-memo-flow/aios-memo-flow.html)）已按 `a9951600` 重新通过 Archify 源码引用、浏览器自动化与截图目检。

- 调用路径展开后，Hook 和召回相关的 12 项定向用例通过。临时工作区实测 stdio MCP 的 `tools/list`、写入回执、召回结果，并调用 CLI 命令处理器核对了 `memo search` 的标准输出。
- 合并版流程图通过源码引用和浏览器检查。隔离验证使用 AIOS `375ab47` 和主仓固定的 Rex 版本，24 项定向用例通过，包含用固定 Codex 回复执行的单轮自动写入测试。两项顶层 CLI 用例因验证拷贝缺少 `@hono/node-server` 中止于启动。
- 用结账配置的例子调用实际的记忆写入逻辑、晋升命令处理器和 stdio MCP。自动候选写入本地文件且被共享查询过滤，晋升写下拒绝回执，显式写入生成一个新的共享记录 ID。另一 MCP 进程可以查回新记录。

- 48 项定向源码测试通过，覆盖 Memo 的可见性、替代时效、候选拒绝、并发写入、轮次召回，以及 ContextDB 的事件去重、索引同步和上下文包裁剪。
- 同一临时工作区调用实际 stdio MCP，确认显式写入是可召回的 `verified`，运行时自动写入的共享 `candidate` 被默认召回过滤掉。另验证了反馈日志让示例得分从 `6` 变为 `3.6`，一次采纳后为 `7.5`。
- ContextDB 原仓重建用例的子进程写死了仓内 `node_modules/tsx` 路径，当前检出缺少该依赖。用现有 `tsx` 直接运行同一 CLI，移开临时 SQLite 文件后，实测从原始会话重建索引并查回事件和检查点。验证范围是当前检出的代码路径和临时工作区。
