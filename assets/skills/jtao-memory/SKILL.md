---
name: jtao-memory
description: Use jtao MCP memory tools to recall prior project facts, record verified conclusions, mark useful memories, and save session checkpoints cleanly.
---

# jtao Memory Skill

Use the `jtao` MCP tools whenever working across sessions in this repository:

1. **Before non-trivial changes**: call `memory_recall` with a short focused query (or `session_pack` with `work_item_id`) to recover existing decisions, constraints, and pitfalls.
2. **When a recalled memory helped**: call `memory_useful` with the exact returned `id` so future retrieval ranks it higher.
3. **When discovering a durable fact or decision**: call `memory_write` with concrete `text`, `evidence_ref` (file path or command), and `entities`. If an older memory is outdated, pass `supersedes` with the old memory ID.
4. **Before stopping or handing off**: call `session_checkpoint` with summary, next actions, and touched files.
5. **Rules and milestones**: use `memory_pin` only for long-lived repository rules or key milestones.
