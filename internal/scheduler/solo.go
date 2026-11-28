package scheduler

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/JacobZyy/jtao/internal/runner"
	"github.com/JacobZyy/jtao/internal/workflow"
)

const (
	defaultMaxIterations      = 20
	maxConsecutiveInfraErrors = 3
)

// SoloRunInput configures a graph-driven Solo loop run.
type SoloRunInput struct {
	WorkspaceRoot  string
	WorkItemID     string
	RunID          string // Optional pre-allocated Run ID (auto-generated if empty)
	Provider       string // codex | claude
	UseWorktree    bool
	BaseRef        string
	MaxIterations  int
	TurnTimeout    time.Duration
	Pacing         PacingConfig
	DryRun         bool
	RunnerOverride runner.AgentRunner // Optional injection for tests
}

// SoloReadinessReport describes preflight checks without invoking any model.
type SoloReadinessReport struct {
	Verdict     string   `json:"verdict"` // ready | blocked
	WorkItemID  string   `json:"workItemId"`
	Provider    string   `json:"provider"`
	ReadyNodes  []string `json:"readyNodes,omitempty"`
	Reasons     []string `json:"reasons,omitempty"`
	NextActions []string `json:"nextActions,omitempty"`
}

// SoloRunResult is the final summary returned by Solo Run or Resume.
type SoloRunResult struct {
	Run       RunRecord         `json:"run"`
	Turns     int               `json:"turns"`
	WorkItem  workflow.WorkItem `json:"workItem"`
	FinalNote string            `json:"finalNote"`
}

// SoloService executes graph-driven, receipt-verified Solo agent loops.
type SoloService struct {
	Workflow  *workflow.SQLiteService
	Memory    MemoryPort // Optional
	Worktree  runner.WorktreeManager
	SleepFunc func(ctx context.Context, d time.Duration) error // Optional test override
	NowFunc   func() time.Time                                 // Optional test override
}

var soloTurnOutputSchema = []byte(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["outcome", "summary", "nextAction", "evidenceRefs", "filesTouched", "selfReport"],
  "properties": {
    "outcome": { "type": "string", "enum": ["success", "noop", "blocked", "human-gate", "failed"] },
    "summary": { "type": "string" },
    "nextAction": { "type": "string" },
    "evidenceRefs": {
      "type": "array",
      "description": "Strict ASCII scheme:value evidence references only (e.g. file:path/to/file, receipt:<id>, artifact:<id>). Never include natural language, command execution summaries, Chinese fullwidth colons (：), or placeholders; omit verifySpec receipts because the runner injects them automatically.",
      "items": {
        "type": "string",
        "description": "Strict ASCII scheme:value reference (e.g. file:path/to/file, receipt:<id>); never free text or command output."
      }
    },
    "filesTouched": { "type": "array", "items": { "type": "string" } },
    "selfReport": {
      "type": "object",
      "additionalProperties": false,
      "required": ["progressMade", "blockedReason", "summary"],
      "properties": {
        "progressMade": { "type": "boolean" },
        "blockedReason": { "type": "string" },
        "summary": { "type": "string" }
      }
    }
  }
}`)

// DryRun checks workItem readiness and CLI availability with zero model calls and zero state writes.
func (s *SoloService) DryRun(ctx context.Context, in SoloRunInput) (SoloReadinessReport, error) {
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		provider = "codex"
	}
	report := SoloReadinessReport{
		Verdict:    "ready",
		WorkItemID: in.WorkItemID,
		Provider:   provider,
	}

	detail, err := s.Workflow.GetWorkItem(ctx, in.WorkspaceRoot, in.WorkItemID)
	if err != nil {
		return SoloReadinessReport{}, err
	}
	if detail == nil {
		report.Verdict = "blocked"
		report.Reasons = append(report.Reasons, fmt.Sprintf("work item not found: %s", in.WorkItemID))
		report.NextActions = append(report.NextActions, "Create a work item via `jtao work create` first.")
		return report, nil
	}

	readyNodes, err := s.Workflow.ListReadyNodes(ctx, in.WorkspaceRoot, in.WorkItemID)
	if err != nil {
		return SoloReadinessReport{}, err
	}
	for _, n := range readyNodes {
		report.ReadyNodes = append(report.ReadyNodes, n.ID)
	}
	if len(readyNodes) == 0 && detail.Status != workflow.WorkDone {
		report.Verdict = "blocked"
		report.Reasons = append(report.Reasons, "no ready nodes available in task graph")
		report.NextActions = append(report.NextActions, "Inspect blocked nodes via `jtao work status`.")
	}

	if in.RunnerOverride == nil {
		ag, err := runner.ResolveRunner(provider)
		if err != nil {
			report.Verdict = "blocked"
			report.Reasons = append(report.Reasons, err.Error())
		} else if check := ag.CheckReadiness(); !check.OK {
			report.Verdict = "blocked"
			report.Reasons = append(report.Reasons, check.Reason)
			report.NextActions = append(report.NextActions, check.NextActions...)
		}
	}

	return report, nil
}

// Run starts a new graph-driven Solo loop for the given WorkItem.
func (s *SoloService) Run(ctx context.Context, in SoloRunInput) (SoloRunResult, error) {
	if in.DryRun {
		rep, err := s.DryRun(ctx, in)
		if err != nil {
			return SoloRunResult{}, err
		}
		if rep.Verdict == "blocked" {
			return SoloRunResult{FinalNote: "dry-run blocked: " + strings.Join(rep.Reasons, "; ")}, fmt.Errorf("dry-run blocked: %s", strings.Join(rep.Reasons, "; "))
		}
		return SoloRunResult{FinalNote: "dry-run ready"}, nil
	}

	ag, provider, err := resolveAgentRunner(in)
	if err != nil {
		return SoloRunResult{}, err
	}
	if in.RunnerOverride == nil {
		if check := ag.CheckReadiness(); !check.OK {
			return SoloRunResult{}, fmt.Errorf("provider readiness failed: %s", check.Reason)
		}
	}

	detail, err := s.Workflow.GetWorkItem(ctx, in.WorkspaceRoot, in.WorkItemID)
	if err != nil {
		return SoloRunResult{}, err
	}
	if detail == nil {
		return SoloRunResult{}, fmt.Errorf("work item not found: %s", in.WorkItemID)
	}

	maxIter := in.MaxIterations
	if maxIter <= 0 {
		maxIter = defaultMaxIterations
	}

	runID := strings.TrimSpace(in.RunID)
	if runID == "" {
		runID = newRunID("run:solo")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	run := RunRecord{
		ID:            runID,
		WorkItemID:    detail.ID,
		Mode:          "solo",
		Provider:      provider,
		Status:        "running",
		MaxIterations: maxIter,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	db, err := s.Workflow.DB(ctx, in.WorkspaceRoot)
	if err != nil {
		return SoloRunResult{}, err
	}
	if err := insertRun(ctx, db, run); err != nil {
		return SoloRunResult{}, fmt.Errorf("create solo run: %w", err)
	}

	if in.UseWorktree {
		wt, err := s.Worktree.Prepare(in.WorkspaceRoot, run.ID, in.BaseRef)
		if err != nil {
			run.Status = "failed"
			run.SummaryText = err.Error()
			run.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_ = updateRun(ctx, db, run)
			return SoloRunResult{Run: run, WorkItem: detail.WorkItem}, err
		}
		run.Worktree = &wt
		_ = updateRun(ctx, db, run)
	}

	return s.executeLoop(ctx, in.WorkspaceRoot, run, ag, in.TurnTimeout, in.Pacing)
}

// Resume clears stop flags, reconciles crashed turns, and continues an existing Solo run.
func (s *SoloService) Resume(ctx context.Context, workspaceRoot, runID string, maxIterations int, turnTimeout time.Duration, pacing PacingConfig, runnerOverride runner.AgentRunner) (SoloRunResult, error) {
	db, err := s.Workflow.DB(ctx, workspaceRoot)
	if err != nil {
		return SoloRunResult{}, err
	}
	run, err := loadRun(ctx, db, runID)
	if err != nil {
		return SoloRunResult{}, err
	}
	if run == nil {
		return SoloRunResult{}, fmt.Errorf("run not found: %s", runID)
	}

	ag := runnerOverride
	if ag == nil {
		ag, err = runner.ResolveRunner(run.Provider)
		if err != nil {
			return SoloRunResult{}, err
		}
	}

	now := s.now().UTC().Format(time.RFC3339Nano)
	_, _ = markIncompleteTurnsCrashed(ctx, db, run.ID, now)

	if detail, err := s.Workflow.GetWorkItem(ctx, workspaceRoot, run.WorkItemID); err == nil && detail != nil {
		reopen := make(map[string]bool)
		for _, n := range detail.Nodes {
			if n.Status == workflow.NodeBlocked || n.Status == workflow.NodeFailed {
				reopen[n.ID] = true
			}
		}
		if len(reopen) > 0 {
			if err := resetNodesForRetry(ctx, db, run.WorkItemID, reopen); err != nil {
				return SoloRunResult{}, fmt.Errorf("reopen blocked nodes: %w", err)
			}
		}
	}

	run.StopRequested = false
	run.StopReason = ""
	run.Status = "running"
	run.Backoff.ConsecutiveFailures = 0
	run.Backoff.LastFailureClass = ""
	if maxIterations > 0 && maxIterations > run.CurrentIteration {
		run.MaxIterations = maxIterations
	}
	run.UpdatedAt = now
	if err := updateRun(ctx, db, *run); err != nil {
		return SoloRunResult{}, err
	}

	return s.executeLoop(ctx, workspaceRoot, *run, ag, turnTimeout, pacing)
}

// Stop flags a running Solo run to halt cleanly at the next boundary.
func (s *SoloService) Stop(ctx context.Context, workspaceRoot, runID, reason string) (*RunRecord, error) {
	db, err := s.Workflow.DB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	run, err := loadRun(ctx, db, runID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("run not found: %s", runID)
	}
	if strings.TrimSpace(reason) == "" {
		reason = "operator-request"
	}
	run.StopRequested = true
	run.StopReason = reason
	run.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := updateRun(ctx, db, *run); err != nil {
		return nil, err
	}
	return run, nil
}

// Status loads a Solo run by runID or latest run for workItemID.
func (s *SoloService) Status(ctx context.Context, workspaceRoot, runID, workItemID string) (*RunRecord, error) {
	db, err := s.Workflow.DB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(runID) != "" {
		return loadRun(ctx, db, runID)
	}
	if strings.TrimSpace(workItemID) != "" {
		return latestRunForWorkItem(ctx, db, workItemID)
	}
	return nil, fmt.Errorf("either runId or workItemId is required")
}

func (s *SoloService) executeLoop(ctx context.Context, workspaceRoot string, run RunRecord, ag runner.AgentRunner, turnTimeout time.Duration, pacing PacingConfig) (SoloRunResult, error) {
	db, err := s.Workflow.DB(ctx, workspaceRoot)
	if err != nil {
		return SoloRunResult{}, err
	}

	lease, err := AcquireLease(ctx, db, "workitem:"+run.WorkItemID, run.ID)
	if err != nil {
		return SoloRunResult{Run: run}, err
	}
	defer lease.Release()

	sessionID := ""
	if s.Memory != nil {
		detail, _ := s.Workflow.GetWorkItem(ctx, workspaceRoot, run.WorkItemID)
		goal := ""
		if detail != nil {
			goal = detail.Objective
		}
		sessionID, _ = s.Memory.EnsureSession(ctx, workspaceRoot, run.ID, run.Provider, goal, run.WorkItemID)
	}

	receiptExec := runner.ReceiptExecutor{
		Sink: func(rctx context.Context, ws string, rec runner.ReceiptRecord) (string, error) {
			stored, err := s.Workflow.RecordReceipt(rctx, ws, workflow.ExecutionReceipt{
				WorkItemID:    rec.WorkItemID,
				NodeID:        rec.NodeID,
				RunID:         rec.RunID,
				Command:       rec.Command,
				Args:          rec.Args,
				Cwd:           rec.Cwd,
				ExitCode:      rec.ExitCode,
				TimedOut:      rec.TimedOut,
				DurationMs:    rec.DurationMs,
				StdoutSHA256:  rec.StdoutSHA256,
				StderrSHA256:  rec.StderrSHA256,
				OutputExcerpt: rec.OutputExcerpt,
				ArtifactRef:   rec.ArtifactRef,
				ExecError:     rec.ExecError,
			})
			if err != nil {
				return "", err
			}
			return stored.ID, nil
		},
		Offload: s.offloadHook(),
	}

	turnsExecuted := 0
	finalNote := ""
	// Fresh Run / explicit Resume counts as operator acknowledgment for prior gates (never bypasses quota).
	operatorAck := true
	var lastOutcome Outcome
	lastFailureClass := ""
	bypassUsed := run.Pacing.SafeBypassUsed
	bypassTurnActive := false

	for run.CurrentIteration < run.MaxIterations {
		// 1. Check stop control or context cancellation before starting a new turn.
		latest, _ := loadRun(ctx, db, run.ID)
		if (latest != nil && latest.StopRequested) || ctx.Err() != nil {
			run.Status = "stopped"
			run.StopRequested = true
			if latest != nil && latest.StopReason != "" {
				run.StopReason = latest.StopReason
			} else {
				run.StopReason = "context-cancelled"
			}
			finalNote = "stopped by control request: " + run.StopReason
			break
		}

		// 1b. Unattended should-run pacing gate (wait does NOT consume an iteration).
		if pacing.Enabled && !bypassTurnActive {
			nowMs := s.now().UnixMilli()
			quota := ComputeQuotaState(run.Pacing.QuotaSpendMs, nowMs, pacing)
			decision := ResolveShouldRun(lastOutcome, run.Status, lastFailureClass, quota, run.Pacing, pacing, operatorAck, nowMs)
			run.Pacing.LastDecision = &decision

			if decision.Action == "wait" {
				if err := s.sleep(ctx, time.Duration(decision.NextWakeMs)*time.Millisecond); err != nil {
					run.Status = "stopped"
					finalNote = "cancelled while waiting on pacing gate: " + decision.ReasonCode
					break
				}
				continue
			}
			if decision.Action == "ask" {
				run.Status = "human-gate"
				finalNote = "pacing gate requested operator decision: " + decision.ReasonCode
				break
			}
			if decision.Action == "quiet" {
				run.Status = "stopped"
				finalNote = "pacing gate shut down after unchanged polls: " + decision.ReasonCode
				break
			}
		}

		// 2. Claim the next ready node from the unified WorkItem DAG.
		claimed, err := s.Workflow.ClaimNode(ctx, workspaceRoot, run.WorkItemID, "")
		if err != nil {
			run.Status = "failed"
			finalNote = fmt.Sprintf("claim node failed: %v", err)
			break
		}
		if claimed == nil {
			gate, _ := s.Workflow.EvaluateDoneGate(ctx, workspaceRoot, run.WorkItemID)
			if gate.OK {
				run.Status = "done"
				finalNote = "all DAG nodes verified and completed"
			} else {
				run.Status = "blocked"
				finalNote = "no ready nodes; done gate incomplete: " + strings.Join(gate.Reasons, "; ")
			}
			break
		}

		bypassWasActive := bypassTurnActive
		bypassTurnActive = false

		run.CurrentIteration++
		turnsExecuted++
		turnStart := s.now().UTC()
		turn := TurnRecord{
			ID:           newRunID("turn"),
			RunID:        run.ID,
			WorkItemID:   run.WorkItemID,
			NodeID:       claimed.Node.ID,
			Iteration:    run.CurrentIteration,
			PhaseState:   "started",
			IsSafeBypass: bypassWasActive,
			StartedAt:    turnStart.Format(time.RFC3339Nano),
		}
		_ = recordTurnStart(ctx, db, turn)

		// 3. Assemble fresh-context prompt (no unbounded transcript accumulation).
		recalled := ""
		if s.Memory != nil {
			recalled, _ = s.Memory.RecallForTurn(ctx, workspaceRoot, claimed.Node.Title+" "+claimed.Node.Objective, sessionID, run.Provider, "")
		}
		prompt := buildSoloTurnPrompt(claimed.Node, run.CurrentIteration, recalled, bypassWasActive)
		execCwd := runner.ResolveExecutionCwd(workspaceRoot, run.Worktree)

		turnRes, runErr := ag.RunTurn(ctx, runner.RunTurnInput{
			WorkspaceRoot: workspaceRoot,
			ExecutionCwd:  execCwd,
			SessionID:     sessionID,
			WorkItemID:    run.WorkItemID,
			UserPrompt:    prompt,
			OutputSchema:  soloTurnOutputSchema,
			Timeout:       turnTimeout,
			Offload:       s.offloadHook(),
		})

		// 4. Evaluate process safety guards first (fail-closed on surviving process trees).
		outcome, failureClass, summary, selfReport, agentRefs, shouldStop := interpretTurnResult(turnRes, runErr)
		var receiptIDs []string

		// Code-level seal for safe_bypass: a read-only probe turn can NEVER claim material progress or advance the DAG.
		if bypassWasActive {
			outcome = OutcomeHumanGate
			if failureClass == "" || failureClass == "none" {
				failureClass = "safety-gate"
			}
			selfReport.ProgressMade = false
			summary = "[safe-bypass] " + summary
			finalNote = summary
			shouldStop = true
		}

		var verifyExecErr string
		if outcome == OutcomeSuccess && claimed.Node.VerifySpec != nil {
			verifyCwd := execCwd
			if rel := strings.TrimSpace(claimed.Node.VerifySpec.CwdRel); rel != "" {
				verifyCwd = filepath.Join(execCwd, rel)
			}
			recOut, recErr := receiptExec.RunAndRecord(ctx, runner.ReceiptExecInput{
				WorkspaceRoot: workspaceRoot,
				ExecutionCwd:  verifyCwd,
				WorkItemID:    run.WorkItemID,
				NodeID:        claimed.Node.ID,
				RunID:         run.ID,
				SessionID:     sessionID,
				Command:       claimed.Node.VerifySpec.Command,
				Args:          claimed.Node.VerifySpec.Args,
			})
			if recErr != nil {
				outcome = OutcomeBlocked
				failureClass = "runtime-error"
				summary = fmt.Sprintf("verification command execution error: %v", recErr)
				shouldStop = true
			} else {
				receiptIDs = append(receiptIDs, recOut.ReceiptID)
				verifyExecErr = recOut.Record.ExecError
			}
		}

		// 5. Submit node result into Workflow DAG using sealed executionToken + real receipt.
		// Noop polls and safe_bypass read-only probes do NOT consume DAG retries.
		var advResult *workflow.AdvanceNodeResult
		var turnHandoff *workflow.NodeHandoff
		if !bypassWasActive && outcome != OutcomeStopped && outcome != OutcomeNoop && !(turnRes.TimedOut && turnRes.TreeAlive) {
			submitStatus := mapOutcomeToSubmitStatus(outcome)
			receiptID := ""
			if len(receiptIDs) > 0 {
				receiptID = receiptIDs[0]
			}
			sanitized := sanitizeEvidenceRefsForSubmit(agentRefs, receiptIDs)
			var findings []string
			if note := recordDroppedEvidenceRefs(claimed.Node.ID, sanitized.DroppedFreeText); note != "" {
				findings = append(findings, note)
			}
			var blockers []string
			handoffStatus := submitStatus
			handoffSummary := summary
			if verifyExecErr != "" {
				envReason := "verification environment error: " + verifyExecErr
				handoffStatus = "blocked"
				handoffSummary = envReason
				blockers = append(blockers, envReason)
			}
			turnHandoff = &workflow.NodeHandoff{
				Status:       handoffStatus,
				Summary:      handoffSummary,
				Findings:     findings,
				Blockers:     blockers,
				EvidenceRefs: sanitized.Refs,
			}
			adv, advErr := s.Workflow.SubmitNodeResult(ctx, workflow.SubmitNodeResultInput{
				WorkspaceRoot:  workspaceRoot,
				WorkItemID:     run.WorkItemID,
				NodeID:         claimed.Node.ID,
				ExecutionToken: claimed.ExecutionToken,
				Status:         submitStatus,
				Summary:        handoffSummary,
				ReceiptID:      receiptID,
				EvidenceRefs:   sanitized.Refs,
				Handoff:        turnHandoff,
			})
			if advErr != nil {
				outcome = OutcomeBlocked
				failureClass = "runtime-error"
				summary = fmt.Sprintf("workflow submit rejected: %v", advErr)
				shouldStop = true
			} else {
				advResult = &adv
				switch adv.Decision {
				case workflow.DecisionCompleted:
					shouldStop = true
					finalNote = adv.Reason
				case workflow.DecisionRetry:
					outcome = OutcomeBlocked
					shouldStop = false
					summary = adv.Reason
				case workflow.DecisionHumanGate:
					outcome = OutcomeHumanGate
					shouldStop = true
					summary = adv.Reason
					finalNote = adv.Reason
				case workflow.DecisionBlocked:
					outcome = OutcomeBlocked
					if verifyExecErr != "" {
						failureClass = "environment-error"
					}
					shouldStop = true
					summary = adv.Reason
					finalNote = adv.Reason
				}
			}
		}

		// 6. Apply single-owner TURN_CONSEQUENCES decision table + pacing state writeback.
		material := IsMaterialTurn(outcome, selfReport)
		effectiveFailure := DeriveFailureClass(outcome, failureClass)
		row := TurnConsequences[outcome]
		if row.ResetsBackoff && (!row.AllowMaterial || material) {
			run.Backoff.ConsecutiveFailures = 0
			run.Backoff.LastFailureClass = ""
		} else if outcome == OutcomeInfraRetry {
			run.Backoff.ConsecutiveFailures++
			run.Backoff.LastFailureClass = effectiveFailure
			if run.Backoff.ConsecutiveFailures >= maxConsecutiveInfraErrors {
				outcome = OutcomeFailed
				shouldStop = true
				finalNote = fmt.Sprintf("aborted after %d consecutive infra failures", run.Backoff.ConsecutiveFailures)
			}
		}

		if pacing.Enabled {
			operatorAck = false
			nowMs := s.now().UnixMilli()
			run.Pacing.QuotaSpendMs = ChargeQuotaSpend(run.Pacing.QuotaSpendMs, material, nowMs, pacing)
			class, untilMs, step := AdvanceCadence(run.Pacing, outcome, material, nowMs, pacing)
			run.Pacing.CadenceClass = class
			run.Pacing.CadenceUntilMs = untilMs
			run.Pacing.CadenceStep = step
			if row.CountsAsNoopPoll {
				run.Pacing.ConsecutiveNoop++
			} else {
				run.Pacing.ConsecutiveNoop = 0
			}
		}
		lastOutcome = outcome
		lastFailureClass = effectiveFailure

		persistCtx := context.WithoutCancel(ctx)
		turn.PhaseState = "completed"
		if outcome == OutcomeStopped {
			turn.PhaseState = "aborted"
		}
		turn.Outcome = outcome
		turn.FailureClass = effectiveFailure
		turn.Material = material
		turn.OutputRef = turnRes.OutputArtifactRef
		turn.ReceiptIDs = receiptIDs
		turn.Handoff = turnHandoff
		turn.ElapsedMs = turnRes.ElapsedMs
		turn.FinishedAt = s.now().UTC().Format(time.RFC3339Nano)
		_ = recordTurnFinish(persistCtx, db, turn)

		run.Status = DeriveRunStatus(outcome, effectiveFailure, shouldStop)
		run.SummaryText = summary
		run.UpdatedAt = turn.FinishedAt
		_ = updateRun(persistCtx, db, run)

		// 7. Write checkpoint & continuity into decoupled MemoryPort (best-effort).
		if s.Memory != nil && sessionID != "" {
			_ = s.Memory.Checkpoint(persistCtx, workspaceRoot, sessionID, run.WorkItemID, run.Status, fmt.Sprintf("[iter %d · %s] %s", run.CurrentIteration, claimed.Node.ID, summary), nil, receiptIDs)
			_ = s.Memory.UpdateContinuity(persistCtx, workspaceRoot, sessionID, run.WorkItemID, claimed.Node.Objective, summary, nil, nil)
		}

		// Grant exactly one read-only safe_bypass turn before halting on human-gate in unattended mode.
		if (shouldStop || row.StopByDefault) && row.RequiresOperator && pacing.Enabled && pacing.SafeBypass && !bypassUsed {
			bypassUsed = true
			run.Pacing.SafeBypassUsed = true
			bypassTurnActive = true
			// Re-open node to active if workflow marked it blocked during human-gate so probe can read its context.
			_, _ = db.ExecContext(ctx, `UPDATE task_nodes SET status = 'active' WHERE work_item_id = ? AND id = ? AND status = 'blocked'`, run.WorkItemID, claimed.Node.ID)
			continue
		}

		if shouldStop || row.StopByDefault {
			if finalNote == "" {
				finalNote = summary
			}
			break
		}
		if advResult != nil && advResult.Decision == workflow.DecisionCompleted {
			break
		}
	}

	if run.Status == "running" && run.CurrentIteration >= run.MaxIterations {
		run.Status = "human-gate"
		finalNote = fmt.Sprintf("reached maxIterations (%d)", run.MaxIterations)
	}

	if run.Worktree != nil && run.Worktree.Enabled {
		finalWT, _ := s.Worktree.Finalize(workspaceRoot, *run.Worktree, run.Status, true, false)
		run.Worktree = &finalWT
	}

	persistCtx := context.WithoutCancel(ctx)
	run.SummaryText = finalNote
	run.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
	_ = updateRun(persistCtx, db, run)

	detail, _ := s.Workflow.GetWorkItem(persistCtx, workspaceRoot, run.WorkItemID)
	var wi workflow.WorkItem
	if detail != nil {
		wi = detail.WorkItem
	}
	return SoloRunResult{
		Run:       run,
		Turns:     turnsExecuted,
		WorkItem:  wi,
		FinalNote: finalNote,
	}, nil
}

func (s *SoloService) now() time.Time {
	if s.NowFunc != nil {
		return s.NowFunc()
	}
	return time.Now().UTC()
}

func (s *SoloService) sleep(ctx context.Context, d time.Duration) error {
	if s.SleepFunc != nil {
		return s.SleepFunc(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *SoloService) offloadHook() runner.OffloadHook {
	if s.Memory == nil {
		return nil
	}
	return s.Memory.StoreArtifact
}

func resolveAgentRunner(in SoloRunInput) (runner.AgentRunner, string, error) {
	if in.RunnerOverride != nil {
		return in.RunnerOverride, in.RunnerOverride.ID(), nil
	}
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		provider = "codex"
	}
	ag, err := runner.ResolveRunner(provider)
	return ag, provider, err
}

func buildSoloTurnPrompt(node workflow.TaskNode, iteration int, recalled string, safeBypass bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## jtao Solo Turn %d — Node `%s`\n\n", iteration, node.ID)
	if safeBypass {
		b.WriteString("**SAFE-BYPASS READ-ONLY PROBE**: Do NOT modify repository files; collect diagnostic context for the operator.\n\n")
	}
	fmt.Fprintf(&b, "- Title: %s\n", node.Title)
	if node.Objective != "" {
		fmt.Fprintf(&b, "- Objective: %s\n", node.Objective)
	}
	if node.Acceptance != "" {
		fmt.Fprintf(&b, "- Acceptance: %s\n", node.Acceptance)
	}
	if node.VerifySpec != nil {
		fmt.Fprintf(&b, "- Verification command (host will execute and verify receipt): `%s %s` (expected exit=%d)\n",
			node.VerifySpec.Command, strings.Join(node.VerifySpec.Args, " "), node.VerifySpec.ExpectedExitCode)
	}
	if recalled != "" {
		b.WriteString("\n" + recalled + "\n")
	}
	b.WriteString("\nReturn a single JSON object matching the required turn schema with `outcome`, `summary`, and `selfReport.progressMade`.\n")
	b.WriteString(evidenceRefsPromptRule + "\n")
	return b.String()
}

func interpretTurnResult(res runner.RunTurnResult, runErr error) (Outcome, string, string, SelfReport, []string, bool) {
	if runErr != nil {
		return OutcomeInfraRetry, "runtime-error", runErr.Error(), SelfReport{}, nil, false
	}
	if res.Aborted {
		return OutcomeStopped, "stop-requested", "turn aborted by cancellation signal", SelfReport{}, nil, true
	}
	if res.TimedOut {
		if res.TreeAlive {
			return OutcomeBlocked, "runtime-error", fmt.Sprintf("turn timed out and process tree (pid=%d) survived SIGKILL; refusing to continue", res.ChildPID), SelfReport{}, nil, true
		}
		return OutcomeInfraRetry, "runtime-error", "turn timed out; process tree cleaned up cleanly", SelfReport{}, nil, false
	}
	if res.ParsedJSON == nil {
		if res.ExitCode != 0 {
			msg := fmt.Sprintf("agent exited with status %d without valid JSON output", res.ExitCode)
			if excerpt := failureOutputExcerpt(res.RawOutput); excerpt != "" {
				msg += ": " + excerpt
			}
			return OutcomeInfraRetry, "runtime-error", msg, SelfReport{}, nil, false
		}
		return OutcomeInfraRetry, "runtime-error", "agent output missing required JSON contract", SelfReport{}, nil, false
	}

	rawOutcome, _ := res.ParsedJSON["outcome"].(string)
	outcome := Outcome(strings.ToLower(strings.TrimSpace(rawOutcome)))
	if _, ok := TurnConsequences[outcome]; !ok {
		outcome = OutcomeInfraRetry
	}
	summary, _ := res.ParsedJSON["summary"].(string)
	if strings.TrimSpace(summary) == "" {
		summary = "turn completed"
	}

	var report SelfReport
	if sr, ok := res.ParsedJSON["selfReport"].(map[string]any); ok {
		report.Declared = true
		report.ProgressMade, _ = sr["progressMade"].(bool)
		report.BlockedReason, _ = sr["blockedReason"].(string)
		report.Summary, _ = sr["summary"].(string)
	}

	refs := stringSliceField(res.ParsedJSON, "evidenceRefs")

	shouldStop := TurnConsequences[outcome].StopByDefault
	return outcome, "", summary, report, refs, shouldStop
}

func mapOutcomeToSubmitStatus(outcome Outcome) string {
	switch outcome {
	case OutcomeSuccess:
		return "pass"
	case OutcomeHumanGate:
		return "needs-input"
	case OutcomeBlocked, OutcomeFailed:
		return "blocked"
	default:
		return "blocked"
	}
}

func failureOutputExcerpt(raw string) string {
	const maxRunes = 240
	var picked string
	var fallback string
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		fallback = trimmed
		if strings.HasPrefix(strings.ToLower(trimmed), "error:") {
			picked = trimmed
		}
	}
	if picked == "" {
		picked = fallback
	}
	runes := []rune(picked)
	if len(runes) <= maxRunes {
		return picked
	}
	return string(runes[:maxRunes]) + "..."
}
