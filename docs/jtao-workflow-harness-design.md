# `jtao` 工作流与调度执行系统技术设计规范（Workflow · Runner · Scheduler v1）

2026-11-25 · **状态：已拍板定案** · 技术栈：**Go 1.23+ 单文件静态二进制（`modernc.org/sqlite`，零 CGO）** · 基于 AIOS `a9951600`（含 `rex-harness` 子模块）全链路源码盘点，面向 `jtao` 的独立解耦工作流与调度执行设计。

> **已拍板核心决策**
> 1. **技术栈与存储**：采用 **Go 1.23+ 单文件静态二进制**（`CGO_ENABLED=0`）；工作流与调度状态落在独立单文件 SQLite（`<workspace>/.jtao/workflow/workflow.db`，WAL 模式，`modernc.org/sqlite`），与记忆库（`.jtao/memory/memory.db`）物理隔离、零共享表，仅通过 `workItemId` 和 `evidenceRefs` 关联；按需自动投影人类可读的 `docs/plans/<date>-<workItemId>.md`。
> 2. **合四为三、消除割裂**：不再复制 AIOS 中「Plan `tasks[]` / Rex `features[]` / Team `phases[]+jobs[]`」三套结构互转，也不再复制「Solo 走 `ctx-agent` / Team 走 `one-shot-runner`」两套执行器。`jtao` 统一收口为 **Workflow（统一任务图 + 真回执门禁）**、**Runner（统一子进程与 Worktree 底座）**、**Scheduler（Solo 长循环 + Team 并发调度）** 三个解耦内核包。
> 3. **通用门禁、模板可选**：保留 AIOS/Rex 最核心的「不靠正则猜语义」和「真实命令回执验收（`receipt:`）+ 一次性执行令牌」机制；将 Rex 内置的 13 套软件工程流程沉淀为**可选模板/蓝图**（通过 `//go:embed` 打进二进制），默认不强绑日常小改动。
> 4. **范式定位**：内层由 Coding Agent（Codex / Claude Code）跑 **ReAct** 单轮探索与编码，外层由 `jtao` 提供 **Plan-and-Execute 确定性 DAG 状态机 + 真环境验收闭环（Verifier-Gated Loop）+ Orchestrator-Workers 并发编排 + 外置记忆** 的 Long-Running Agent Harness。

本文档回答四个问题：**模块边界怎么划、统一任务图与门禁怎么存、对外抛哪些固定接口、Solo/Team/聊天内如何共用同一套调度闭环**（下文接口契约作为语言无关 IDL，Go 落地时直接映射为 `internal/workflow`、`internal/runner`、`internal/scheduler` 下的 struct 与 interface）。

---

## 1. 架构定位与解耦边界

### 1.1 四条硬边界

1. **三层独立内核包（零循环依赖，Go `internal/` 强制边界）**：
   - `internal/workflow`：规则分流（Policy）、统一任务图（WorkItem DAG）、真回执验收器（Receipt & Gate）、蓝图模板与 Markdown 投影。**不依赖**具体 Coding Agent CLI，也不直接拉起 Agent 子进程。
   - `internal/runner`：统一的 Coding Agent 执行器（Codex / Claude Code 等）、命令回执执行器、进程组防孤儿监管（`Setpgid` + `Kill(-pgid)`）、Git Worktree 隔离池、大输出卸载适配。**不感知**业务状态机。
   - `internal/scheduler`：运行账本与租约、Solo 自动循环（节拍/配额/退避/恢复）、Team 并发 DAG 调度池（`goroutine` + `context`）与 Merge Gate。只通过固定接口编排 `WorkflowService`、`AgentRunner` 和可选的 `MemoryService`。
2. **与 `internal/memory` 彻底解耦**：
   - Memory 不内嵌任何工作流状态机；Workflow/Scheduler 也不直接读 `memory.db`。
   - 跨系统只通过抽象端口 `MemoryPort`（对接 [`MemoryService`](./jtao-memory-design.md) 的 `recall` / `session` / `memo` / `artifact` 子域）通信；即使关闭 Memory，Workflow/Runner/Scheduler 仍可独立运行与测试。
3. **显式工作区入参**：所有接口显式接收 `workspaceRoot`，状态根支持 `JTAO_STATE_DIR` 覆盖，绝不隐式依赖 `process.cwd()`。
4. **单一内核、三端复用**：交互式聊天（Hook / MCP）、后台长跑（Solo CLI）、多角色协作（Team CLI）共用同一张任务图与运行账本，随时无缝接力，零中间 schema 翻译。

```text
┌────────────────────────────────────────────────────────────────────────────┐
│  薄接入壳（CLI / MCP Server / Host Hooks / Skills）                        │
│  · 聊天内：jtao workflow / MCP workflow_* + work_*                         │
│  · 后台跑：jtao solo run|resume|stop|status / jtao team run|retry-blocked  │
└───────────────┬─────────────────────────────────────────┬──────────────────┘
                │                                         │
                ▼                                         ▼
┌────────────────────────────────────┐    ┌──────────────────────────────────┐
│  internal/workflow（纯状态机内核）  │◄───│  internal/scheduler（调度控制面） │
│  · Policy（显式分流，不猜自然语言） │    │  · Solo Loop（图驱动+节拍+续跑）│
│  · WorkItem Task DAG（唯一任务模型）│    │  · Team Pool（并发DAG+MergeGate）│
│  · Receipt & Token Gate（真回执验收）│    │  · Lease / Control / Consequence │
│  · Blueprints & Plan MD 投影       │    └─────────┬──────────────┬─────────┘
└─────────────────┬──────────────────┘              │              │
                  │                                 ▼              ▼
                  │                  ┌───────────────────────┐ ┌──────────────────┐
                  │                  │ internal/runner       │ │ internal/memory  │
                  │                  │ · Codex/Claude 适配   │ │ (通过 MemoryPort)│
                  │                  │ · Setpgid 进程树强杀  │ │ · recall 注入    │
                  │                  │ · Git Worktree 隔离池 │ │ · checkpoint/交接│
                  │                  │ · 真命令回执执行器    │ │ · artifact 卸载  │
                  │                  └───────────────────────┘ └──────────────────┘
                  ▼
┌────────────────────────────────────────────────────────────────────────────┐
│  工作区隔离持久化（<workspaceRoot>/.jtao/workflow/，可由 JTAO_STATE_DIR 覆盖）│
│  · workflow.db（SQLite WAL：WorkItem / DAG / Receipt / Run / Lease 事实源） │
│  · receipts/<receiptId>.json（完整命令回执与截断输出审计记录）              │
│  · 可选投影：docs/plans/<date>-<workItemId>.md（只读展示/协作友好）         │
└────────────────────────────────────────────────────────────────────────────┘
```

---

## 2. 物理存储与统一数据模型（Schema v1）

### 2.1 存储选型：独立单文件 SQLite（WAL）

AIOS 在磁盘上分散了 `.aios/planning/active.json`、`.aios/planning/plans/<id>.json`、`.rex-harness/*.json`、`.aios/workflow-activations/long-running-deliveries/*.json`、Solo journal 目录以及 Team dispatch 工件，跨进程一致性和恢复逻辑极难维护。

`jtao` 统一收敛到 `<workspaceRoot>/.jtao/workflow/workflow.db`（纯 Go SQLite 驱动 `modernc.org/sqlite`，零 CGO，开启 `PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;`）：

- **事务原子性**：节点推进、回执绑定、执行令牌吊销、Run 状态更新在同一个 SQLite 事务内完成，避免“代码改了或测试过了，但 JSON 写了一半崩溃”导致的双账与脏状态。
- **人类可读投影**：每当 `WorkItem` 创建或节点状态变化时，可按配置自动将当前状态渲染为 `docs/plans/<date>-<workItemId>.md`（单向投影，事实源始终是 `workflow.db`）。

### 2.2 核心表结构

```sql
-- 1. 工作项总表（替代 AIOS Plan Contract + Rex Delivery + Team Plan 头部）
CREATE TABLE work_items (
  id                  TEXT PRIMARY KEY,          -- 规范化 slug（安全校验：禁止 .. / 控制字符 / <=80）
  title               TEXT NOT NULL,
  objective           TEXT NOT NULL,
  route               TEXT NOT NULL DEFAULT 'implement', -- implement|debug|design|verify|ops|team|harness
  blueprint           TEXT NOT NULL DEFAULT 'single',    -- single|delivery|feature|bugfix|refactor|security|custom
  status              TEXT NOT NULL DEFAULT 'active',    -- active|approved|executing|done|blocked|cancelled
  is_active           INTEGER NOT NULL DEFAULT 0,        -- 工作区当前活跃项标记
  baseline_receipt_id TEXT NOT NULL DEFAULT '',          -- 可选基线验收回执（长任务交付前校验 exit=0）
  origin_client       TEXT NOT NULL DEFAULT 'unknown',
  origin_session_id   TEXT NOT NULL DEFAULT '',
  plan_md_path        TEXT NOT NULL DEFAULT '',          -- 可选投影的 docs/plans/... 相对路径
  metadata_json       TEXT NOT NULL DEFAULT '{}',
  created_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
);

-- 2. 统一任务图节点（替代 Plan.tasks[] + Rex.features[] + Team.phases[]/jobs[]）
CREATE TABLE task_nodes (
  id                     TEXT NOT NULL,          -- 如 t1 / feat-checkout / phase.implement / merge.final-checks
  work_item_id           TEXT NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
  node_kind              TEXT NOT NULL DEFAULT 'task',   -- task | phase | merge_gate
  title                  TEXT NOT NULL,
  objective              TEXT NOT NULL DEFAULT '',
  role                   TEXT NOT NULL DEFAULT 'implementer', -- planner|implementer|reviewer|security-reviewer|custom
  status                 TEXT NOT NULL DEFAULT 'pending', -- pending|active|done|blocked|skipped|failed
  order_index            INTEGER NOT NULL DEFAULT 0,
  parallel_group         TEXT NOT NULL DEFAULT '',       -- 同组节点可并发执行并汇聚到 merge_gate
  can_edit_files         INTEGER NOT NULL DEFAULT 1,
  owned_path_prefixes_json TEXT NOT NULL DEFAULT '[]',   -- 可写路径前缀白名单（Team 文件归属权检查）
  targets_json           TEXT NOT NULL DEFAULT '[]',     -- 目标文件路径
  acceptance             TEXT NOT NULL DEFAULT '',       -- 验收标准说明
  verify_spec_json       TEXT NOT NULL DEFAULT 'null',   -- 可选强验收命令：{ command, args, cwdRel, expectedExitCode }
  template_stage         TEXT NOT NULL DEFAULT '',       -- 可选 Rex 能力模板标记（如 testing.tdd / review.specialist）
  retry_count            INTEGER NOT NULL DEFAULT 0,
  max_retries            INTEGER NOT NULL DEFAULT 2,
  execution_token        TEXT NOT NULL DEFAULT '',       -- 当前签发的一次性令牌（推进节点时强制核对）
  handoff_json           TEXT NOT NULL DEFAULT 'null',   -- 最新结构化交接包（status/findings/filesTouched/blockers）
  evidence_refs_json     TEXT NOT NULL DEFAULT '[]',     -- 已绑定的 receipt:/artifact:/note: 引用
  created_at             TEXT NOT NULL,
  updated_at             TEXT NOT NULL,
  PRIMARY KEY (work_item_id, id)
);

-- 3. 任务图有向无环边（写入时强制执行无环校验，拒绝自环与循环依赖）
CREATE TABLE task_edges (
  work_item_id  TEXT NOT NULL,
  from_node_id  TEXT NOT NULL,                   -- 前置依赖节点
  to_node_id    TEXT NOT NULL,                   -- 下游节点
  PRIMARY KEY (work_item_id, from_node_id, to_node_id),
  FOREIGN KEY (work_item_id, from_node_id) REFERENCES task_nodes(work_item_id, id) ON DELETE CASCADE,
  FOREIGN KEY (work_item_id, to_node_id)   REFERENCES task_nodes(work_item_id, id) ON DELETE CASCADE
);

-- 4. 真实命令执行回执（对标 Rex execution-receipts，不可伪造、支持核对命令与退出码）
CREATE TABLE execution_receipts (
  id             TEXT PRIMARY KEY,               -- receipt:<ulid>
  work_item_id   TEXT NOT NULL DEFAULT '',
  node_id        TEXT NOT NULL DEFAULT '',
  run_id         TEXT NOT NULL DEFAULT '',
  command        TEXT NOT NULL,                  -- 实际执行的可执行文件
  args_json      TEXT NOT NULL,                  -- 实际 argv 数组
  cwd            TEXT NOT NULL,                  -- 实际工作目录
  exit_code      INTEGER NOT NULL,
  timed_out      INTEGER NOT NULL DEFAULT 0,
  duration_ms    INTEGER NOT NULL DEFAULT 0,
  stdout_sha256  TEXT NOT NULL DEFAULT '',
  stderr_sha256  TEXT NOT NULL DEFAULT '',
  output_excerpt TEXT NOT NULL DEFAULT '',       -- 截断摘要（全文可卸载到 artifact/receipt JSON）
  artifact_ref   TEXT NOT NULL DEFAULT '',       -- 完整输出引用
  created_at     TEXT NOT NULL
);

-- 5. 调度运行账本（统一 Solo 与 Team 运行实例）
CREATE TABLE runs (
  id                 TEXT PRIMARY KEY,           -- run:<mode>:<ulid>
  work_item_id       TEXT NOT NULL REFERENCES work_items(id),
  mode               TEXT NOT NULL,              -- solo | team
  provider           TEXT NOT NULL DEFAULT 'codex',
  status             TEXT NOT NULL DEFAULT 'running', -- running|done|blocked|human-gate|stopped|failed|backoff
  current_iteration  INTEGER NOT NULL DEFAULT 0,
  max_iterations     INTEGER NOT NULL DEFAULT 20,
  worktree_json      TEXT NOT NULL DEFAULT 'null', -- { enabled, baseRef, path, initialHead, preserved, cleanupReason }
  pacing_json        TEXT NOT NULL DEFAULT 'null', -- { quota, cadence, consecutiveNoop, safeBypass, lastDecision }
  backoff_json       TEXT NOT NULL DEFAULT 'null', -- { consecutiveFailures, until, lastFailureClass }
  stop_requested     INTEGER NOT NULL DEFAULT 0,
  stop_reason        TEXT NOT NULL DEFAULT '',
  summary_text       TEXT NOT NULL DEFAULT '',
  created_at         TEXT NOT NULL,
  updated_at         TEXT NOT NULL
);

-- 6. 单轮/单节点执行流水（支持崩溃检测、审计与 --retry-blocked 跳过已成功上游）
CREATE TABLE run_turns (
  id               TEXT PRIMARY KEY,             -- turn:<ulid>
  run_id           TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  work_item_id     TEXT NOT NULL,
  node_id          TEXT NOT NULL,
  iteration        INTEGER NOT NULL,
  phase_state      TEXT NOT NULL,                -- started | completed | aborted | crashed
  outcome          TEXT NOT NULL DEFAULT '',     -- success|noop|blocked|infra-retry|human-gate|stopped|failed
  failure_class    TEXT NOT NULL DEFAULT 'none',
  material         INTEGER NOT NULL DEFAULT 0,   -- S通道 selfReport.progressMade + 门禁确认
  is_safe_bypass   INTEGER NOT NULL DEFAULT 0,
  prompt_ref       TEXT NOT NULL DEFAULT '',     -- 短文本或 artifact: 引用
  output_ref       TEXT NOT NULL DEFAULT '',     -- 短文本或 artifact: 引用
  receipt_ids_json TEXT NOT NULL DEFAULT '[]',
  files_touched_json TEXT NOT NULL DEFAULT '[]',
  handoff_json     TEXT NOT NULL DEFAULT 'null',
  elapsed_ms       INTEGER NOT NULL DEFAULT 0,
  started_at       TEXT NOT NULL,
  finished_at      TEXT NOT NULL DEFAULT ''
);

-- 7. 运行租约（防同工作项/同 Run 双开并发写脏工作区）
CREATE TABLE run_leases (
  lease_key      TEXT PRIMARY KEY,               -- workitem:<id> 或 run:<id>
  run_id         TEXT NOT NULL,
  owner_pid      INTEGER NOT NULL,
  hostname       TEXT NOT NULL,
  acquired_at    TEXT NOT NULL,
  heartbeat_at   TEXT NOT NULL,
  expires_at     TEXT NOT NULL
);

CREATE INDEX idx_nodes_work_status ON task_nodes(work_item_id, status, order_index);
CREATE INDEX idx_receipts_work_node ON execution_receipts(work_item_id, node_id, created_at);
CREATE INDEX idx_turns_run_iter ON run_turns(run_id, iteration);
```

---

## 3. 子系统一：`internal/workflow` 对外固定接口

### 3.1 Policy 分流（严格继承 AIOS“北极星原则”）

`workflow.policy.classify()` 是**纯函数、零磁盘副作用**，绝不用关键词正则从自由文本猜测“是否只读 / 是否需要计划 / 是否需要多 Agent”：

- **只识别确定性信号**：
  1. 显式指令前缀（`/plan` `/team` `/solo` `/harness` `/debug` `/review` `/spec` 等）或调用方传入的 `explicitIntent`；
  2. 显式确认/恢复协议前缀（如 `ok` / `可以` / `继续` / `resume` + 是否有非空新目标 tail）；
  3. 当前工作区是否存在未终结的活跃 `WorkItem` 及同会话匹配状态；
  4. 策略模式（`adaptive` 默认 / `strict` 强制计划）。
- **固定输出 5 种处置（`disposition`）与 3 种宿主（`executionHost`）**：
  - `disposition`: `'noop' | 'direct' | 'guarded' | 'planned' | 'blocked'`
  - `persistence`: `'none' | 'reuse' | 'create'`
  - `executionHost`: `'single' | 'team' | 'harness'`

### 3.2 `WorkflowService` 固定接口契约（5 大子域）

```ts
export interface WorkflowService {
  // 1. 策略分流（纯函数）
  policy: {
    classify(input: PolicyClassifyInput): PolicyDecision;
  };

  // 2. 工作项与任务图管理（统一 WorkItem DAG）
  workItem: {
    create(input: CreateWorkItemInput): WorkItemDetail;        // 支持直接传入 nodes+edges 或套用 blueprint 模板；写入前强制无环校验与可选基线回执校验
    openOrCreate(input: OpenWorkItemInput): { item: WorkItemDetail; created: boolean }; // 按规范化 workItemId 幂等打开
    get(workspaceRoot: string, workItemId: string): WorkItemDetail | null;
    getActive(workspaceRoot: string): WorkItemDetail | null;
    list(workspaceRoot: string, filter?: { status?: WorkItemStatus[]; limit?: number }): WorkItemSummary[];
    setStatus(workspaceRoot: string, workItemId: string, status: WorkItemStatus, opts?: { note?: string; force?: boolean }): WorkItemDetail; // 标记 done 前强制跑 DoneGate
    replaceGraph(workspaceRoot: string, workItemId: string, graph: { nodes: TaskNodeInput[]; edges: TaskEdgeInput[] }): WorkItemDetail;
    exportMarkdown(workspaceRoot: string, workItemId: string): { path: string; markdown: string };
  };

  // 3. 任务图推进与令牌派发（聊天内 / Solo / Team 共用）
  graph: {
    listReadyNodes(workspaceRoot: string, workItemId: string): TaskNode[]; // 返回全部前置依赖已 done/skipped 的就绪节点
    claimNode(workspaceRoot: string, workItemId: string, nodeId?: string): ClaimedCommand | null; // 签发一次性 executionToken + 当前节点完整契约
    submitNodeResult(input: SubmitNodeResultInput): AdvanceNodeResult; // 强制校验 executionToken + verifySpec 真实回执 + 占位符拒绝 → 返回 continue|retry|blocked|human-gate|completed
    addEvidence(workspaceRoot: string, workItemId: string, nodeId: string, evidence: EvidenceItem): void;
    evaluateDoneGate(workspaceRoot: string, workItemId: string): DoneGateResult;
  };

  // 4. 真实命令回执仓库（可审计、防伪造）
  receipt: {
    record(workspaceRoot: string, input: RecordReceiptInput): ExecutionReceipt;
    get(workspaceRoot: string, receiptRef: string): ExecutionReceipt | null;
    verifyAgainstSpec(workspaceRoot: string, receiptRef: string, spec: VerifyCommandSpec): ReceiptVerifyResult;
  };

  // 5. 蓝图与可选流程模板（含 AIOS 4 套 Team 蓝图 + 可选 Rex 能力模板）
  template: {
    listBlueprints(): BlueprintSummary[];
    expandBlueprint(name: string, input: { objective: string; tasks?: TaskSeed[] }): { nodes: TaskNodeInput[]; edges: TaskEdgeInput[] };
  };
}
```

### 3.3 防糊弄门禁规则（继承 Rex 精华，去掉繁琐绑定）

1. **一次性 `executionToken` 封签**：每次 `claimNode` 为当前活跃节点生成新令牌；提交结果（`submitNodeResult`）必须携带当前令牌，拒绝旧轮次或篡改节点状态的回写。
2. **真回执核验（Receipt Verification）**：
   - 若节点配置了 `verifySpec`（例如 `pnpm test`），则**必须**提交真实存在的 `receipt:<id>`，且 `receipt.verifyAgainstSpec()` 核对 `command`、`args`、工作目录和 `exitCode === 0` 全部匹配才允许将节点标为 `done`。
   - 明确拒绝占位符引用（如 `placeholder` / `todo` / `tbd` / 非 `<scheme>:<value>` 格式）。
   - 若回执真实有效但 `exitCode !== 0`：
     - `retryCount < maxRetries` → `retryCount + 1`，返回 `decision: 'retry'`（保持节点 `active`，把失败摘要交给下一轮修复）；
     - 达到 `maxRetries` → 节点进入 `blocked`，返回 `decision: 'human-gate'`。
   - 若节点未配置 `verifySpec`（例如轻量文档/探索任务）：允许以结构化 handoff + 有效 evidence（如 `artifact:` / `diff:` / `note:`）过关，不强迫所有杂活都写测试命令。
3. **可选的 Rex 13 能力模板**：将 `requirements.clarify`、`testing.tdd`、`review.specialist` 等定义为可插拔 `Blueprint` / `StageTemplate`，按需通过 `--blueprint tdd-feature` 选用，不硬编码阻断普通任务。

---

## 4. 子系统二：`internal/runner` 对外固定接口

`internal/runner` 把 AIOS 分裂的 `execute-turn.mjs`（Solo）与 `one-shot-runner.mjs`（Team）合二为一，成为整个 `jtao` 唯一负责拉起外部进程与管理工作树沙箱的底座。

### 4.1 `AgentRunner` 与配套接口

```ts
export interface AgentRunner {
  readonly id: 'codex' | 'claude' | string;

  /** 仅做本地环境与 CLI 可用性探测，零模型调用 */
  checkReadiness(opts?: { env?: NodeJS.ProcessEnv }): Promise<ReadinessCheck>;

  /** 执行单轮 Agent 任务（统一超时、进程组清理、结构化输出、大文本卸载） */
  runTurn(input: RunTurnInput): Promise<RunTurnResult>;
}

export interface RunTurnInput {
  workspaceRoot: string;       // 原始仓库根
  executionCwd: string;        // 实际执行目录（原始仓库或 Worktree 路径）
  systemPrompt?: string;
  userPrompt: string;
  outputSchema?: object;       // Codex 原生 --output-schema 强结构化输出；Claude 用 JSON 约束提取
  timeoutMs?: number;          // 默认 30 分钟，范围 [1s, 6h]
  abortSignal?: AbortSignal;   // 外部 stop / SIGINT / SIGTERM 联动取消
  env?: NodeJS.ProcessEnv;
  extraArgs?: string[];        // 校验禁止覆盖保留参数
  offload?: OffloadHook;       // 可选注入 MemoryPort.artifact.store，超阈值自动落盘只留 ref
}

export interface RunTurnResult {
  exitCode: number | null;
  timedOut: boolean;
  aborted: boolean;
  treeAlive: boolean;          // 关键：若超时/中止后进程树未杀干净，强制 true，触发上层 fail-closed
  killEscalated: boolean;      // 是否升级到 SIGKILL
  childPid?: number;
  rawOutput: string;           // 已卸载压缩或原文（用于日志）
  parsedJson?: unknown;        // 按 outputSchema 提取的结构化结果
  outputArtifactRef?: string;  // 大输出卸载后的 artifact: 引用
  elapsedMs: number;
  executedCommand: { command: string; args: string[]; cwd: string };
  error?: string;
}

export interface WorktreeManager {
  prepare(input: { workspaceRoot: string; runId: string; nodeId?: string; baseRef?: string }): Promise<WorktreeHandle>;
  detectChanges(handle: WorktreeHandle): Promise<{ changed: boolean; hasDirtyFiles: boolean; headChanged: boolean; files: string[] }>;
  finalize(handle: WorktreeHandle, opts: { finalStatus: string; preserveOnChange?: boolean; forceCleanup?: boolean }): Promise<WorktreeHandle>;
}

export interface ReceiptExecutor {
  /** 在指定目录执行验收命令，直接生成并落盘真实回执 receipt:<id> */
  runAndRecord(input: {
    workspaceRoot: string;
    executionCwd: string;
    workItemId?: string;
    nodeId?: string;
    runId?: string;
    command: string;
    args: string[];
    timeoutMs?: number;
    env?: NodeJS.ProcessEnv;
  }): Promise<ExecutionReceipt>;
}
```

### 4.2 关键工程护栏（直接保留 AIOS 实测精华）

1. **进程组整树强杀（Fail-Closed）**：
   - Go 子进程设置 `SysProcAttr{Setpgid: true}` 以独立进程组启动，配合 `context.WithTimeout` / `context.WithCancel`；清理时调用 `syscall.Kill(-pgid, SIGTERM)` → 等待宽限期 → `SIGKILL`。
   - 超时或收到 `SIGINT/SIGTERM` 后探测进程组（`syscall.Kill(-pgid, 0)`）是否彻底退出；若 `treeAlive === true`，上层必须直接 `blocked` 停机并记录 `childPid`，**绝不开启下一轮并发写脏工作区**。
2. **Worktree 隔离保留策略**：
   - 默认在系统临时目录或 `.jtao/worktrees/<runId>` 创建 `git worktree add --detach`。
   - 结束时若检测到代码改动，或终态为 `blocked / human-gate / stopped / failed / backoff`，**自动保留 Worktree** 供人工检查或 `resume` 恢复；仅在无改动且正常结束时清理。
   - Team 模式下，当同一波并发出现多个 `canEditFiles: true` 节点时，可直接为每个可写节点分配独立 Worktree，解决 AIOS Team 同目录只能单写的限制。
3. **大输出自动卸载与敏感上下文脱敏**：
   - 单轮 prompt / output 超过阈值（默认 2048 字节）时通过 `MemoryPort.artifact.store()` 卸载到外部文件，turn 日志和下一轮上下文只保留紧凑摘要 + `artifact:` 引用，防止长任务日志撑爆窗口。

---

## 5. 子系统三：`internal/scheduler` 调度控制面

`internal/scheduler` 运行在 `WorkflowService` + `AgentRunner` + 可选 `MemoryPort` 之上，统一负责 **Solo 图驱动长循环** 与 **Team 并发 DAG 编排**。

### 5.1 统一后果裁决表（`TURN_CONSEQUENCES`）

完整保留 AIOS `turn-consequences.mjs` 的单一裁决表设计——新增或调整一种轮次结果时**只改一张表**，彻底消除多处 `if/else` 漂移：

| `outcome` | 允许 `material`（有效进展） | 默认失败类 | 退避动作 | 是否重置失败计数 | 节拍归类 | 默认停机 | 需人工放行（`ask`） | 计入空转（`quiet`） |
| :--- | :---: | :--- | :--- | :---: | :--- | :---: | :---: | :---: |
| `success` | 是（**须 S 通道 `progressMade: true` 确认**） | `none` | `none` | 是（须 `material`） | `active_work`（非 material 降为 `monitor_wait`） | 否 | 否 | 否 |
| `noop` | 否 | `none` | `none` | 是 | `monitor_wait` | 否 | 否 | **是** |
| `blocked` | 否 | `no-progress` | `shrink-scope` | 否 | `monitor_wait` | 否 | 否 | 否 |
| `infra-retry` | 否 | `runtime-error` | 可重试类→`retry-with-backoff` | 否 | `monitor_wait` | 否 | 否 | 否 |
| `human-gate` | 否 | `safety-gate` | `human-gate` | 否 | `human_gate` | **是** | **是** | 否 |
| `stopped` | 否 | `stop-requested` | `none` | 否 | `monitor_wait` | **是** | 否 | 否 |
| `failed` | 否 | `runtime-error` | `none` | 否 | `monitor_wait` | **是** | 否 | 否 |

- **可重试失败类白名单**：仅 `runtime-error | tool-error | rate-limited | provider-overloaded` 允许自动退避重试；`ownership-gate | safety-gate | budget-exhausted` 强制转 `human-gate`，绝不盲目烧 token。

### 5.2 Solo 模式：真正打通「任务图 → 自动循环 → 真回执验收」

AIOS 里 Solo Loop 和 Rex Long-Running Delivery 是断开的两座孤岛；`jtao` 在 Solo 主循环中直接以 `WorkflowService` 的 Task DAG 为驱动核心：

1. **启动前预检（Dry-Run Readiness）**：检查 CLI 可用性、Git 工作区状态、基线回执（若配置）、租约冲突；`--dry-run` 严格只读、零模型调用。
2. **独占租约 + 信号挂钩**：`claimLease(workitem:<id>)` + 心跳续约；监听 `SIGINT/SIGTERM` 触发当前 `AbortController` 清理子进程树并落盘 `stopped`。
3. **主循环每轮流程（`while iteration <= maxIterations`）**：
   1. 检查控制标志 `stop_requested` → 立即优雅停机；
   2. 执行退避等待（若处于 `backoff.until`）；
   3. 若启用无人值守节拍（`pacing`），调用纯函数 **`shouldRun()` 决策门**：
      - `ask`（未解除的人工门 / 预算耗尽）→ 停机待人工 `resume`；
      - `wait`（占空比配额耗尽 / cadence 冷却未到）→ 休眠到 `nextWakeMs` 后重问（不消耗 iteration）；
      - `quiet`（连续 `noop` 达到 `quietThreshold`）→ 自动停机防空转；
      - `run` → 继续本轮；
   4. 从 `WorkflowService.graph.claimNode()` 领取当前就绪节点与一次性 `executionToken`（若全图已完成，跑 `evaluateDoneGate()` 后正常结束）；
   5. **净室上下文拼装**：每轮用干净上下文构造 Prompt（当前节点目标 + 验收标准 + `MemoryPort.recall.forTurn()` 召回的关联记忆 + 上轮压缩交接摘要），**绝不把几十轮原始聊天记录越滚越大**；
   6. 落盘 `run_turns(phase_state='started')` 崩溃标记 → 调用 `runner.runTurn()`；
   7. 若节点绑定了 `verifySpec` 且 Agent 声明完成，由 `ReceiptExecutor.runAndRecord()` 在执行目录**实际运行验收命令**获取真实 `receipt:<id>`（也可核验 Agent 经由 CLI/MCP 产生的本地真实回执）；
   8. 调用 `WorkflowService.graph.submitNodeResult()` 按 `executionToken` + 真实回执推进图状态，并按 `TURN_CONSEQUENCES` 更新退避、配额与节拍；
   9. 若挂载 `MemoryPort`，写回 `session.checkpoint`、`session.writeContinuity`、以及解析 Agent 输出的 `<!--memory: ...-->` 声明；
   10. 处理 `safe_bypass`（无人值守在 `human-gate` 停机前可获准恰好一次只读探查，且代码级封印禁止该轮声称 `material` 进展）、连续失败熔断、或 `shouldStop` 退出。
4. **崩溃恢复与断点续跑（`resume`）**：
   - `resume` 显式清除 `stop_requested` 并视为操作者确认（解除上一轮遗留的 `operator_gate`，但**不绕过配额钱闸**）。
   - 若检测到上一轮停在 `phase_state = 'started'`（进程被强杀或断电），先检查 Worktree/工作区 `git status` 与已有 `execution_receipts`：若代码已改且验收命令未跑，先补跑验收确认进度，避免重复盲目重写。

### 5.3 Team 模式：并发 DAG 调度 + 文件归属权 Merge Gate

Team 模式直接跑在同一张 `WorkItem` 任务图与 `AgentRunner` 上，不再另造轮子：

1. **内置角色与蓝图**（完整兼容 AIOS 4 角色 × 4 蓝图，支持自定义扩展）：
   - 角色：`planner`（只读）、`implementer`（可写，受 `ownedPathPrefixes` 约束）、`reviewer`（只读）、`security-reviewer`（只读）。
   - 蓝图：
     - `feature`：`plan` → `implement` → `[review, security]`（并行）→ `merge-gate`
     - `bugfix` / `refactor`：`plan` → `implement` → `review`
     - `security`：`security-assessment` → `plan` → `implement` → `review`
2. **有界并发调度池**：按 `concurrency`（默认 3）持续拉取 `task_edges` 全部满足的就绪节点并发执行，上游任一节点 `blocked` 时下游自动级联标记 `blocked`。
3. **文件归属权与 Merge Gate**：
   - 每个 `merge_gate` 节点收集上游并行分支的结构化 Handoff 与实际 `git diff` 改动文件列表；
   - 存在以下任一情况立即 `blocked` 并输出清晰人工决议问题：**任一上游 blocked/needs-input**、**改动了越出 `ownedPathPrefixes` 的文件**、**多个并行分支修改了同一文件且未声明合入归属**；
   - 当并行分支均需改代码时，启用 `WorktreeManager` 为各分支隔离目录，Merge Gate 再执行干净合入检查。
4. **省钱与可靠恢复机制**：
   - **只读审查自动跳过**：上游未产生任何文件改动时，自动跳过纯只读 review 节点；
   - **改动前快照**：可选在可写节点执行前创建轻量 git snapshot/stash 标记，便于回滚；
   - **局部失败重跑（`--retry-blocked`）**：直接从 `run_turns` 读取已成功节点作为 seed 跳过，只重跑 `blocked/failed` 及其下游节点；若同一节点在历史运行中连续反复阻塞且无改动，触发保护提示拒绝盲目重试（可用 `--force` 覆盖）。

### 5.4 `SchedulerService` 固定接口契约

```ts
export interface SchedulerService {
  solo: {
    dryRun(input: SoloRunInput): Promise<ReadinessReport>;
    run(input: SoloRunInput): Promise<RunResult>;
    resume(input: { workspaceRoot: string; runId: string; maxIterations?: number; dryRun?: boolean }): Promise<RunResult>;
    stop(input: { workspaceRoot: string; runId: string; reason?: string }): Promise<RunStatus>;
    status(input: { workspaceRoot: string; runId?: string; workItemId?: string }): Promise<RunStatus | null>;
  };

  team: {
    plan(input: TeamRunInput): Promise<TeamPlanPreview>;       // 展开蓝图 + 检查文件归属权，不调用模型
    run(input: TeamRunInput): Promise<TeamRunResult>;
    retryBlocked(input: { workspaceRoot: string; runId: string; force?: boolean; dryRun?: boolean }): Promise<TeamRunResult>;
  };
}
```

---

## 6. AIOS 能力全覆盖与避坑对照表

确保迁移或对齐 AIOS 能力时**零能力缺失**，同时彻底解决实测发现的 5 处割裂：

| AIOS 现有能力 / 痛点 | AIOS 现状（源码实测） | `jtao` 对应落地方式 |
| :--- | :--- | :--- |
| Workflow Policy 规则分流 | `workflow-policy.mjs` 纯规则，不猜自然语言 | 原样保留于 `workflow.policy.classify()`（5 disposition × 3 host） |
| 计划账本 + Done Gate | `docs/plans/*.md` + `.aios/planning/*.json` | 统一为 `workflow.db` 的 `work_items + task_nodes` + 可选 Markdown 单向投影 + `evaluateDoneGate()` |
| Rex Token + 真回执验收 | `rex-activation-store` + `validateCommandEvidence` | 抽离为通用的 `execution_token` + `execution_receipts` 验收门禁，不再强绑 13 种固定流程 |
| **痛点 1：Rex 长任务图未接进生产循环** | `rex-long-running-delivery-store.mjs` 仅单测调用，Solo 循环不读它 | **彻底打通**：Solo 主循环直接驱动 `WorkItem` DAG，每轮领节点 + 跑真回执推进 |
| **痛点 2：三套任务结构来回翻译** | `Plan.tasks` → `Team.workItems` → `Team.jobs` → `Rex.activation` | **合三为一**：全系统唯一 `WorkItem + TaskNode + TaskEdge` 模型，零翻译层 |
| **痛点 3：Solo 与 Team 两套执行器** | Solo 调 `ctx-agent.mjs`；Team 调 `one-shot-runner.mjs`（缺 Worktree） | **合二为一**：统一 `internal/runner`，Worktree、进程树强杀、结构化输出全共享 |
| Solo 工程护栏（租约/退避/节拍/空转停机/`safe_bypass`） | `solo-runtime` 非常成熟 | 完整继承至 `internal/scheduler/solo`，并用 SQLite 事务替代散落 JSON 锁文件 |
| Team 蓝图/并发池/文件归属/Merge Gate/`--retry-blocked` | `orchestrator` + `subagent-runtime` | 完整继承至 `internal/scheduler/team`，并去掉 AIOS 重型 `learn-eval` / `release-guard` 耦合 |
| 聊天内 ↔ 后台接力 | `ctx-agent` 与 `harness` / `work` 割裂 | 统一读写同一 `workItemId`，聊天里开单、后台 Solo/Team 接力跑完 |

---

## 7. 演进里程碑（与 Memory Core 协同推进）

与 [`jtao-memory-design.md`](./jtao-memory-design.md) 对齐，按“先跑通单闭环，再上多并发和无人值守”分三阶段落地：

- **M1（最小闭环 — 能跑、能验、不断档）**：
  - `internal/workflow`：`workflow.db` schema、`policy.classify`、`workItem` + `task_nodes/edges`（含无环校验）、`claimNode`（`executionToken`）、`receipt.record/verify`、`submitNodeResult`、`exportMarkdown`。
  - `internal/runner`：Codex CLI 适配器（含 `--output-schema`）、`Setpgid` 进程组超时强杀（fail-closed）、`ReceiptExecutor`、基础 `WorktreeManager`。
  - `internal/scheduler`：Solo 图驱动基础循环（租约 + `TURN_CONSEQUENCES` + `run/status/stop/resume` + 崩溃检测恢复），对接 `MemoryPort` 写入 checkpoint 与 recall。
- **M2（并发与无人值守 — Team 编排 + 节拍闸门）**：
  - `internal/scheduler/solo`：补齐无人值守 `pacing`（配额窗口、cadence 动态退避、`quietThreshold` 空转停机、`safe_bypass` 门前只读探查）。
  - `internal/scheduler/team`：4 套内置蓝图、有界并发池（goroutine + context）、`ownedPathPrefixes` 越权检测 + `merge-gate`、只读无改动自动跳过、`--retry-blocked` 局部重跑、多写节点独立 Worktree。
  - `internal/runner`：补齐 Claude Code 适配器与大输出 `artifact` 自动卸载。
- **M3（可选模板与扩展）**：
  - 按需沉淀 Rex 风格的高严谨模板（如 `tdd-delivery`、`security-audit`）为可选 Blueprint/Skill；
  - 如有需要再扩展 Pi RPC 长连接执行器或本地轻量守护进程。
