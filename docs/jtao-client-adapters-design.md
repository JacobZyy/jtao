# `jtao` 客户端适配层与 Go 单二进制工程规范（CLI · MCP · Hooks · Skills v1）

2026-11-25 · **状态：已拍板定案** · 技术栈：**Go 1.23+ 单文件静态二进制（`CGO_ENABLED=0`，纯 Go `modernc.org/sqlite` + `cobra` + `go:embed`）**

> **已拍板核心决策**
> 1. **单二进制全收口**：整个 `jtao` 编译为**唯一一个静态二进制文件 `jtao`**，集齐命令行（`memo/work/solo/team`）、常驻 stdio MCP Server（`jtao mcp`）、极速宿主 Hook 处理（`jtao hook`）与环境接入（`jtao init / doctor`）。
> 2. **零外部运行时依赖**：无 Node/NVM PATH 坑，Hook 触发冷启动 **<10ms**；内置 Skills 与蓝图模板直接通过 `//go:embed` 打进二进制，丢进 `PATH` 即可开箱使用。
> 3. **薄壳原则**：CLI / MCP / Hooks / Skills 不含核心业务规则和存储读写逻辑，100% 委托给 `internal/memory`、`internal/workflow`、`internal/runner`、`internal/scheduler` 四大解耦内核包。

---

## 1. Go 项目目录骨架与依赖方向

利用 Go 原生 `internal/` 访问控制强制锁定单向依赖，编译器直接禁止循环依赖或跨层偷调：

```text
jtao/
├── go.mod                          # Go 1.23+（modernc.org/sqlite, spf13/cobra, go-sdk/mcp）
├── cmd/
│   └── jtao/
│       └── main.go                 # 唯一入口，装配依赖并运行 Cobra 根命令
├── internal/
│   ├── memory/                     # 独立记忆内核（memory.db + FTS5 + 6 大子域）
│   │   ├── store/                  # SQLite WAL + Schema 迁移 + 中英分词器（英文+CJK 2-gram）
│   │   ├── memo/                   # 双时态折叠、实体扩散、五要素校验、置顶
│   │   ├── session/                # 会话、事件、检查点、Continuity、Handoff、Pack
│   │   ├── recall/                 # 统一 BM25+实体+反馈检索、forTurn、forWorkItem
│   │   ├── governance/             # 安全扫描、候选晋升、归档治理
│   │   ├── artifact/               # 大文本外置存储与分页读回
│   │   └── service.go              # MemoryService 固定对外接口
│   ├── workflow/                   # 独立工作流内核（workflow.db + Task DAG + Receipt Gate）
│   │   ├── policy/                 # 显式规则分流（5 disposition × 3 host，不猜自然语言）
│   │   ├── dag/                    # WorkItem / TaskNode / TaskEdge + 无环校验 + DoneGate
│   │   ├── receipt/                # 真实命令回执存储与核验 + 一次性 executionToken
│   │   ├── template/               # 内置蓝图（go:embed）与 docs/plans/*.md 单向投影
│   │   └── service.go              # WorkflowService 固定对外接口
│   ├── runner/                     # 统一执行底座（Solo/Team 共用）
│   │   ├── proc/                   # Setpgid 进程组超时强杀 + fail-closed 存活检测
│   │   ├── worktree/               # Git Worktree 隔离池（变更保留 / 洁净自动清理）
│   │   ├── clients/                # Codex CLI（--output-schema）、Claude Code CLI 适配
│   │   └── receipt_exec.go         # 真实命令执行并签发 receipt:<id>
│   ├── scheduler/                  # 调度控制面（仅依赖 workflow + runner + 可选 MemoryPort）
│   │   ├── consequence/            # TURN_CONSEQUENCES 单一后果裁决表
│   │   ├── solo/                   # 图驱动长循环、租约、should-run 节拍、safe_bypass、resume
│   │   └── team/                   # 有界 goroutine 并发池、ownedPathPrefixes、Merge Gate、retry-blocked
│   ├── mcp/                        # `jtao mcp` stdio MCP Server（memory_* + workflow_*）
│   ├── hook/                       # `jtao hook <event>` 极速宿主回调处理
│   ├── adapter/                    # `jtao init` / `jtao doctor`（Codex/Claude 幂等注册+回读校验）
│   └── cli/                        # Cobra 子命令定义与 text/--json 输出格式化
├── assets/
│   ├── blueprints/                 # 内置蓝图 JSON（//go:embed）
│   └── skills/                     # 薄 Skill 模板（jtao-memory / jtao-workflow，//go:embed）
└── docs/                           # 已拍板的全部设计规范
```

**严格单向依赖**：
- `internal/memory`、`internal/workflow`、`internal/runner` 三者平级独立，彼此 **0 import**；
- `internal/scheduler` 只依赖 `workflow`、`runner` 及抽象端口 `MemoryPort`；
- `internal/mcp`、`internal/hook`、`internal/adapter`、`internal/cli` 都是薄壳，只做参数解析、身份/工作区注入与结果渲染。

---

## 2. 统一 CLI 命令面（全命令支持 `--json` 与显式 `--workspace`）

```text
jtao
├── memo
│   ├── add <text> [--space ...] [--scope ...] [--supersedes <id>] [--work-item <id>] [--evidence ...]
│   ├── search <query> [--space ...] [--work-item <id>] [--limit 10]
│   ├── list [--space ...] [--status verified|candidate]
│   ├── useful <id...> [--session <id>]
│   ├── pin [get|set|append|status]
│   └── candidates [list|promote <id>|reject <id>]
├── recall <query> [--work-item <id>] [--session <id>] [--budget 1200]
├── session
│   ├── new | latest | list | event | checkpoint | continuity | handoff | pack | close
├── artifact
│   ├── read <ref> [--offset 0] [--limit 4000] | grep <ref> <pattern> | list | prune
├── work
│   ├── create --title ... [--blueprint single|delivery|feature|bugfix|refactor|security] [--graph <file.json>]
│   ├── status [--id <workItemId>]
│   ├── list
│   ├── claim [--id <workItemId>] [--node <nodeId>]
│   ├── submit --token <token> [--receipt <receiptId>] [--status pass|blocked] ...
│   ├── receipt run -- <cmd...>   # 在工作区执行真实命令并落盘 receipt:<id>
│   └── export-md [--id <workItemId>]
├── solo
│   ├── run --work-item <id> [--provider codex|claude] [--worktree] [--max-iterations 20] [--unattended] [--dry-run]
│   ├── resume --run <runId> [--dry-run]
│   ├── stop --run <runId> [--reason ...]
│   └── status [--run <runId> | --work-item <id>]
├── team
│   ├── plan --work-item <id> [--blueprint feature]
│   ├── run --work-item <id> [--blueprint feature] [--provider codex|claude] [--concurrency 3] [--worktree]
│   └── retry-blocked --run <runId> [--force] [--dry-run]
├── mcp                           # 启动常驻 stdio MCP Server（可按 --only memory|workflow 裁剪）
├── hook
│   ├── user-prompt-submit        # stdin JSON → stdout Hook JSON（0 命中彻底静默）
│   ├── session-start             # 注入窗口内置顶规则与最近 continuity
│   ├── stop                      # Save Guard 自动写 checkpoint
│   └── post-tool-use             # 大输出自动卸载（返回 artifact: 短引用）
├── init [--client codex|claude|all] [--workspace <dir>] [--dry-run]
└── doctor [--workspace <dir>]    # 体检二进制路径、SQLite/FTS5、Codex/Claude CLI、MCP/Hook 漂移
```

---

## 3. MCP 工具面（`jtao mcp`）

单进程 stdio MCP Server（基于官方 `github.com/modelcontextprotocol/go-sdk/mcp`），补齐 AIOS MCP 残血短板，工具参数统一显式带 `workspace_root`（未传时回退服务启动时的默认工作区）：

| 分组 | MCP 工具名 | 对应内核接口 | 关键约定 |
| --- | --- | --- | --- |
| **Memory** | `memory_recall` | `memory.Recall.Query` | 返回完整 `id`、`source`、`score`、`refs`、`workItemId`，不再吞掉 ID |
| **Memory** | `memory_write` | `memory.Memo.Write` | 完整支持五要素、`supersedes`、`scope`、`workItemId`，与 CLI 100% 对等 |
| **Memory** | `memory_useful` | `memory.Memo.Feedback` | 闭环采纳反馈，直接提升高价值记忆召回权重 |
| **Memory** | `memory_pin` | `memory.Memo.Pin` | 明确管理置顶规则/里程碑，与会话检查点彻底分离 |
| **Memory** | `session_checkpoint` | `memory.Session.Checkpoint` | 记录阶段进展、阻断点与下一步动作 |
| **Memory** | `session_pack` | `memory.Session.Pack` / `Recall.ForWorkItem` | 按 `sessionId` 或 `workItemId` 一键取紧凑恢复上下文 |
| **Memory** | `artifact_read` | `memory.Artifact.Read` | 按需分页查看被卸载的大工具输出原文 |
| **Workflow** | `workflow_auto_gate` | `workflow.Policy.Classify` + `WorkItem` | 纯规则分流，显式意图下开/复用工作项并下发当前节点契约 |
| **Workflow** | `workflow_work_create` | `workflow.WorkItem.Create` | 创建工作项与任务依赖图（支持指定蓝图或节点列表） |
| **Workflow** | `workflow_status` | `workflow.WorkItem.Get` + `EvaluateDoneGate` | 查看任务图进度、就绪节点与完成门禁缺口 |
| **Workflow** | `workflow_node_claim` | `workflow.Graph.ClaimNode` | 领取当前就绪节点与一次性 `executionToken` |
| **Workflow** | `workflow_node_submit` | `workflow.Graph.SubmitNodeResult` | 提交节点结果与真实回执引用，推进 DAG 状态机 |
| **Workflow** | `workflow_receipt_run` | `runner.ReceiptExec.RunAndRecord` | 在工作区受控执行验收命令并返回真实 `receipt:<id>` |

---

## 4. 宿主 Hook 与 Skills 差异化接入（防 AIOS 断层）

### 4.1 宿主能力矩阵与安全接入规则

| 能力 | Codex CLI | Claude Code | `jtao` 落地策略 |
| --- | --- | --- | --- |
| **MCP Server** | 原生支持（`codex mcp add` / `config.toml`） | 原生支持（`claude mcp add` / `settings.json`） | `jtao init` 优先调用客户端自带 CLI 注册，写入后回读校验防 drift |
| **`UserPromptSubmit` Hook** | 按客户端版本探测支持 | 原生支持 | 调 `jtao hook user-prompt-submit`；**无记忆命中且无活跃工单时返回空对象彻底静默**，不塞空块 |
| **后置记忆声明解析** | 原生聊天无后置消费钩子 | 可选 `Stop` Hook | **绝不向原生 Codex 注入 `<!--memory: ...-->` 声明提示**；聊天内统一引导调 `memory_write` / `memory_useful` MCP 工具；仅在 `jtao solo/team` 自管闭环中解析声明标签 |
| **`SessionStart` / `Stop` / `PostToolUse`** | 按需降级为 Skill + MCP | 原生支持嵌套 Hook schema | 支持即注册 `session-start`（注入置顶规则）、`stop`（自动 checkpoint）、`post-tool-use`（大输出卸载） |
| **可执行路径稳定性** | 非交互 shell 易丢用户 PATH | 非交互 shell 易丢用户 PATH | `jtao init` 直接写入 `os.Executable()` 解析出的绝对路径（如 `/usr/local/bin/jtao`），零 NVM/Runtime 依赖，冷启动 <10ms |

### 4.2 幂等安装与体检（`jtao init` & `jtao doctor`）

借鉴 AIOS 客户端注册平面的工程规范：
1. **写前备份**：修改任何客户端 JSON/TOML 配置前自动生成时间戳备份（`.bak-YYYYMMDD-HHMMSS`），只增量 upsert `jtao` 命名空间，绝不破坏用户其他 MCP/Hook 条目；
2. **写后回读校验**：写入后立即重新解析目标配置并比对指纹，若存在冲突或解析异常直接报告 `conflict / drift`；
3. **支持 `--dry-run`**：改动前可完整预览将要写入的 MCP、Hook 与 Skill 路径；
4. **内嵌薄 Skills**：`assets/skills/jtao-memory/SKILL.md` 与 `assets/skills/jtao-workflow/SKILL.md` 只写「何时调用哪组 MCP 工具与回执规范」，经 `//go:embed` 打包进二进制，由 `jtao init` 按需展开到客户端 Skills 目录。

---

## 5. 整体 Go 落地里程碑（M1 → M2 → M3）

1. **M1（单二进制可用闭环）**：
   - 初始化 `go.mod` 与 `cmd/jtao` + `internal/{memory,workflow,runner,scheduler,mcp,hook,adapter,cli}` 骨架；
   - 落地 `internal/memory` M1（`memory.db` WAL + FTS5 + 中英 2-gram 分词 + 6 子域核心）；
   - 落地 `internal/workflow` M1（`workflow.db` + `WorkItem` DAG + 无环校验 + `executionToken` + 真实回执验收 + Markdown 投影）；
   - 落地 `internal/runner` M1（Codex CLI 适配 + `Setpgid` 进程组强杀 + `ReceiptExecutor` + `WorktreeManager`）；
   - 落地 `internal/scheduler/solo` 基础图驱动循环（租约 + `TURN_CONSEQUENCES` + `run/status/stop/resume`）；
   - 落地 `jtao cli` + `jtao mcp` + `jtao init` / `jtao doctor`，跑通端到端测试。
2. **M2（并发编排、无人值守与全治理）**：
   - `internal/memory`：补齐 `artifact` 卸载、`governance.candidates` 晋升、`admin.hygiene/export`；
   - `internal/scheduler/solo`：补齐 `pacing`（配额/cadence/空转停机/`safe_bypass`）；
   - `internal/scheduler/team`：4 套内嵌蓝图、goroutine 并发池、`ownedPathPrefixes` + Merge Gate、`--retry-blocked`、多写节点 Worktree；
   - `internal/runner`：补齐 Claude Code 适配器与大输出卸载联动。
3. **M3（可选模板与扩展）**：
   - 沉淀 Rex 风格高严谨模板（如 `tdd-delivery`、`security-audit`）进 `assets/blueprints/`；
   - 按需扩展外部检索源（`ExternalSearchProvider`）、可选本地向量检索或守护进程模式。
