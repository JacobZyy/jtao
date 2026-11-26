package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JacobZyy/jtao/internal/runner"
	"github.com/JacobZyy/jtao/internal/workflow"
)

const (
	defaultTeamConcurrency       = 3
	unstableBlockedRunThreshold  = 2
)

// TeamRunInput configures a multi-role concurrent DAG execution on a WorkItem.
type TeamRunInput struct {
	WorkspaceRoot       string
	WorkItemID          string
	RunID               string // Optional pre-allocated Run ID (auto-generated if empty)
	Provider            string // codex | claude
	Concurrency         int
	UseWorktree         bool   // Isolate every editable node in its own git worktree
	DisableAutoWorktree bool   // Opt out of automatic worktree isolation for parallel writers
	BaseRef             string
	PreMutationSnapshot bool
	TurnTimeout         time.Duration
	RunnerOverride      runner.AgentRunner // Optional injection for tests
}

// TeamRetryInput retries only blocked/failed nodes from a prior Team run.
type TeamRetryInput struct {
	WorkspaceRoot  string
	RunID          string // Prior Team Run ID to retry
	NextRunID      string // Optional pre-allocated ID for the newly spawned retry run
	Provider       string
	Concurrency    int
	UseWorktree    bool
	Force          bool // Override instability guard after repeated blocked runs
	DryRun         bool
	TurnTimeout    time.Duration
	RunnerOverride runner.AgentRunner
}

// TeamPlanPreview previews DAG scheduling waves and ownership checks with zero model calls.
type TeamPlanPreview struct {
	WorkItemID  string              `json:"workItemId"`
	Title       string              `json:"title"`
	Blueprint   string              `json:"blueprint"`
	Concurrency int                 `json:"concurrency"`
	Waves       [][]string          `json:"waves"`
	Nodes       []workflow.TaskNode `json:"nodes"`
}

// TeamNodeRun captures the execution result of a single node inside a Team run.
type TeamNodeRun struct {
	NodeID           string                 `json:"nodeId"`
	NodeKind         workflow.NodeKind      `json:"nodeKind"`
	Role             string                 `json:"role"`
	CanEditFiles     bool                   `json:"canEditFiles"`
	OwnedPathPrefixes []string              `json:"ownedPathPrefixes,omitempty"`
	Status           string                 `json:"status"` // done | blocked | failed | skipped
	AutoCompleted    bool                   `json:"autoCompleted,omitempty"`
	Seeded           bool                   `json:"seeded,omitempty"`
	Reason           string                 `json:"reason,omitempty"`
	FilesTouched     []string               `json:"filesTouched,omitempty"`
	ReceiptIDs       []string               `json:"receiptIds,omitempty"`
	SnapshotRef      string                 `json:"snapshotRef,omitempty"`
	Worktree         *runner.WorktreeHandle `json:"worktree,omitempty"`
	Handoff          *workflow.NodeHandoff  `json:"handoff,omitempty"`
	MergeGate        *MergeGateResult       `json:"mergeGate,omitempty"`
	ElapsedMs        int64                  `json:"elapsedMs"`
}

// TeamRunResult summarizes a completed or blocked Team DAG execution.
type TeamRunResult struct {
	OK          bool              `json:"ok"`
	Run         RunRecord         `json:"run"`
	WorkItem    workflow.WorkItem `json:"workItem"`
	NodeRuns    []TeamNodeRun     `json:"nodeRuns"`
	BlockedIDs  []string          `json:"blockedIds,omitempty"`
	SeededCount int               `json:"seededCount,omitempty"`
	FinalNote   string            `json:"finalNote"`
}

// TeamService executes concurrent multi-role WorkItem DAGs with ownership and Merge Gate guards.
type TeamService struct {
	Workflow *workflow.SQLiteService
	Memory   MemoryPort // Optional
	Worktree runner.WorktreeManager
}

var teamNodeOutputSchema = []byte(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["status", "summary"],
  "properties": {
    "status": { "type": "string", "enum": ["pass", "blocked", "needs-input", "fail"] },
    "summary": { "type": "string" },
    "findings": { "type": "array", "items": { "type": "string" } },
    "blockers": { "type": "array", "items": { "type": "string" } },
    "filesTouched": { "type": "array", "items": { "type": "string" } },
    "nextSteps": { "type": "array", "items": { "type": "string" } },
    "evidenceRefs": { "type": "array", "items": { "type": "string" } }
  }
}`)

// Plan validates DAG ownership rules and computes parallel execution waves without calling any model.
func (s *TeamService) Plan(ctx context.Context, in TeamRunInput) (TeamPlanPreview, error) {
	detail, err := s.Workflow.GetWorkItem(ctx, in.WorkspaceRoot, in.WorkItemID)
	if err != nil {
		return TeamPlanPreview{}, err
	}
	if detail == nil {
		return TeamPlanPreview{}, fmt.Errorf("work item not found: %s", in.WorkItemID)
	}
	if err := AssertEditableParallelOwnership(*detail); err != nil {
		return TeamPlanPreview{}, err
	}
	conc := in.Concurrency
	if conc <= 0 {
		conc = defaultTeamConcurrency
	}
	return TeamPlanPreview{
		WorkItemID:  detail.ID,
		Title:       detail.Title,
		Blueprint:   detail.Blueprint,
		Concurrency: conc,
		Waves:       computeExecutionWaves(*detail),
		Nodes:       detail.Nodes,
	}, nil
}

// Run dispatches all nodes in a WorkItem DAG concurrently using a bounded goroutine pool.
func (s *TeamService) Run(ctx context.Context, in TeamRunInput) (TeamRunResult, error) {
	return s.runWithSeeds(ctx, in, nil)
}

// RetryBlocked replays only blocked/failed nodes (and their downstream dependents) from a prior Team run,
// seeding all previously completed nodes and guarding against blind repeated retries.
func (s *TeamService) RetryBlocked(ctx context.Context, in TeamRetryInput) (TeamRunResult, error) {
	db, err := s.Workflow.DB(ctx, in.WorkspaceRoot)
	if err != nil {
		return TeamRunResult{}, err
	}
	prior, err := loadRun(ctx, db, in.RunID)
	if err != nil {
		return TeamRunResult{}, err
	}
	if prior == nil {
		return TeamRunResult{}, fmt.Errorf("prior team run not found: %s", in.RunID)
	}
	if prior.Mode != "team" {
		return TeamRunResult{}, fmt.Errorf("run %s is mode=%s, not team", in.RunID, prior.Mode)
	}

	priorTurns, err := loadRunTurns(ctx, db, prior.ID)
	if err != nil {
		return TeamRunResult{}, err
	}

	detail, err := s.Workflow.GetWorkItem(ctx, in.WorkspaceRoot, prior.WorkItemID)
	if err != nil || detail == nil {
		return TeamRunResult{}, fmt.Errorf("work item not found: %s", prior.WorkItemID)
	}

	// Identify blocked/failed nodes and seed completed nodes.
	blockedSet := make(map[string]bool)
	seeds := make(map[string]TeamNodeRun)
	for _, n := range detail.Nodes {
		if n.Status == workflow.NodeBlocked || n.Status == workflow.NodeFailed {
			blockedSet[n.ID] = true
		}
	}
	for _, t := range priorTurns {
		if t.Outcome == OutcomeBlocked || t.Outcome == OutcomeFailed {
			blockedSet[t.NodeID] = true
		}
	}
	if len(blockedSet) == 0 {
		return TeamRunResult{}, fmt.Errorf("run %s has no blocked or failed nodes to retry", prior.ID)
	}

	// Expand blocked set to all transitive downstream dependents so merge gates re-run too.
	expandDownstream(detail.Edges, blockedSet)

	// Instability guard: check if any blocked node failed in >= unstableBlockedRunThreshold consecutive prior team runs.
	if !in.Force {
		repeatNodes, err := detectRepeatedBlockedNodes(ctx, db, prior.WorkItemID, blockedSet)
		if err == nil && len(repeatNodes) > 0 {
			return TeamRunResult{}, fmt.Errorf(
				"guard: refusing blind retry-blocked for workItem %s — node(s) %s blocked repeatedly (>=%d runs); inspect blockers or pass --force",
				prior.WorkItemID, strings.Join(repeatNodes, ", "), unstableBlockedRunThreshold,
			)
		}
	}

	// Build seeds for all completed non-blocked nodes.
	nodeByID := make(map[string]workflow.TaskNode, len(detail.Nodes))
	for _, n := range detail.Nodes {
		nodeByID[n.ID] = n
	}
	for _, t := range priorTurns {
		if blockedSet[t.NodeID] {
			continue
		}
		n, exists := nodeByID[t.NodeID]
		if !exists {
			continue
		}
		if n.Status == workflow.NodeDone || n.Status == workflow.NodeSkipped || t.Outcome == OutcomeSuccess {
			seeds[t.NodeID] = TeamNodeRun{
				NodeID:            n.ID,
				NodeKind:          n.NodeKind,
				Role:              n.Role,
				CanEditFiles:      n.CanEditFiles,
				OwnedPathPrefixes: n.OwnedPathPrefixes,
				Status:            "done",
				Seeded:            true,
				FilesTouched:      t.FilesTouched,
				ReceiptIDs:        t.ReceiptIDs,
				Handoff:           n.Handoff,
				Reason:            "seeded from prior completed run " + prior.ID,
			}
		}
	}

	if in.DryRun {
		var retryIDs []string
		for id := range blockedSet {
			retryIDs = append(retryIDs, id)
		}
		sort.Strings(retryIDs)
		return TeamRunResult{
			OK:          true,
			Run:         *prior,
			WorkItem:    detail.WorkItem,
			SeededCount: len(seeds),
			FinalNote:   fmt.Sprintf("dry-run retry-blocked: seeded=%d retry=%v", len(seeds), retryIDs),
		}, nil
	}

	// Reset blocked/downstream nodes in workflow.db back to pending so they can be re-claimed cleanly.
	if err := resetNodesForRetry(ctx, db, prior.WorkItemID, blockedSet); err != nil {
		return TeamRunResult{}, err
	}

	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		provider = prior.Provider
	}
	return s.runWithSeeds(ctx, TeamRunInput{
		WorkspaceRoot:  in.WorkspaceRoot,
		WorkItemID:     prior.WorkItemID,
		RunID:          in.NextRunID,
		Provider:       provider,
		Concurrency:    in.Concurrency,
		UseWorktree:    in.UseWorktree,
		TurnTimeout:    in.TurnTimeout,
		RunnerOverride: in.RunnerOverride,
	}, seeds)
}

func (s *TeamService) runWithSeeds(ctx context.Context, in TeamRunInput, seeds map[string]TeamNodeRun) (TeamRunResult, error) {
	detail, err := s.Workflow.GetWorkItem(ctx, in.WorkspaceRoot, in.WorkItemID)
	if err != nil {
		return TeamRunResult{}, err
	}
	if detail == nil {
		return TeamRunResult{}, fmt.Errorf("work item not found: %s", in.WorkItemID)
	}
	if err := AssertEditableParallelOwnership(*detail); err != nil {
		return TeamRunResult{}, err
	}

	ag, provider, err := resolveTeamRunner(in)
	if err != nil {
		return TeamRunResult{}, err
	}
	if in.RunnerOverride == nil {
		if check := ag.CheckReadiness(); !check.OK {
			return TeamRunResult{}, fmt.Errorf("provider readiness failed: %s", check.Reason)
		}
	}

	conc := in.Concurrency
	if conc <= 0 {
		conc = defaultTeamConcurrency
	}

	db, err := s.Workflow.DB(ctx, in.WorkspaceRoot)
	if err != nil {
		return TeamRunResult{}, err
	}

	runID := strings.TrimSpace(in.RunID)
	if runID == "" {
		runID = newRunID("run:team")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	run := RunRecord{
		ID:            runID,
		WorkItemID:    detail.ID,
		Mode:          "team",
		Provider:      provider,
		Status:        "running",
		MaxIterations: len(detail.Nodes),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := insertRun(ctx, db, run); err != nil {
		return TeamRunResult{}, fmt.Errorf("create team run: %w", err)
	}

	lease, err := AcquireLease(ctx, db, "workitem:"+detail.ID, run.ID)
	if err != nil {
		run.Status = "failed"
		run.SummaryText = err.Error()
		_ = updateRun(ctx, db, run)
		return TeamRunResult{Run: run, WorkItem: detail.WorkItem}, err
	}
	defer lease.Release()

	sessionID := ""
	if s.Memory != nil {
		sessionID, _ = s.Memory.EnsureSession(ctx, in.WorkspaceRoot, run.ID, provider, detail.Objective, detail.ID)
	}

	depsByNode := make(map[string][]string, len(detail.Nodes))
	nodeByID := make(map[string]workflow.TaskNode, len(detail.Nodes))
	for _, n := range detail.Nodes {
		nodeByID[n.ID] = n
	}
	for _, e := range detail.Edges {
		depsByNode[e.ToNodeID] = append(depsByNode[e.ToNodeID], e.FromNodeID)
	}

	// Automatically enable per-node worktrees if multiple editable nodes can execute in the same wave.
	autoWorktree := !in.DisableAutoWorktree && hasParallelEditableWave(*detail)

	completed := make(map[string]TeamNodeRun, len(detail.Nodes))
	pending := make(map[string]workflow.TaskNode, len(detail.Nodes))
	seededCount := 0
	for _, n := range detail.Nodes {
		if seed, ok := seeds[n.ID]; ok {
			completed[n.ID] = seed
			seededCount++
			continue
		}
		if n.Status == workflow.NodeDone || n.Status == workflow.NodeSkipped {
			completed[n.ID] = TeamNodeRun{
				NodeID:            n.ID,
				NodeKind:          n.NodeKind,
				Role:              n.Role,
				CanEditFiles:      n.CanEditFiles,
				OwnedPathPrefixes: n.OwnedPathPrefixes,
				Status:            string(n.Status),
				Seeded:            true,
				Handoff:           n.Handoff,
			}
			seededCount++
			continue
		}
		pending[n.ID] = n
	}

	type doneEvent struct {
		nodeID string
		result TeamNodeRun
	}
	doneCh := make(chan doneEvent, len(detail.Nodes))
	inFlight := 0
	var claimMu sync.Mutex
	iterCounter := 0

	stopDetected := false
	stopReason := ""
	for len(pending) > 0 || inFlight > 0 {
		if !stopDetected {
			if ctx.Err() != nil {
				stopDetected = true
				stopReason = "context-cancelled"
			} else if latest, _ := loadRun(ctx, db, run.ID); latest != nil && latest.StopRequested {
				stopDetected = true
				stopReason = latest.StopReason
			}
		}

		startedAny := false
		if !stopDetected {
			// Launch all dependency-satisfied pending nodes up to concurrency bound.
			var launchIDs []string
			for id := range pending {
				depsOK := true
				for _, depID := range depsByNode[id] {
					if _, ok := completed[depID]; !ok {
						depsOK = false
						break
					}
				}
				if depsOK {
					launchIDs = append(launchIDs, id)
				}
			}
			sort.Slice(launchIDs, func(i, j int) bool {
				return pending[launchIDs[i]].OrderIndex < pending[launchIDs[j]].OrderIndex
			})

			for _, id := range launchIDs {
				if inFlight >= conc {
					break
				}
				node := pending[id]
				delete(pending, id)
				startedAny = true
				inFlight++
				iterCounter++
				iterNum := iterCounter

				var upstream []TeamNodeRun
				for _, depID := range depsByNode[id] {
					upstream = append(upstream, completed[depID])
				}

				go func(n workflow.TaskNode, deps []TeamNodeRun, iteration int) {
					res := s.executeTeamNode(ctx, db, &claimMu, in, run.ID, sessionID, ag, n, deps, iteration, in.UseWorktree || autoWorktree)
					doneCh <- doneEvent{nodeID: n.ID, result: res}
				}(node, upstream, iterNum)
			}
		}

		if inFlight > 0 {
			ev := <-doneCh
			inFlight--
			completed[ev.nodeID] = ev.result
			run.CurrentIteration = len(completed)
			run.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_ = updateRun(context.WithoutCancel(ctx), db, run)
			continue
		}
		if stopDetected {
			break
		}
		if !startedAny && len(pending) > 0 {
			// Unresolvable dependency cycle fallback (defensive).
			for id, n := range pending {
				completed[id] = TeamNodeRun{
					NodeID:   id,
					NodeKind: n.NodeKind,
					Role:     n.Role,
					Status:   "blocked",
					Reason:   "unresolved dependency cycle",
				}
			}
			break
		}
	}

	// Assemble ordered NodeRuns and overall verdict.
	nodeRuns := make([]TeamNodeRun, 0, len(detail.Nodes))
	var blockedIDs []string
	for _, n := range detail.Nodes {
		nr, ok := completed[n.ID]
		if !ok {
			continue
		}
		nodeRuns = append(nodeRuns, nr)
		if nr.Status == "blocked" || nr.Status == "failed" {
			blockedIDs = append(blockedIDs, nr.NodeID)
		}
	}

	persistCtx := context.WithoutCancel(ctx)
	allOK := !stopDetected && len(blockedIDs) == 0 && len(pending) == 0
	run.CurrentIteration = len(completed)
	if stopDetected {
		run.Status = "stopped"
		run.StopRequested = true
		if stopReason != "" {
			run.StopReason = stopReason
		}
		run.SummaryText = "team DAG stopped by control request: " + run.StopReason
	} else if allOK {
		run.Status = "done"
		run.SummaryText = fmt.Sprintf("team DAG completed cleanly (%d nodes, %d seeded)", len(nodeRuns), seededCount)
	} else {
		run.Status = "blocked"
		run.SummaryText = fmt.Sprintf("team DAG blocked on node(s): %s", strings.Join(blockedIDs, ", "))
	}
	run.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_ = updateRun(persistCtx, db, run)

	if s.Memory != nil && sessionID != "" {
		_ = s.Memory.Checkpoint(persistCtx, in.WorkspaceRoot, sessionID, detail.ID, run.Status, run.SummaryText, nil, nil)
	}

	updatedDetail, _ := s.Workflow.GetWorkItem(persistCtx, in.WorkspaceRoot, detail.ID)
	wi := detail.WorkItem
	if updatedDetail != nil {
		wi = updatedDetail.WorkItem
	}

	return TeamRunResult{
		OK:          allOK,
		Run:         run,
		WorkItem:    wi,
		NodeRuns:    nodeRuns,
		BlockedIDs:  blockedIDs,
		SeededCount: seededCount,
		FinalNote:   run.SummaryText,
	}, nil
}

func (s *TeamService) executeTeamNode(
	ctx context.Context,
	db *sql.DB,
	claimMu *sync.Mutex,
	in TeamRunInput,
	runID string,
	sessionID string,
	ag runner.AgentRunner,
	node workflow.TaskNode,
	upstream []TeamNodeRun,
	iteration int,
	useWorktreeForEditable bool,
) TeamNodeRun {
	start := time.Now()
	base := TeamNodeRun{
		NodeID:            node.ID,
		NodeKind:          node.NodeKind,
		Role:              node.Role,
		CanEditFiles:      node.CanEditFiles,
		OwnedPathPrefixes: node.OwnedPathPrefixes,
	}

	// Claim node safely under mutex so SQLite single-writer tx stays clean across goroutines.
	claimMu.Lock()
	claimed, claimErr := s.Workflow.ClaimNode(ctx, in.WorkspaceRoot, in.WorkItemID, node.ID)
	claimMu.Unlock()
	if claimErr != nil || claimed == nil {
		base.Status = "blocked"
		base.Reason = fmt.Sprintf("failed to claim node %s: %v", node.ID, claimErr)
		return base
	}

	turn := TurnRecord{
		ID:         newRunID("turn"),
		RunID:      runID,
		WorkItemID: in.WorkItemID,
		NodeID:     node.ID,
		Iteration:  iteration,
		PhaseState: "started",
		StartedAt:  start.UTC().Format(time.RFC3339Nano),
	}
	claimMu.Lock()
	_ = recordTurnStart(ctx, db, turn)
	claimMu.Unlock()

	finishAndSubmit := func(status, submitStatus, reason string, handoff *workflow.NodeHandoff, filesTouched, receiptIDs []string, outcome Outcome) TeamNodeRun {
		base.Status = status
		base.Reason = reason
		base.Handoff = handoff
		base.FilesTouched = filesTouched
		base.ReceiptIDs = receiptIDs
		base.ElapsedMs = time.Since(start).Milliseconds()

		receiptID := ""
		if len(receiptIDs) > 0 {
			receiptID = receiptIDs[0]
		}
		var refs []string
		refs = append(refs, receiptIDs...)
		if handoff != nil {
			refs = append(refs, handoff.EvidenceRefs...)
		}
		if len(refs) == 0 && submitStatus == "pass" {
			refs = []string{"note:team-" + node.ID}
		}

		claimMu.Lock()
		_, _ = s.Workflow.SubmitNodeResult(ctx, workflow.SubmitNodeResultInput{
			WorkspaceRoot:  in.WorkspaceRoot,
			WorkItemID:     in.WorkItemID,
			NodeID:         node.ID,
			ExecutionToken: claimed.ExecutionToken,
			Status:         submitStatus,
			Summary:        reason,
			ReceiptID:      receiptID,
			EvidenceRefs:   refs,
			Handoff:        handoff,
		})
		turn.PhaseState = "completed"
		turn.Outcome = outcome
		turn.Material = outcome == OutcomeSuccess
		turn.ReceiptIDs = receiptIDs
		turn.FilesTouched = filesTouched
		turn.Handoff = handoff
		turn.ElapsedMs = base.ElapsedMs
		turn.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		_ = recordTurnFinish(ctx, db, turn)
		claimMu.Unlock()
		return base
	}

	// 1. Merge-gate nodes are evaluated deterministically in Go without calling any LLM.
	if node.NodeKind == workflow.NodeKindMergeGate {
		gate := EvaluateMergeGate(node.ParallelGroup, upstream)
		base.MergeGate = &gate
		handoff := &workflow.NodeHandoff{
			Status:       condStatus(gate.OK, "pass", "blocked"),
			Summary:      condStatus(gate.OK, "Merge gate passed for "+node.ParallelGroup, gate.Question),
			Findings:     gate.MergedFindings,
			FilesTouched: gate.TouchedFiles,
			NextSteps:    gate.MergedNextSteps,
		}
		if !gate.OK {
			handoff.Blockers = []string{gate.Question}
			return finishAndSubmit("blocked", "blocked", gate.Question, handoff, gate.TouchedFiles, nil, OutcomeBlocked)
		}
		return finishAndSubmit("done", "pass", "merge gate passed", handoff, gate.TouchedFiles, nil, OutcomeSuccess)
	}

	// 2. Cascade block if any upstream dependency failed or blocked.
	for _, dep := range upstream {
		if dep.Status == "blocked" || dep.Status == "failed" {
			msg := fmt.Sprintf("blocked by upstream dependency %s (%s)", dep.NodeID, dep.Status)
			handoff := &workflow.NodeHandoff{Status: "blocked", Summary: msg, Blockers: []string{msg}}
			return finishAndSubmit("blocked", "blocked", msg, handoff, nil, nil, OutcomeBlocked)
		}
	}

	// 3. Read-only review auto-complete optimization when upstream produced zero file changes.
	if shouldAutoCompleteReadOnlyReview(node, upstream) {
		base.AutoCompleted = true
		handoff := &workflow.NodeHandoff{
			Status:   "pass",
			Summary:  "Auto-completed read-only review: upstream produced no file changes.",
			Findings: []string{"no-upstream-file-changes"},
		}
		return finishAndSubmit("done", "pass", "no-upstream-file-changes", handoff, nil, nil, OutcomeSuccess)
	}

	// 4. Optional pre-mutation snapshot & Git Worktree isolation for editable nodes.
	execCwd := in.WorkspaceRoot
	var wtHandle *runner.WorktreeHandle
	if node.CanEditFiles {
		if in.PreMutationSnapshot {
			base.SnapshotRef = capturePreMutationSnapshot(in.WorkspaceRoot)
		}
		if useWorktreeForEditable {
			if wt, err := s.Worktree.Prepare(in.WorkspaceRoot, runID+"-"+node.ID, in.BaseRef); err == nil {
				wtHandle = &wt
				execCwd = wt.Path
				base.Worktree = wtHandle
			}
		}
	}

	// 5. Build focused node prompt and execute via unified AgentRunner.
	prompt := buildTeamNodePrompt(node, upstream)
	turnRes, runErr := ag.RunTurn(ctx, runner.RunTurnInput{
		WorkspaceRoot: in.WorkspaceRoot,
		ExecutionCwd:  execCwd,
		SessionID:     sessionID,
		WorkItemID:    in.WorkItemID,
		UserPrompt:    prompt,
		OutputSchema:  teamNodeOutputSchema,
		Timeout:       in.TurnTimeout,
		Offload:       s.offloadHook(),
	})

	// Detect real files touched in worktree if isolated.
	var realTouched []string
	if wtHandle != nil {
		if changes, err := s.Worktree.DetectChanges(*wtHandle); err == nil {
			realTouched = changes.Files
		}
	}

	if runErr != nil || (turnRes.TimedOut && turnRes.TreeAlive) || turnRes.Aborted {
		reason := fmt.Sprintf("agent execution error: err=%v timedOut=%v treeAlive=%v aborted=%v", runErr, turnRes.TimedOut, turnRes.TreeAlive, turnRes.Aborted)
		if wtHandle != nil {
			finalWT, _ := s.Worktree.Finalize(in.WorkspaceRoot, *wtHandle, "blocked", true, false)
			base.Worktree = &finalWT
		}
		handoff := &workflow.NodeHandoff{Status: "blocked", Summary: reason, Blockers: []string{reason}}
		return finishAndSubmit("blocked", "blocked", reason, handoff, realTouched, nil, OutcomeBlocked)
	}

	handoff := parseTeamNodeHandoff(turnRes, realTouched)
	filesTouched := mergeUniqueSlice(realTouched, handoff.FilesTouched)

	// Ownership guard directly on single editable node (even before any merge gate).
	if !node.CanEditFiles && len(filesTouched) > 0 {
		msg := fmt.Sprintf("read-only role %s unexpectedly touched file(s): %s", node.Role, strings.Join(filesTouched, ", "))
		handoff.Status = "blocked"
		handoff.Blockers = append(handoff.Blockers, msg)
		return finishAndSubmit("blocked", "blocked", msg, handoff, filesTouched, nil, OutcomeBlocked)
	}
	if node.CanEditFiles && len(node.OwnedPathPrefixes) > 0 {
		var violated []string
		for _, f := range filesTouched {
			if !matchesAnyPrefix(normalizeRelPath(f), node.OwnedPathPrefixes) {
				violated = append(violated, f)
			}
		}
		if len(violated) > 0 {
			msg := fmt.Sprintf("node %s violated ownedPathPrefixes %v by editing: %s", node.ID, node.OwnedPathPrefixes, strings.Join(violated, ", "))
			handoff.Status = "blocked"
			handoff.Blockers = append(handoff.Blockers, msg)
			if wtHandle != nil {
				finalWT, _ := s.Worktree.Finalize(in.WorkspaceRoot, *wtHandle, "blocked", true, false)
				base.Worktree = &finalWT
			}
			return finishAndSubmit("blocked", "blocked", msg, handoff, filesTouched, nil, OutcomeBlocked)
		}
	}

	// Optional real verification command receipt check.
	var receiptIDs []string
	if handoff.Status == "pass" && node.VerifySpec != nil {
		recExec := runner.ReceiptExecutor{
			Sink: func(rctx context.Context, root string, rec runner.ReceiptRecord) (string, error) {
				claimMu.Lock()
				defer claimMu.Unlock()
				stored, err := s.Workflow.RecordReceipt(rctx, root, workflow.ExecutionReceipt{
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
				})
				if err != nil {
					return "", err
				}
				return stored.ID, nil
			},
			Offload: s.offloadHook(),
		}
		recOut, recErr := recExec.RunAndRecord(ctx, runner.ReceiptExecInput{
			WorkspaceRoot: in.WorkspaceRoot,
			ExecutionCwd:  execCwd,
			WorkItemID:    in.WorkItemID,
			NodeID:        node.ID,
			RunID:         runID,
			SessionID:     sessionID,
			Command:       node.VerifySpec.Command,
			Args:          node.VerifySpec.Args,
		})
		if recErr != nil {
			handoff.Status = "blocked"
			handoff.Blockers = append(handoff.Blockers, recErr.Error())
		} else {
			receiptIDs = append(receiptIDs, recOut.ReceiptID)
			if recOut.Record.ExitCode != node.VerifySpec.ExpectedExitCode {
				handoff.Status = "blocked"
				handoff.Blockers = append(handoff.Blockers, fmt.Sprintf("verification failed (exit=%d)", recOut.Record.ExitCode))
			}
		}
	}

	finalStatus := "done"
	outcome := OutcomeSuccess
	if handoff.Status != "pass" {
		finalStatus = "blocked"
		outcome = OutcomeBlocked
	}
	if wtHandle != nil {
		finalWT, _ := s.Worktree.Finalize(in.WorkspaceRoot, *wtHandle, finalStatus, true, false)
		base.Worktree = &finalWT
	}
	return finishAndSubmit(finalStatus, handoff.Status, handoff.Summary, handoff, filesTouched, receiptIDs, outcome)
}

func (s *TeamService) offloadHook() runner.OffloadHook {
	if s.Memory == nil {
		return nil
	}
	return s.Memory.StoreArtifact
}

func resolveTeamRunner(in TeamRunInput) (runner.AgentRunner, string, error) {
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

func shouldAutoCompleteReadOnlyReview(node workflow.TaskNode, upstream []TeamNodeRun) bool {
	if node.CanEditFiles || len(upstream) == 0 {
		return false
	}
	if node.Role != "reviewer" && node.Role != "security-reviewer" {
		return false
	}
	hasEditableUpstream := false
	for _, u := range upstream {
		if u.CanEditFiles {
			hasEditableUpstream = true
		}
		if len(u.FilesTouched) > 0 {
			return false
		}
	}
	return hasEditableUpstream
}

func buildTeamNodePrompt(node workflow.TaskNode, upstream []TeamNodeRun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## jtao Team Node `%s` (role=%s)\n\n", node.ID, node.Role)
	fmt.Fprintf(&b, "- Title: %s\n", node.Title)
	if node.Objective != "" {
		fmt.Fprintf(&b, "- Objective: %s\n", node.Objective)
	}
	fmt.Fprintf(&b, "- CanEditFiles: %v\n", node.CanEditFiles)
	if len(node.OwnedPathPrefixes) > 0 {
		fmt.Fprintf(&b, "- OwnedPathPrefixes: %s\n", strings.Join(node.OwnedPathPrefixes, ", "))
	}
	if len(upstream) > 0 {
		b.WriteString("\n### Upstream Handoffs\n")
		for _, u := range upstream {
			summary := u.Reason
			if u.Handoff != nil && u.Handoff.Summary != "" {
				summary = u.Handoff.Summary
			}
			fmt.Fprintf(&b, "- `%s` (%s · %s): %s (files=%v)\n", u.NodeID, u.Role, u.Status, summary, u.FilesTouched)
		}
	}
	b.WriteString("\nReturn a single JSON handoff object with `status` (pass|blocked|needs-input|fail), `summary`, `findings`, `blockers`, `filesTouched`, `nextSteps`, and `evidenceRefs`.\n")
	return b.String()
}

func parseTeamNodeHandoff(res runner.RunTurnResult, realTouched []string) *workflow.NodeHandoff {
	if res.ParsedJSON == nil {
		status := "blocked"
		summary := "agent output missing structured JSON handoff"
		if res.ExitCode != 0 {
			summary = fmt.Sprintf("agent exited with status %d", res.ExitCode)
		}
		return &workflow.NodeHandoff{Status: status, Summary: summary, Blockers: []string{summary}, FilesTouched: realTouched}
	}
	h := &workflow.NodeHandoff{
		Status:       strings.ToLower(strings.TrimSpace(stringField(res.ParsedJSON, "status"))),
		Summary:      strings.TrimSpace(stringField(res.ParsedJSON, "summary")),
		Findings:     stringSliceField(res.ParsedJSON, "findings"),
		Blockers:     stringSliceField(res.ParsedJSON, "blockers"),
		FilesTouched: mergeUniqueSlice(realTouched, stringSliceField(res.ParsedJSON, "filesTouched")),
		NextSteps:    stringSliceField(res.ParsedJSON, "nextSteps"),
		EvidenceRefs: stringSliceField(res.ParsedJSON, "evidenceRefs"),
	}
	if h.Status == "" {
		h.Status = "pass"
	}
	if h.Summary == "" {
		h.Summary = "node finished with status " + h.Status
	}
	return h
}

func computeExecutionWaves(detail workflow.WorkItemDetail) [][]string {
	indegree := make(map[string]int, len(detail.Nodes))
	dependents := make(map[string][]string)
	order := make(map[string]int, len(detail.Nodes))
	for _, n := range detail.Nodes {
		indegree[n.ID] = 0
		order[n.ID] = n.OrderIndex
	}
	for _, e := range detail.Edges {
		indegree[e.ToNodeID]++
		dependents[e.FromNodeID] = append(dependents[e.FromNodeID], e.ToNodeID)
	}

	var waves [][]string
	remaining := len(detail.Nodes)
	for remaining > 0 {
		var wave []string
		for id, deg := range indegree {
			if deg == 0 {
				wave = append(wave, id)
			}
		}
		if len(wave) == 0 {
			break
		}
		sort.Slice(wave, func(i, j int) bool { return order[wave[i]] < order[wave[j]] })
		waves = append(waves, wave)
		for _, id := range wave {
			delete(indegree, id)
			remaining--
			for _, dep := range dependents[id] {
				indegree[dep]--
			}
		}
	}
	return waves
}

func hasParallelEditableWave(detail workflow.WorkItemDetail) bool {
	editableByID := make(map[string]bool, len(detail.Nodes))
	for _, n := range detail.Nodes {
		editableByID[n.ID] = n.CanEditFiles
	}
	for _, wave := range computeExecutionWaves(detail) {
		editableCount := 0
		for _, id := range wave {
			if editableByID[id] {
				editableCount++
			}
		}
		if editableCount > 1 {
			return true
		}
	}
	return false
}

func expandDownstream(edges []workflow.TaskEdge, target map[string]bool) {
	changed := true
	for changed {
		changed = false
		for _, e := range edges {
			if target[e.FromNodeID] && !target[e.ToNodeID] {
				target[e.ToNodeID] = true
				changed = true
			}
		}
	}
}

func resetNodesForRetry(ctx context.Context, db *sql.DB, workItemID string, nodeIDs map[string]bool) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for id := range nodeIDs {
		if _, err := tx.ExecContext(ctx, `
			UPDATE task_nodes
			SET status = 'pending', execution_token = '', retry_count = 0, updated_at = ?
			WHERE work_item_id = ? AND id = ?
		`, now, workItemID, id); err != nil {
			return err
		}
	}
	_, _ = tx.ExecContext(ctx, `UPDATE work_items SET status = 'executing', updated_at = ? WHERE id = ?`, now, workItemID)
	return tx.Commit()
}

func detectRepeatedBlockedNodes(ctx context.Context, db *sql.DB, workItemID string, blockedSet map[string]bool) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id FROM runs WHERE work_item_id = ? AND mode = 'team'
		ORDER BY created_at DESC LIMIT ?
	`, workItemID, unstableBlockedRunThreshold)
	if err != nil {
		return nil, err
	}
	var recentRunIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			recentRunIDs = append(recentRunIDs, id)
		}
	}
	_ = rows.Close()
	if len(recentRunIDs) < unstableBlockedRunThreshold {
		return nil, nil
	}

	blockCounts := make(map[string]int)
	for _, runID := range recentRunIDs {
		turns, err := loadRunTurns(ctx, db, runID)
		if err != nil {
			continue
		}
		seenInRun := make(map[string]bool)
		for _, t := range turns {
			if blockedSet[t.NodeID] && (t.Outcome == OutcomeBlocked || t.Outcome == OutcomeFailed) && !seenInRun[t.NodeID] {
				seenInRun[t.NodeID] = true
				blockCounts[t.NodeID]++
			}
		}
	}

	var repeated []string
	for id, cnt := range blockCounts {
		if cnt >= unstableBlockedRunThreshold {
			repeated = append(repeated, id)
		}
	}
	sort.Strings(repeated)
	return repeated, nil
}

func capturePreMutationSnapshot(workspaceRoot string) string {
	cmd := exec.Command("git", "stash", "create")
	cmd.Dir = workspaceRoot
	out, err := cmd.Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		return "git-stash:" + strings.TrimSpace(string(out))
	}
	headCmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	headCmd.Dir = workspaceRoot
	headOut, err := headCmd.Output()
	if err == nil && strings.TrimSpace(string(headOut)) != "" {
		return "git-head:" + strings.TrimSpace(string(headOut))
	}
	return ""
}

func stringField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func stringSliceField(m map[string]any, key string) []string {
	switch raw := m[key].(type) {
	case []string:
		var out []string
		for _, s := range raw {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	case []any:
		var out []string
		for _, item := range raw {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	default:
		return nil
	}
}

func mergeUniqueSlice(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	var out []string
	for _, list := range [][]string{a, b} {
		for _, item := range list {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if _, dup := seen[item]; dup {
				continue
			}
			seen[item] = struct{}{}
			out = append(out, item)
		}
	}
	return out
}
