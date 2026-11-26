# AIOS Harness 流程控制

[打开 Graph 与 Agent 执行闭环图](./architecture/aios-harness-flow/aios-harness-flow.html)

任务图回答哪个节点可以做，宿主调度器负责启动下一轮。每轮 Agent 领到一个节点和它的验收命令。Rex 核对实际执行回执，给出下一步决定。宿主保存决定，再按它继续、重试或停下来。

## 一轮怎么闭合

| 模块 | 做什么 |
| --- | --- |
| 建立任务图 | 把目标拆成有依赖关系的节点，每个节点写明验收场景。Rex 校验依赖无环和基线回执，从无前置任务的节点中选出第一个。 [初始化与依赖](https://github.com/rexleimo/rex-harness/blob/f143b9f6bb53461cf6f7fe1e3fb05a5568d6cfa3/src/domain/long-running-delivery.mjs#L60-L183) |
| 派发一轮 | 宿主读取已保存的决定，只把当前节点交给 Codex。Codex 完成这一步，运行对应的验收命令并返回结果。运行时生成的回执包含实际命令、参数、工作目录及退出码。 [单轮适配](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/workflows/adapters/rex-long-running-delivery-store.mjs#L89-L110) · [命令回执](https://github.com/rexleimo/rex-harness/blob/f143b9f6bb53461cf6f7fe1e3fb05a5568d6cfa3/src/standalone/store.mjs#L197-L240) |
| 验收与推进 | Rex 核对节点 ID 和回执是否对应指定命令。验收命令成功就标记当前节点已通过，再从前置节点均已验收的待办中选一个。回执有效但命令失败时，在重试额度内重派当前节点。缺失、错节点或错命令的证据会阻塞。验收条件不明确或重试耗尽时转人工。全部节点验收后才返回完成。 [验收和结果分支](https://github.com/rexleimo/rex-harness/blob/f143b9f6bb53461cf6f7fe1e3fb05a5568d6cfa3/src/domain/long-running-delivery.mjs#L223-L345) |
| 保存与接续 | AIOS 适配器按交付 ID 保存 Rex 返回的状态和决定。接入方看到 `continue` 或 `retry` 就调度下一轮，看到 `completed` 就收尾，看到 `blocked` 或 `human-gate` 就交接原因。重启后按同一个 ID 读回状态。 [保存和读取](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/workflows/adapters/rex-long-running-delivery-store.mjs#L37-L110) |

## 假设你让 Codex 改造结账

1. 任务图先约定 **A. 拒绝非法结账输入 → B. 生成结账回执**，分别指定验收命令。基线通过后，宿主派发 A。
2. Codex 改完 A，验收命令仍失败。有效的失败回执让 Rex 决定重试 A，宿主保存这个决定，再开一轮 Codex 修复。B 此时还在等待。
3. A 的验收通过后，Rex 接受 A，把 B 设为当前节点。宿主先保存，再派发 B。假如这时进程退出，恢复入口能读到「A 已验收，当前是 B」。
4. B 通过后整个目标完成。若 B 的验收需要业务决定，宿主停在 B 并交接具体问题。

图中的回箭头表示宿主再次派发。结账任务依赖始终是 A 到 B。

## AIOS 当前实现

源码基线为 AIOS `375ab47`，Rex 使用该提交固定的 `f143b9f`。

- **Rex 长任务 API** 实现了图里的依赖与验收规则。AIOS 有按交付 ID 落盘并执行一次的适配器。当前直接调用该适配器的是测试，需由接入方重复调用才能跑完整个图。 [适配测试](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/tests/execution/rex/harness/rex-long-running-delivery-store.test.mjs#L82-L135)
- **AIOS Solo** 的 `aios harness run` 已有自动启动 Agent 的轮次循环。它带着目标和上轮摘要执行下一轮，把结果、日志和连续性摘要写盘，再按轮次结果、停止信号、连续失败和次数上限处理。进程退出后，`aios harness resume --session <ID>` 按原会话启动。 [单轮调用](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/harness/solo/solo-runtime/loop/loop.mjs#L278-L318) · [保存进度](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/harness/solo/solo-runtime/support/state.mjs#L56-L115) · [继续或停止](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/harness/solo/solo-runtime/loop/loop.mjs#L477-L572) · [恢复入口](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/lib/execution/lifecycle/run/harness/execution/commands.mjs#L275-L344)
- **Codex 聊天内接入**可参考 Rex 自带的 `rex-workflow` Skill。它引导 Codex 用 `start` 或 `resume` 取当前命令，执行后提交证据，再取下一条。这个入口的循环由当前会话里的 Codex 发起。 [Skill 的执行循环](https://github.com/rexleimo/rex-harness/blob/f143b9f6bb53461cf6f7fe1e3fb05a5568d6cfa3/skill-sources/rex-workflow/SKILL.md#L16-L61)

## jtao 设计草案

如果要离开 Codex 聊天继续跑，并把 AIOS 里彼此割裂的「Plan Contract / Rex 长任务图 / Solo 循环 / Team 并发编排」收拢成一套无转换、真回执验收集中的体系，已拍板的完整技术设计见 [`jtao` 工作流与调度执行系统技术设计规范](./jtao-workflow-harness-design.md)。

> **2026-11-25 复核补充（基于 AIOS `a9951600`）**：
> 1. `rex-long-running-delivery-store.mjs` 在生产路径下依然只有单测调用，尚未接入 Solo 循环或 Team 编排；
> 2. AIOS 内部存在 Plan `tasks[]` → Team `workItems[]` → `phases[]/jobs[]` → Rex `activation` 多层翻译，且 Solo（`ctx-agent.mjs`）与 Team（`one-shot-runner.mjs`）各自维护子进程执行器（Team 缺少 Worktree 隔离）；
> 3. `jtao` 通过独立解耦的 `workflow`（统一任务图 + 通用真回执门禁）+ `runner`（统一执行与 Worktree 底座）+ `scheduler`（Solo + Team 控制面）彻底解决以上割裂。

## 核验范围

在固定的源码版本中运行了 10 项 Rex 和单轮适配测试，以及 12 项 Solo 运行、停止、恢复和检查点测试，均通过。Solo 测试使用注入的轮次执行器，验证的是控制流和状态续接。 [Rex 依赖测试](https://github.com/rexleimo/rex-harness/blob/f143b9f6bb53461cf6f7fe1e3fb05a5568d6cfa3/tests/contract/workflow-outcome-dependencies.test.mjs) · [Solo 生命周期测试](https://github.com/rexleimo/aios/blob/375ab47f8e11d9ce037e0a3c5ed6d652a2043853/scripts/tests/execution/harness/infra/command/command-lifecycle.mjs)
