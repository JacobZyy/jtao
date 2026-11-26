---
name: jtao-work
description: Manage structured WorkItem task DAGs inside Codex chat using jtao MCP tools — break requirements into dependency-ordered nodes with real verification commands, claim nodes with execution tokens, record tamper-evident receipts, and advance cleanly.
---

# jtao Work Mode (Interactive WorkItem DAG)

Always pass the current repository's absolute path as `workspace_root` on every `jtao` MCP tool call.

## When to Use

The user explicitly invoked `$jtao-work` to run a structured, receipt-verified WorkItem in this interactive session (instead of ad-hoc edits).

## Workflow

1. **Inspect existing state**: call `workflow_status` first.
   - If the user asks about status or wants to continue the active WorkItem, show the DAG state, ready nodes, and `DoneGate` summary.
   - If starting a new requirement, create a WorkItem with `workflow_work_create` (`set_active: true`, `export_markdown: true`). Prefer concrete custom `nodes` with `depends_on`, `acceptance`, and real `verify_spec` commands (`go test`, `pnpm test`, `pytest`, etc.) when the task has clear steps; otherwise choose a blueprint (`feature`, `bugfix`, `refactor`, `security`, or `single`).
2. **Execute step by step**:
   - Call `workflow_node_claim` to claim the next ready node and obtain its one-time `execution_token`.
   - Implement the node's scoped changes directly in the repository.
   - Run real verification through `workflow_receipt_run` (passing `work_item_id`, `node_id`, `command`, `args`) to obtain a tamper-evident `receipt:<id>`. Never fabricate pass evidence in prose.
   - Call `workflow_node_submit` with `execution_token`, `status` (`pass` / `blocked` / `needs-input` / `fail`), `summary`, `receipt_id`, and `evidence_refs`.
3. **Pause or hand off cleanly**:
   - When pausing mid-WorkItem, call `session_checkpoint` with `work_item_id`, summary, and next actions.
   - If the user wants the remaining nodes finished unattended in the background, suggest invoking `$jtao-solo` (sequential loop) or `$jtao-team` (parallel multi-role DAG).
