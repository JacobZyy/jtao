# jtao

`jtao` 是一个用 **Go 单文件静态二进制**实现的本地 Coding Agent 工程 Harness，为 Codex / Claude Code 等现有编程 Agent 提供**跨会话记忆、统一任务图（DAG）与真命令回执验收、Solo 长循环调度、MCP Server 与极速宿主 Hooks**。

## 架构概览

- `internal/memory`：独立记忆内核（`.jtao/memory/memory.db`，SQLite WAL + FTS5 + 中英 2-gram 分词 + 双时态折叠 + 实体扩散 + 安全扫描 + 大输出外置 `artifacts/`）
- `internal/workflow`：独立工作流内核（`.jtao/workflow/workflow.db`，纯规则分流 + 统一 WorkItem DAG + 一次性 `executionToken` + 真实命令回执门禁 + Markdown 投影）
- `internal/runner`：统一执行底座（Codex / Claude CLI 适配、`Setpgid` 进程组超时强杀 fail-closed、Git Worktree 隔离池、真实回执执行器）
- `internal/scheduler`：调度控制面（图驱动 Solo 循环、租约防双开、`TURN_CONSEQUENCES` 单一后果裁决表、断点续跑）
- `internal/mcp` / `internal/hook` / `internal/adapter` / `internal/cli`：全收口于唯一二进制 `jtao`（内置 `//go:embed` 蓝图与 Skills）

完整设计文档见 [`docs/README.md`](./docs/README.md)。

## 构建与测试

```bash
# 运行全部单测与竞态检测
go test -race ./...

# 零 CGO 编译单文件静态二进制
CGO_ENABLED=0 go build -o bin/jtao ./cmd/jtao
```

## 快速上手

```bash
# 1. 环境体检（检查 SQLite WAL/FTS5、Codex/Claude CLI）
./bin/jtao doctor

# 2. 记忆读写与检索
./bin/jtao memo add "结账接口金额 <= 0 时返回 422" --entity checkout.json --evidence file:checkout.go
./bin/jtao memo search "结账金额"
./bin/jtao recall "checkout.json"

# 3. 任务图 + 真实命令回执验收 + Solo / Team 调度
./bin/jtao work create --id demo --title "Demo delivery" --blueprint feature
./bin/jtao work status --id demo
./bin/jtao team plan --work-item demo
./bin/jtao solo run --work-item demo --dry-run
./bin/jtao solo run --work-item demo --unattended --quota 5 --quiet-threshold 3 --worktree
./bin/jtao team run --work-item demo --concurrency 3 --worktree

# 4. 记忆维护与大输出检索
./bin/jtao memory hygiene
./bin/jtao artifact list

# 5. 启动 stdio MCP Server（提供 18 个 memory_* / workflow_* / scheduler_* 工具）
./bin/jtao mcp

# 6. 安装为 Codex 原生插件（打包 MCP + Hooks + 4 个 Skills，一键启停）
make install
codex plugin marketplace add "$(pwd)"
codex plugin add jtao@jtao

# 7. 或按仓库配置 Claude Code（先 --dry-run 预览）
./bin/jtao init --client claude --dry-run
```
