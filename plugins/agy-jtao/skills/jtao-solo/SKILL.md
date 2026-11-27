---
name: jtao-solo
description: >-
  Manual-only background runner mode. Activate ONLY when the user explicitly
  requests jtao-solo or detached background Solo loop execution, status, stop,
  or resume; do NOT trigger automatically for normal interactive tasks.
---

# jtao Solo Mode (Detached Background Loop)

Always pass the current repository's absolute path as `workspace_root` on every `jtao` MCP tool call. Never block the chat turn by running `jtao solo run` synchronously in shell — always use the non-blocking `scheduler_*` MCP tools.

## Routing User Intent

1. **Check progress / logs**: call `scheduler_status` (optionally with `run_id` or `work_item_id`). Report current status, worker liveness (`liveProcess`), iteration progress, WorkItem node states, and relevant `logTail` excerpt if blocked or failed.
2. **Stop a running Solo loop**: call `scheduler_stop` (pass `immediate: true` if the user wants it terminated immediately rather than waiting for the current turn to finish).
3. **Resume a stopped, blocked, or orphaned run**: call `scheduler_resume` (set `unattended: true` when requested).
4. **Start a new background Solo run**:
   - First call `workflow_status` to see whether an unfinished active WorkItem already matches the user's request.
   - If no matching WorkItem exists, create one via `workflow_work_create` (`set_active: true`, `export_markdown: true`) with clear node objectives, `acceptance`, and concrete `verify_spec` commands so the Solo loop verifies every step with real receipts.
   - Call `scheduler_solo_start` (non-blocking). Set:
     - `unattended: true` when the user wants unattended / overnight / hands-off pacing (quota, cadence backoff, quiet stop, read-only safe-bypass).
     - `use_worktree: true` when the user asks for git worktree isolation.
     - `max_iterations` / `provider` (`codex` default, or `claude`) if specified by the user.
   - Immediately report `runId`, `workItemId`, `pid`, `status`, and `logPath`, and remind the user they can keep working or check/stop/resume anytime in chat.
