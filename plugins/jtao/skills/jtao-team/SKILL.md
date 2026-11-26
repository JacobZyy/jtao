---
name: jtao-team
description: Split a multi-part task into an ownership-checked WorkItem DAG and launch, inspect, stop, or retry-blocked detached background jtao Team runs directly inside Codex chat.
---

# jtao Team Mode (Detached Concurrent Multi-Role DAG)

Always pass the current repository's absolute path as `workspace_root` on every `jtao` MCP tool call. Never block the chat turn by running `jtao team run` synchronously in shell — always use the non-blocking `scheduler_*` MCP tools.

## Routing User Intent

1. **Check progress / blocked nodes**: call `scheduler_status`. Summarize per-node status, `liveProcess`, Merge Gate / `DoneGate` verdict, and `logTail` if anything is blocked.
2. **Stop a running Team DAG**: call `scheduler_stop` (Team stops terminate cleanly via `SIGTERM`).
3. **Retry only blocked/failed nodes** (preserving already completed nodes): call `scheduler_resume` (set `force: true` only when the user explicitly confirms overriding the repeated-block instability guard).
4. **Start a new background Team run**:
   - Inspect existing WorkItem with `workflow_status`, or decompose the user's goal into a custom DAG via `workflow_work_create` (`set_active: true`, `export_markdown: true`).
   - **Mandatory Team ownership invariant**:
     - Any two editable nodes (`can_edit_files: true`) that can run in the same wave (no dependency edge between them) MUST declare non-overlapping `owned_path_prefixes` (for example `["internal/auth/"]` vs `["internal/billing/"]`).
     - Read-only research or review nodes MUST set `can_edit_files: false` (and `node_kind: "review"` for reviewers so read-only skip optimization works).
     - Attach real `verify_spec` commands on verification/integration nodes so Merge Gate and DoneGate verify real receipts.
   - Launch with `scheduler_team_start` (non-blocking; default `concurrency: 3`, optional `use_worktree: true`, `snapshot: true`).
   - Report `runId`, `workItemId`, `pid`, node wave structure, and `logPath`.
