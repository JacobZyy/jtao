# `jtao` 记忆系统技术设计规范（Memory Core v1）

2026-11-24（2026-11-25 对齐 Go 技术栈） · **状态：已拍板定案** · 技术栈：**Go 1.23+ 单文件静态二进制（`modernc.org/sqlite`，零 CGO）** · 基于 AIOS `a9951600` 全仓外部消费点盘点，面向 `jtao` 的独立解耦记忆子系统设计。

> **已拍板核心决策**
> 1. **存储方案**：采用**单文件 SQLite（WAL + FTS5，纯 Go 驱动 `modernc.org/sqlite`）+ 外置大文本目录**（`.jtao/memory/memory.db` + `artifacts/`），零部署、按工作区物理隔离，不引入 PostgreSQL；接口已与存储解耦，未来如需多人云端协作只需替换底层实现。
> 2. **设计取舍**：保留 AIOS 经过验证的记忆语义与核心机制（避免过度自造），仅收敛对外固定接口（6 大子域）并修复已验证的 4 处 AIOS 断层（Hook 空注/声明无消费、置顶双路径与无自动注入、MCP 缺 `eventId` 与 `useful/supersede`、候选晋升写死拒绝）。
> 3. **迁移保障**：所有适配层（CLI / MCP / Hook）及未来迁移的 Workflow / Solo / Team / Dream 模块统一调用同一套 `MemoryService` 接口，保证零能力缺失。

本文档回答三个问题：**边界怎么划、对外抛哪些固定接口、内部存储与检索怎么做**（下文接口伪代码作为语言无关契约 IDL，Go 落地时直接映射为 `internal/memory` 下的 struct 与 interface）。

---

## 1. 架构定位与解耦原则

### 1.1 四条硬边界

1. **完全独立的内核包（`internal/memory`）**：零宿主依赖，不反向依赖任何 Coding Agent（Codex / Claude / Pi）、CLI、MCP、工作流状态机、调度器或 Dream 整理模块。
2. **显式上下文入参**：所有接口显式接收 `workspaceRoot`（工作区隔离键），绝不隐式读取 `process.cwd()` 或靠全局配置文件决定工作区。
3. **单一内核、多薄壳适配**：CLI、MCP Server、宿主 Hook、Solo/Team 调度器只做「参数校验、权限/身份注入、输出格式化」，业务规则与持久化全部收口在 `MemoryService` 内核，彻底消除 AIOS 中「MCP 残血、CLI 完整、ContextDB 另起炉灶」的三套割裂。
4. **领域边界清晰（高内聚、低耦合）**：
   - **归 Memory 管**：项目知识（Memo）、置顶规则与里程碑（Pinned）、会话轨迹与恢复（Session / Event / Checkpoint / Continuity / Handoff）、大输出外置引用（Artifact / Offload）、统一召回（Recall）、内容安全与候选治理（Governance）、巡检与维护（Admin）。
   - **不归 Memory 管**：
     - **工单状态机与验收回执校验**属于工作流层（对标 AIOS Planning + Rex）。Memory 不内嵌状态机逻辑，但所有核心实体原生预留一等字段 `workItemId` 和 `evidenceRefs`，支持按工单 ID 反查全部关联记忆、检查点与交接包。
     - **项目源码 / 计划文档等非记忆文件的搜索**通过可注入的 `ExternalSearchProvider` 扩展口挂载，Memory 内核不硬编码宿主仓库的目录结构（如 `docs/plans/`）。
     - **Dream 自动整理**作为外部消费方调用 `memo.list` + `governance.archive` + `admin.hygiene`，消除 AIOS 中 `memo/query.mjs` 反向依赖 `execution/evolve/dream/` 的循环耦合。

```text
┌──────────────────────────────────────────────────────────────────────────┐
│  薄适配壳 & 上层消费方（零存储逻辑，只调 MemoryService 固定接口）          │
│  ┌───────────┐  ┌──────────────┐  ┌───────────┐  ┌────────────────────┐  │
│  │ jtao CLI  │  │ jtao-memory  │  │ Host Hook │  │ Workflow / Solo /  │  │
│  │ (json/txt)│  │  MCP Server  │  │ (按需注入) │  │ Team / Dream       │  │
│  └─────┬─────┘  └──────┬───────┘  └─────┬─────┘  └─────────┬──────────┘  │
└────────┼───────────────┼────────────────┼──────────────────┼─────────────┘
         └───────────────┴────────┬───────┴──────────────────┘
                                  ▼
┌──────────────────────────────────────────────────────────────────────────┐
│                     MemoryService（独立解耦内核）                         │
│  ┌──────┐ ┌─────────┐ ┌────────┐ ┌────────────┐ ┌──────────┐ ┌───────┐   │
│  │ memo │ │ session │ │ recall │ │ governance │ │ artifact │ │ admin │   │
│  └──────┘ └─────────┘ └────────┘ └────────────┘ └──────────┘ └───────┘   │
│  内部机制：安全扫描 · 五要素校验 · 双时态折叠 · 中英分词 · BM25+实体+反馈  │
└─────────────────────────────────┬────────────────────────────────────────┘
                                  ▼
┌──────────────────────────────────────────────────────────────────────────┐
│  工作区隔离持久化（<workspaceRoot>/.jtao/memory/，可由 JTAO_STATE_DIR 覆盖）│
│  · memory.db（SQLite WAL + FTS5：唯一事实源与检索索引）                   │
│  · artifacts/<sha256>.txt（大工具输出外置原文，库内只存引用与摘要）       │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## 2. 物理存储与检索技术方案

### 2.1 存储选型：单库 SQLite（WAL + FTS5）+ 外置大文本文件

AIOS 早期在磁盘上叠了四层存储（Memo JSONL、ContextDB 会话 JSONL、跨会话 JSONL 索引、SQLite 侧车 + 旧 `workspace-memory` 镜像），直接导致置顶双路径脱节、Memo 只能全量扫描、多进程靠手写目录锁互斥。

`jtao` 改为**每个工作区一份 SQLite（使用纯 Go 驱动 `modernc.org/sqlite`，内置 FTS5、`CGO_ENABLED=0` 零原生编译依赖）**作为唯一事实源，配合外置文本目录存大对象：

- **默认目录布局**：
  - `<workspace>/.jtao/memory/memory.db`（开启 `PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;`）
  - `<workspace>/.jtao/memory/artifacts/<sha256>.txt`（大输出原文，默认阈值 ≥ 2048 字节）
  - 可通过 `JTAO_STATE_DIR` 环境变量统一重定向状态根，测试与多工作区完全隔离。
- **并发与原子性**：依赖 SQLite WAL 与事务保证 MCP、CLI、Hook、多子 Agent 并发读写不损数据、不丢写入，无需自研文件锁目录。
- **可审计与可迁移性**：`memo_events` 和 `session_events` 仍坚持**只追加（append-only）**语义，修订靠 `memo_supersedes` 关联新旧 ID，绝不原地覆写历史；提供 `admin.exportJsonl()` 随时导出人类可读的纯文本快照，以及 `admin.rebuildIndex()` 一键重建 FTS5。

### 2.2 核心表结构（Schema v1）

```sql
-- 1. 项目知识（Append-only + 双时态 + 五要素 + 来源治理）
CREATE TABLE memo_events (
  id             TEXT PRIMARY KEY,               -- memo:<space>:<ulid/uuid>
  seq            INTEGER NOT NULL,               -- 空间内单调递增序号
  space          TEXT NOT NULL DEFAULT 'default',-- 如 default / role:reviewer
  scope          TEXT NOT NULL DEFAULT 'project_shared', -- project_shared | agent_private | agent_ephemeral
  agent          TEXT NOT NULL DEFAULT '',
  work_item_id   TEXT NOT NULL DEFAULT '',       -- 关联工单 ID（一等索引字段）
  text           TEXT NOT NULL,                  -- 事实结论正文
  search_tokens  TEXT NOT NULL,                  -- 写入时预分词结果（供 FTS5 中英统一检索）
  refs_json      TEXT NOT NULL DEFAULT '[]',     -- 关联文件路径/引用
  entities_json  TEXT NOT NULL DEFAULT '[]',     -- 五要素：实体列表（<=24）
  evidence_ref   TEXT NOT NULL DEFAULT '',       -- 五要素：单一证据引用（拒绝数组）
  confidence     TEXT NOT NULL DEFAULT '',       -- 五要素：high | medium | ''
  claim_status   TEXT NOT NULL DEFAULT 'verified', -- verified | candidate | observed
  archived       INTEGER NOT NULL DEFAULT 0,     -- Dream / 治理归档标记（读时过滤）
  promotion_of   TEXT NOT NULL DEFAULT '',       -- 若由候选晋升而来，指向原候选 ID
  provenance_json TEXT NOT NULL,                 -- producerType / principalId / sessionId / runId / sourceRef / sourceHash
  valid_at       TEXT NOT NULL,                  -- ISO 8601：事实生效时间
  created_at     TEXT NOT NULL,                  -- ISO 8601：写入时间
  source_ref     TEXT NOT NULL DEFAULT '',       -- 自动写入幂等去重键（如 auto:<session>:<turn>）
  UNIQUE(space, seq)
);

-- 1b. 替代关系（双时态折叠依据，拒绝跨空间/非法跨私有替代）
CREATE TABLE memo_supersedes (
  source_id      TEXT NOT NULL REFERENCES memo_events(id),
  target_id      TEXT NOT NULL REFERENCES memo_events(id),
  PRIMARY KEY (source_id, target_id)
);

-- 1c. 实体倒排表（支撑 O(1) 一跳共享实体扩散，告别全表扫描）
CREATE TABLE memo_entities (
  memo_id        TEXT NOT NULL REFERENCES memo_events(id),
  entity         TEXT NOT NULL,                  -- normalized lowercase
  PRIMARY KEY (memo_id, entity)
);

-- 1d. 召回反馈流水（impression 曝光 + useful 采纳）
CREATE TABLE memo_feedback (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  memo_id        TEXT NOT NULL REFERENCES memo_events(id),
  signal         TEXT NOT NULL,                  -- impression | useful
  query          TEXT NOT NULL DEFAULT '',
  session_id     TEXT NOT NULL DEFAULT '',
  agent          TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL
);

-- 2. 置顶内容（单一事实源，按 space 隔离 + 乐观锁哈希）
CREATE TABLE pinned_entries (
  id             TEXT PRIMARY KEY,
  space          TEXT NOT NULL DEFAULT 'default',
  kind           TEXT NOT NULL DEFAULT 'rule',   -- rule（常驻规则保底）| checkpoint（里程碑新优先）
  text           TEXT NOT NULL,
  work_item_id   TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL
);
CREATE TABLE pinned_meta (
  space          TEXT PRIMARY KEY,
  content_hash   TEXT NOT NULL,                  -- 乐观锁 expectedHash
  updated_at     TEXT NOT NULL
);

-- 3. 会话轨迹（L2 事件 + L1 检查点 + L0/连续性 + 交接包 + 轮次笔记）
CREATE TABLE sessions (
  id              TEXT PRIMARY KEY,
  agent           TEXT NOT NULL,
  project         TEXT NOT NULL,
  goal            TEXT NOT NULL,
  work_item_id    TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL DEFAULT 'running', -- running | blocked | done | error | archived
  tags_json       TEXT NOT NULL DEFAULT '[]',
  summary_md      TEXT NOT NULL DEFAULT '',      -- L0 摘要（由最新 checkpoint 原子刷新）
  continuity_json TEXT NOT NULL DEFAULT '{}',    -- intent / summary / touchedFiles / nextActions
  handoff_json    TEXT NOT NULL DEFAULT '',      -- 跨 Agent 交接包（v3 含 lineage / receiptRef）
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL
);

CREATE TABLE session_events (
  id              TEXT PRIMARY KEY,              -- <sessionId>#<seq>
  session_id      TEXT NOT NULL REFERENCES sessions(id),
  seq             INTEGER NOT NULL,
  role            TEXT NOT NULL,                 -- system | user | assistant | tool
  kind            TEXT NOT NULL DEFAULT 'message',
  text            TEXT NOT NULL,
  search_tokens   TEXT NOT NULL,
  refs_json       TEXT NOT NULL DEFAULT '[]',
  turn_json       TEXT NOT NULL DEFAULT '{}',    -- turnId / turnType / environment / workItemRefs
  signature_hash  TEXT NOT NULL,                 -- 短窗口同签名去重
  created_at      TEXT NOT NULL,
  UNIQUE(session_id, seq)
);

CREATE TABLE session_checkpoints (
  id                  TEXT PRIMARY KEY,          -- <sessionId>#C<seq>
  session_id          TEXT NOT NULL REFERENCES sessions(id),
  seq                 INTEGER NOT NULL,
  status              TEXT NOT NULL,             -- running | blocked | done
  summary             TEXT NOT NULL,
  search_tokens       TEXT NOT NULL,
  next_actions_json   TEXT NOT NULL DEFAULT '[]',
  artifacts_json      TEXT NOT NULL DEFAULT '[]',
  telemetry_json      TEXT NOT NULL DEFAULT '{}', -- verification / retryCount / failureCategory / elapsedMs / cost
  created_at          TEXT NOT NULL,
  UNIQUE(session_id, seq)
);

CREATE TABLE session_iteration_notes (
  session_id      TEXT NOT NULL REFERENCES sessions(id),
  iteration       INTEGER NOT NULL,
  note_json       TEXT NOT NULL,                 -- completed / blockers / decisions / nextActions / artifacts
  created_at      TEXT NOT NULL,
  PRIMARY KEY (session_id, iteration)
);

-- 4. 治理审计回执（候选晋升/拒绝/过期 + 归档/恢复）
CREATE TABLE governance_receipts (
  id              TEXT PRIMARY KEY,
  target_type     TEXT NOT NULL,                 -- candidate | archive
  target_id       TEXT NOT NULL,
  action          TEXT NOT NULL,                 -- promote | reject | expire | archive | restore
  decision        TEXT NOT NULL,                 -- ALLOW | DENY
  reason          TEXT NOT NULL DEFAULT '',
  reason_code     TEXT NOT NULL,
  principal_json  TEXT NOT NULL DEFAULT '{}',
  safety_json     TEXT NOT NULL DEFAULT '{}',
  promoted_id     TEXT NOT NULL DEFAULT '',
  created_at      TEXT NOT NULL
);

-- 5. 大输出外置索引（Offload Refs）
CREATE TABLE artifacts (
  ref_id          TEXT PRIMARY KEY,
  session_id      TEXT NOT NULL DEFAULT '',
  work_item_id    TEXT NOT NULL DEFAULT '',
  tool            TEXT NOT NULL DEFAULT '',
  label           TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL DEFAULT 'ok',    -- ok | fail
  size_bytes      INTEGER NOT NULL,
  line_count      INTEGER NOT NULL,
  sha256          TEXT NOT NULL,
  rel_path        TEXT NOT NULL,                 -- artifacts/<sha256>.txt
  created_at      TEXT NOT NULL
);

-- 6. 统一 FTS5 检索虚拟表（基于预分词列，原生支持中英文 BM25）
CREATE VIRTUAL TABLE memo_fts USING fts5(memo_id UNINDEXED, search_tokens, tokenize = 'unicode61');
CREATE VIRTUAL TABLE session_events_fts USING fts5(event_id UNINDEXED, search_tokens, tokenize = 'unicode61');
CREATE VIRTUAL TABLE session_checkpoints_fts USING fts5(checkpoint_id UNINDEXED, search_tokens, tokenize = 'unicode61');
```

### 2.3 中英文统一检索与打分机制（吸收 AIOS 精髓，补齐性能短板）

1. **零重依赖中英文预分词**：纯 Go 实现轻量中英分词器——英文/数字按词元小写切分、中文/CJK 连续片段生成**二元组（2-gram）滑动窗口（单字保留）**、同时保留完整标识符（如 `checkout.json`、`order-service`），写入时生成 `search_tokens` 进入 FTS5，无需携带大型分词词典。查询时按同样分词器构造 FTS5 查询，直接走 SQLite 索引而非全量读盘扫描。
2. **双时态与可见性过滤**：
   - 默认过滤：`claim_status != 'candidate'`、`archived = 0`、未到 `valid_at` 或已被非候选记录在 `asOf` 前替代（`invalidAt <= asOf`）、`agent_private` 超出默认 30 天读时 TTL（可由 `JTAO_AGENT_PRIVATE_TTL_DAYS` 配置，`0` 关闭）；
   - `asOf` 支持任意历史时间点回查，`includeInvalid` / `includeCandidates` / `includeArchived` 供审计与治理调用。
3. **四因子可解释排序**：
   - `base = exactMatchBonus + bm25Score * 4 + entityBoost`（直接命中实体最高 `+1.0`，通过 `memo_entities` 表做一跳邻居扩散最高 `+0.5`）；
   - 叠加反馈权重：每次 `useful` `+1.5`（上限 `+4.5`），累计 `impression >= 5` 且 `useful = 0` 乘以 `0.6`；
   - **平局较新优先**：两条得分差 `< 0.2`（`MATCH_SCORE_EPSILON`）时按 `created_at` 较新优先。
4. **可选向量扩展口**：预留 `Embedder` 接口（`embedText(text) -> Promise<Float32Array>`），采用「只扩充候选池、不丢词法命中」的并集策略；第一版零模型零依赖，后续可无缝挂本地小模型或 `sqlite-vec`。

---

## 3. 固定对外接口契约（`MemoryService`）

所有外部模块、CLI、MCP、Hook 只面对同一个入口 `createMemoryService(options)`，对外固定暴露 **6 个子域**。

```ts
export interface MemoryServiceOptions {
  workspaceRoot: string;                      // 必填：工作区根目录
  stateDir?: string;                          // 可选：覆盖 <workspace>/.jtao/memory
  approvalPolicy?: GovernanceApprovalPolicy;  // 可选：候选晋升/归档审批策略（默认支持本地显式确认）
  externalSources?: ExternalSearchProvider[]; // 可选：外部只读搜索源（plans/docs/code 等）
  embedder?: MemoryEmbedder | null;           // 可选：本地向量扩召回接口
}

export interface MemoryService {
  readonly memo: MemoApi;
  readonly session: SessionApi;
  readonly recall: RecallApi;
  readonly governance: GovernanceApi;
  readonly artifact: ArtifactApi;
  readonly admin: AdminApi;
  close(): void;
}
```

### 3.1 `memo` —— 项目知识、置顶与反馈

```ts
export interface MemoApi {
  // 写入一条知识（内置安全扫描、五要素格式校验、替代权限校验、可选 sourceRef 幂等去重）
  write(input: {
    text: string;
    space?: string;                  // 默认 'default'；角色记忆用 'role:<name>'
    scope?: 'project_shared' | 'agent_private' | 'agent_ephemeral';
    agent?: string;
    workItemId?: string;
    refs?: string[];
    entities?: string[];             // 最多 24 个
    date?: string;                   // 绝对 YYYY-MM-DD（拒绝相对时间如“昨天”）
    validAt?: string;                // ISO 8601（缺省取 date 或当前时间）
    evidenceRef?: string;            // 标量字符串（拒绝数组）
    confidence?: 'high' | 'medium';
    supersedes?: string[];           // 声明替代的旧 memo ID
    sourceRef?: string;              // 自动写入幂等键（同键返回 status='duplicate'）
    runtimeIdentity?: RuntimeIdentity | null; // 有非人类 runtimeIdentity 且无发布权 -> 落 candidate
    returnSupersedeHints?: boolean;  // 可选：返回相似旧条目建议（默认 true）
  }): Promise<MemoWriteResult>;      // 返回 { status, event, supersedeDenied, supersedeHints }

  get(id: string): Promise<MemoEvent | null>;

  list(query?: {
    space?: string;
    scope?: string;
    agent?: string;
    workItemId?: string;
    asOf?: string;
    includeInvalid?: boolean;
    includeCandidates?: boolean;
    includeArchived?: boolean;
    limit?: number;
  }): Promise<MemoEvent[]>;

  search(query: {
    query: string;
    space?: string;
    scope?: string;
    agent?: string;
    workItemId?: string;
    asOf?: string;
    includeInvalid?: boolean;
    includeCandidates?: boolean;
    includeArchived?: boolean;
    limit?: number;
    maxCharsPerItem?: number;
    maxTotalChars?: number;
  }): Promise<MemoSearchHit[]>;      // 每条必含 id, score, text, refs, entities, evidenceRef, workItemId, validAt

  // 曝光 / 采纳反馈（支持传入 allowedIds 做本轮召回交集校验）
  feedback(input: {
    eventIds: string[];
    signal: 'impression' | 'useful';
    allowedIds?: string[];           // 传入时仅接受交集内的 ID
    query?: string;
    sessionId?: string;
    agent?: string;
  }): Promise<{ recorded: number; ignored: number }>;

  // 置顶（单一事实源：rule 常驻保底 + checkpoint 从新到旧填充窗口）
  pin: {
    get(input?: { space?: string }): Promise<{ entries: PinnedEntry[]; contentHash: string }>;
    append(input: {
      text: string;
      kind?: 'rule' | 'checkpoint';  // 默认 checkpoint
      space?: string;
      workItemId?: string;
      expectedHash?: string;         // 乐观锁：冲突报 STALE_WRITE
      maxChars?: number;             // 窗口上限（默认 5000，钳制在 [512, 20000]）
    }): Promise<PinnedWriteReceipt>; // 明确返回 { entry, contentHash, served, chars, maxChars, included, dropped }
    set(input: {
      entries: Array<{ kind: 'rule' | 'checkpoint'; text: string; workItemId?: string }>;
      space?: string;
      expectedHash?: string;
      maxChars?: number;
    }): Promise<PinnedWriteReceipt>;
    render(input?: { space?: string; maxChars?: number }): Promise<PinnedRenderResult>; // 供 SessionStart 真正注入
  };
}
```

### 3.2 `session` —— 会话轨迹、检查点、连续性与交接包

```ts
export interface SessionApi {
  create(input: {
    sessionId?: string;
    agent: string;
    project?: string;
    goal: string;
    workItemId?: string;
    tags?: string[];
  }): Promise<SessionMeta>;

  // 幂等登记：存在则返回现有会话，不存在则创建
  ensure(input: {
    sessionId: string;
    agent: string;
    project?: string;
    goal?: string;
    workItemId?: string;
    tags?: string[];
  }): Promise<{ session: SessionMeta; created: boolean }>;

  get(sessionId: string): Promise<SessionMeta | null>;
  latest(filter?: { agent?: string; project?: string; workItemId?: string }): Promise<SessionMeta | null>;
  list(filter?: { agent?: string; status?: string; workItemId?: string; limit?: number }): Promise<SessionMeta[]>;

  // L2 事件写入（带短窗口同签名去重）
  appendEvent(input: {
    sessionId: string;
    role: 'system' | 'user' | 'assistant' | 'tool';
    kind?: string;
    text: string;
    refs?: string[];
    turn?: TurnEnvelope;
  }): Promise<SessionEvent>;

  getEvent(eventId: string): Promise<SessionEvent | null>;
  searchEvents(query: SessionSearchQuery): Promise<SessionEventHit[]>;
  timeline(filter?: { sessionId?: string; workItemId?: string; limit?: number }): Promise<TimelineItem[]>;

  // L1 检查点（原子刷新 L0 摘要与 continuity）
  checkpoint(input: {
    sessionId: string;
    summary: string;
    status?: 'running' | 'blocked' | 'done';
    nextActions?: string[];
    artifacts?: string[];
    touchedFiles?: string[];
    telemetry?: CheckpointTelemetry; // verification / retryCount / failureCategory / elapsedMs / cost
  }): Promise<SessionCheckpoint>;

  // 连续性摘要 & Solo 轮次笔记
  getContinuity(sessionId: string): Promise<ContinuitySummary | null>;
  updateContinuity(sessionId: string, patch: Partial<ContinuitySummary>): Promise<ContinuitySummary>;
  writeIterationNote(sessionId: string, note: IterationNote): Promise<void>;
  getIterationNotes(sessionId: string): Promise<IterationNote[]>;

  // 跨 Agent 交接包（不写死宿主枚举，保留 lineage / receiptRef / verificationRefs）
  writeHandoff(sessionId: string, packet: HandoffPacket): Promise<HandoffPacket>;
  readHandoff(sessionId: string): Promise<HandoffPacket | null>;

  // 会话关闭（更新状态；可选基于最后助手消息与触及文件生成一条 candidate 草稿）
  close(input: {
    sessionId: string;
    status?: 'done' | 'error' | 'blocked';
    generateCandidate?: boolean;     // 默认 false，仅在明确启用草稿队列时生成
  }): Promise<{ session: SessionMeta; candidate?: MemoEvent | null }>;

  // 上下文恢复包（smart/tail 事件选取 + legacy/balanced/aggressive token 预算 + Offload Refs）
  pack(input: {
    sessionId: string;
    eventLimit?: number;
    tokenBudget?: number;
    tokenStrategy?: 'legacy' | 'balanced' | 'aggressive';
    recallStrategy?: 'smart' | 'tail';
    kinds?: string[];
    refs?: string[];
    dedupeEvents?: boolean;
  }): Promise<{ packet: ContextPacketData; markdown: string }>;
}
```

### 3.3 `recall` —— 统一跨源召回与轮次上下文

```ts
export interface RecallApi {
  // 跨来源统一检索（memo / pinned / session_events / checkpoints + 注入的 externalSources）
  query(input: {
    query: string;
    sources?: Array<'memo' | 'pinned' | 'session' | 'checkpoint' | string>;
    space?: string;
    scope?: string;
    agent?: string;
    sessionId?: string;
    excludeSessionId?: string;
    workItemId?: string;
    limit?: number;
    maxCharsPerItem?: number;
    maxTotalChars?: number;
  }): Promise<{
    query: string;
    sources: string[];
    items: RecallItem[];             // 每条必含 id, source, kind, score, text, refs, workItemId, ts
  }>;

  // 轮次自动召回（多级降级查词：原文 -> ICU 词 -> CJK bigram）
  forTurn(input: {
    message: string;
    sessionId?: string;
    agent?: string;
    workItemId?: string;
    limit?: number;                  // 默认 6
    maxTotalChars?: number;          // 默认 2000
    recordImpressions?: boolean;     // 默认 true（仅对实际返回的 memo 记录 impression）
  }): Promise<{
    status: 'hit' | 'no-hit' | 'skipped' | 'error';
    items: RecallItem[];
    text: string;                    // 仅 status === 'hit' 时非空（给 Hook/Prompt 注入用，杜绝空注）
    block: string;                   // 始终含诊断状态（仅给日志/doctor/调试用）
  }>;

  // 工单视角一键恢复（聚合该工单的置顶、相关业务 Memo、最新会话连续性/检查点/交接包）
  forWorkItem(input: {
    workItemId: string;
    query?: string;
    memoLimit?: number;
  }): Promise<{
    workItemId: string;
    pinned: PinnedRenderResult;
    memos: MemoEvent[];
    latestSession: SessionMeta | null;
    latestCheckpoint: SessionCheckpoint | null;
    continuity: ContinuitySummary | null;
    handoff: HandoffPacket | null;
  }>;
}
```

### 3.4 `governance` —— 内容安全、候选审核与归档

```ts
export interface GovernanceApi {
  // 提示注入 / 不可见字符 / 密钥外泄命令扫描 + 密钥脱敏
  scanContent(text: string): { ok: boolean; threatId?: string; reason?: string };
  redactSecrets(text: string): string;

  candidates: {
    list(filter?: { space?: string; status?: 'pending' | 'promoted' | 'rejected' | 'expired'; includeText?: boolean }): Promise<CandidateSummary[]>;
    inspect(candidateId: string): Promise<CandidateDetail | null>;
    // 由注入的 ApprovalPolicy 判定授权；默认策略允许显式人工确认（actor='human' + reason）通过并生成 verified 记录
    decide(input: {
      candidateId: string;
      action: 'promote' | 'reject' | 'expire';
      reason: string;
      actor: { type: 'human' | 'agent' | 'policy'; id: string };
    }): Promise<{ ok: boolean; receipt: GovernanceReceipt; promotedEvent?: MemoEvent }>;
  };

  // 供外部 Dream / 清理流程执行可逆归档（不删原记录）
  archive: {
    mark(input: { memoIds: string[]; reason: string; actor: { type: string; id: string } }): Promise<GovernanceReceipt>;
    restore(input: { memoIds: string[]; reason: string; actor: { type: string; id: string } }): Promise<GovernanceReceipt>;
    list(filter?: { space?: string; limit?: number }): Promise<MemoEvent[]>;
  };
}
```

### 3.5 `artifact` —— 大输出外置（Offload）

```ts
export interface ArtifactApi {
  // 输出字节数 >= minBytes（默认 2048）时落盘外置并返回 refId 与预览；否则返回 null
  offload(input: {
    content: string;
    sessionId?: string;
    workItemId?: string;
    tool?: string;
    label?: string;
    status?: 'ok' | 'fail';
    minBytes?: number;
  }): Promise<ArtifactRef | null>;

  read(input: { refId: string; offsetLine?: number; limitLines?: number }): Promise<{ ref: ArtifactRef; fromLine: number; toLine: number; totalLines: number; text: string }>;
  grep(input: { pattern: string; sessionId?: string; limit?: number }): Promise<ArtifactGrepHit[]>;
  list(filter?: { sessionId?: string; workItemId?: string; limit?: number }): Promise<ArtifactRef[]>;
  prune(input?: { olderThanDays?: number; dryRun?: boolean }): Promise<{ removed: number; bytesFreed: number }>;
}
```

### 3.6 `admin` —— 巡检、维护、导出与可选用户画像

```ts
export interface AdminApi {
  doctor(): Promise<MemoryHealthReport>;
  hygiene(input?: { staleSessionDays?: number; maxEventsPerSession?: number }): Promise<HygieneReport>;
  archiveStaleSessions(input?: { minAgeDays?: number; dryRun?: boolean }): Promise<{ archived: string[] }>;
  rebuildIndex(): Promise<{ memoRows: number; eventRows: number; checkpointRows: number; tookMs: number }>;
  exportJsonl(input: { outDir: string }): Promise<{ files: string[]; counts: Record<string, number> }>;
  stats(): Promise<MemoryStats>;

  // 可选：用户级全局偏好（存在 ~/.jtao/profile/{SOUL,USER}.md，与工作区隔离）
  profile: {
    get(layer: 'persona' | 'user'): Promise<{ path: string; content: string; exists: boolean }>;
    set(layer: 'persona' | 'user', content: string, options?: { mode?: 'set' | 'append'; maxChars?: number }): Promise<{ path: string; chars: number }>;
  };
}
```

---

## 4. 适配层映射：CLI / MCP / Hook 如何统一调同一套内核

### 4.1 MCP 工具面（`jtao-memory-mcp`，彻底补齐 AIOS MCP 的短板）

| MCP 工具名 | 底层内核调用 | 相比 AIOS MCP 的关键改进 |
| --- | --- | --- |
| `memory_recall` | `recall.query` | **返回每条记录的 `id`、`source`、`score`、`refs`、`workItemId`**，不再吞掉 `eventId` |
| `memory_write` | `memo.write` | 支持 `supersedes`、`validAt/date`、`entities`、`evidenceRef`、`confidence`、`scope`、`workItemId`，与 CLI 能力 100% 对等 |
| `memory_useful` | `memo.feedback` | 补齐 AIOS MCP 缺失的采纳反馈闭环 |
| `memory_pin` | `memo.pin.append / get / set` | 语义明确为「置顶」，返回窗口回执与 `contentHash`，与会话检查点彻底解耦 |
| `session_checkpoint` | `session.checkpoint` | 独立命名，不再与置顶工具（AIOS 的 `memory_checkpoint`）混淆 |
| `session_pack` | `session.pack` / `recall.forWorkItem` | 支持按 `sessionId` 或 `workItemId` 一键取恢复上下文 |
| `artifact_read` | `artifact.read` | 支持按需分页查看外置的大工具输出 |

### 4.2 CLI 命令面（全部提供 `--json` 输出）

- `jtao memo add / search / list / useful / pin / candidates` → 调 `memo.*`、`governance.candidates.*`
- `jtao recall "<query>" [--work-item <id>]` → 调 `recall.query` / `recall.forWorkItem`
- `jtao session new / event / checkpoint / continuity / handoff / pack / close` → 调 `session.*`
- `jtao artifact read / grep / list / prune` → 调 `artifact.*`
- `jtao memory doctor / hygiene / rebuild-index / export` → 调 `admin.*`

### 4.3 宿主 Hook 接入规则（避免 AIOS 的“空注与空喊”）

- **启动 Hook（宿主支持 `SessionStart` 时）**：调 `memo.pin.render()` 把窗口内的置顶规则与最新里程碑**真正注入**首轮上下文，并可附最近一次 `continuity` 摘要。
- **提问 Hook（`UserPromptSubmit`）**：调 `recall.forTurn()`，**只读取 `result.text`**——有命中才写入 `additionalContext`，无命中直接放行不塞废话。
- **回复后 / 结束声明**：**只有**在宿主确认配置了回复后/结束回调（或由 `jtao` 自身调度器接管执行）时，才在提示中附带自动提议指令；在纯原生 Codex 聊天框里，默认引导模型通过 `memory_write` / `memory_useful` MCP 工具显式完成，绝不出现“让模型每轮末尾写 `<!--memory: ... -->` 却无人消费”的断层。

---

## 5. AIOS 全仓消费点能力对照矩阵（防迁移缺失核对）

对照 AIOS `a9951600` 全仓对 `scripts/lib/memory/` 与 `contextdb` 的所有外部引用，逐项核对迁移覆盖情况：

| AIOS 上层模块 / 消费点 | 在 AIOS 中依赖的记忆能力 | `jtao` Memory 对应接口 | 覆盖结论 |
| --- | --- | --- | --- |
| `memory-mcp-server` / `aios-pi` | 统一召回、写 Memo、写置顶、标有用、读外置输出 | `recall.query`, `memo.write`, `memo.pin`, `memo.feedback`, `artifact.read` | **超集覆盖**（补齐了 MCP `eventId` 与 `supersedes`） |
| `turn-recall` / `UserPromptSubmit` | 降级分词检索、预算裁剪、曝光记录、采纳 ID 交集 | `recall.forTurn`, `memo.feedback(allowedIds)` | **完整覆盖**（修复无命中空注与声明无人消费） |
| `Solo` 长任务循环（`solo-runtime`） | 读写 `continuity`、写 ContextDB checkpoint、大输出 Offload | `session.getContinuity/updateContinuity`, `session.checkpoint`, `session.writeIterationNote`, `artifact.offload` | **完整覆盖** |
| `Team` / `Orchestrator` / 质量门（`orchestrator-evidence`, `clarity-gate`, `verification-evidence`, `role-memory`） | 写事件与检查点（含 `telemetry` 与 `artifacts`）、按角色隔离记忆与置顶 | `session.appendEvent`, `session.checkpoint`, `memo.*(space: 'role:<r>')`, `memo.pin.*(space: 'role:<r>')` | **完整覆盖**（去掉假 Session 模拟角色空间的弯路） |
| `ctx-agent` / 会话起止 Hook（`start-session`, `close`, `handoff`, `facade`） | 幂等建会话、写 L2 事件、读写交接包、关闭产候选、按层级取摘要 | `session.ensure`, `session.appendEvent`, `session.writeHandoff/readHandoff`, `session.close`, `session.pack` | **完整覆盖** |
| `Dream` / `Hygiene` / `Evolution` / `Skill Workshop` | 读全量事件、读置顶、标记归档/恢复、列候选与晋升、内容安全扫描 | `memo.list`, `memo.pin.get`, `governance.archive.*`, `governance.candidates.*`, `governance.scanContent`, `admin.hygiene` | **完整覆盖**（且消除循环依赖，晋升可真实走通） |
| `Planning` / `Rex` 工单接续 | 按工单 ID 关联背景、会话检查点与回执引用 | 所有核心表内置 `workItemId` + `recall.forWorkItem` | **原生增强**（AIOS 原本只能靠文本搜索凑） |
| `perception/`（小红书内容分析）、`genealogy-gui`、`workspaces` 全局列表 | 垂直业务指标、独立 Web 血缘图、Console 桌面壳列表 | **刻意不进 Memory 内核** | 均非通用 Coding Harness 能力；血缘图可按需由 `session.list/timeline` 在外层派生 |

---

## 6. 分阶段实施路线（接口一次定型，实现分步交付）

虽然契约一次性设计完整，但实现按三阶段渐进落地，确保第一版精炼可控：

1. **M1（核心闭环，优先可验收）**：
   - SQLite WAL + Schema + 中英文预分词 FTS5；
   - `memo`（`write / get / list / search / feedback / pin` + 双时态 + 实体扩散 + 五要素校验）；
   - `session`（`create / ensure / get / latest / list / appendEvent / checkpoint / getContinuity / updateContinuity / pack`）；
   - `recall`（`query / forTurn / forWorkItem`）；
   - `governance.scanContent` + 统一 MCP Server 与 CLI 薄壳；
   - 跑通[第一条跨会话/跨工作区验收场景](./memory-and-persistence.md#第一条验收场景)。
2. **M2（长任务、多 Agent 与治理补齐）**：
   - `session.writeHandoff / readHandoff`、`session.writeIterationNote / getIterationNotes`、`session.close`；
   - `artifact` 大输出外置与分页读回（挂入 `session.pack`）；
   - `governance.candidates`（本地显式确认晋升 + 审计回执）与 `governance.archive`；
   - `admin.doctor / hygiene / archiveStaleSessions / rebuildIndex / exportJsonl`。
3. **M3（按需插拔扩展，不改核心接口）**：
   - 注入外部非记忆检索源 `ExternalSearchProvider`（按需搜 `plans / docs / code`）；
   - 注入可选本地向量 `Embedder`；
   - 外置 Dream 整理任务与可选用户级 `admin.profile`（`SOUL/USER`）。
