---
name: jtao-workflow
description: Use jtao MCP workflow tools to manage WorkItem task DAGs, claim nodes with execution tokens, run real verification receipts, and advance tasks cleanly.
---

# jtao Workflow Skill

Use the `jtao` MCP workflow tools for structured, receipt-verified delivery:

1. **Check or start work**: call `workflow_status` to inspect the active WorkItem, or `workflow_work_create` to define tasks with dependencies and optional verification commands.
2. **Claim the current node**: call `workflow_node_claim` to receive the active node contract and its one-time `execution_token`.
3. **Produce real verification evidence**: when a node defines a verification command, run it through `workflow_receipt_run` to obtain a real `receipt:<id>` instead of asserting success in prose.
4. **Advance the graph**: call `workflow_node_submit` with `execution_token`, status (`pass` or `blocked`), real `evidence_refs` / `receipt_id`, and handoff summary.
5. **Hand off to background execution**: any unfinished WorkItem in `jtao` can be continued unattended via `scheduler_solo_start` / `scheduler_team_start` MCP tools (or `jtao solo run --work-item <id>`).
