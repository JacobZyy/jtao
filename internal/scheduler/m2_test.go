package scheduler

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JacobZyy/jtao/internal/runner"
	"github.com/JacobZyy/jtao/internal/workflow"
)

func TestSoloPacingQuotaQuietAndSafeBypassSeal(t *testing.T) {
	ctx := context.Background()
	wf := workflow.NewService()
	defer wf.Close()

	// 1. QuietThreshold shuts down unattended loop after 2 consecutive noop polls.
	wsQuiet := t.TempDir()
	_, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: wsQuiet,
		ID:            "quiet-item",
		Title:         "Quiet poll item",
	})
	if err != nil {
		t.Fatal(err)
	}
	noopRunner := &mockRunner{
		turns: []func(context.Context, runner.RunTurnInput) (runner.RunTurnResult, error){
			func(_ context.Context, _ runner.RunTurnInput) (runner.RunTurnResult, error) {
				return runner.RunTurnResult{ParsedJSON: map[string]any{"outcome": "noop", "summary": "still waiting", "selfReport": map[string]any{"progressMade": false}}}, nil
			},
			func(_ context.Context, _ runner.RunTurnInput) (runner.RunTurnResult, error) {
				return runner.RunTurnResult{ParsedJSON: map[string]any{"outcome": "noop", "summary": "still waiting 2", "selfReport": map[string]any{"progressMade": false}}}, nil
			},
		},
	}
	soloQuiet := &SoloService{Workflow: wf}
	resQuiet, err := soloQuiet.Run(ctx, SoloRunInput{
		WorkspaceRoot:  wsQuiet,
		WorkItemID:     "quiet-item",
		MaxIterations:  10,
		Pacing:         PacingConfig{Enabled: true, QuietThreshold: 2},
		RunnerOverride: noopRunner,
	})
	if err != nil {
		t.Fatalf("solo quiet run failed: %v", err)
	}
	if resQuiet.Turns != 2 || resQuiet.Run.Status != "stopped" || !strings.Contains(resQuiet.FinalNote, "unchanged_poll") {
		t.Fatalf("expected quiet shutdown after 2 noop turns, got turns=%d status=%s note=%s", resQuiet.Turns, resQuiet.Run.Status, resQuiet.FinalNote)
	}

	// 2. Rolling quota waits without consuming an iteration, then resumes after window clears.
	wsQuota := t.TempDir()
	_, err = wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: wsQuota,
		ID:            "quota-item",
		Title:         "Two-node quota delivery",
		Nodes: []workflow.TaskNode{
			{ID: "n1", Title: "Node 1", CanEditFiles: true},
			{ID: "n2", Title: "Node 2", CanEditFiles: true},
		},
		Edges: []workflow.TaskEdge{{FromNodeID: "n1", ToNodeID: "n2"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	clock := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	waits := 0
	quotaRunner := &mockRunner{
		turns: []func(context.Context, runner.RunTurnInput) (runner.RunTurnResult, error){
			func(_ context.Context, _ runner.RunTurnInput) (runner.RunTurnResult, error) {
				return runner.RunTurnResult{ParsedJSON: map[string]any{"outcome": "success", "summary": "n1 done", "selfReport": map[string]any{"progressMade": true}}}, nil
			},
			func(_ context.Context, _ runner.RunTurnInput) (runner.RunTurnResult, error) {
				return runner.RunTurnResult{ParsedJSON: map[string]any{"outcome": "success", "summary": "n2 done", "selfReport": map[string]any{"progressMade": true}}}, nil
			},
		},
	}
	soloQuota := &SoloService{
		Workflow: wf,
		NowFunc:  func() time.Time { return clock },
		SleepFunc: func(_ context.Context, d time.Duration) error {
			waits++
			clock = clock.Add(d + time.Millisecond)
			return nil
		},
	}
	resQuota, err := soloQuota.Run(ctx, SoloRunInput{
		WorkspaceRoot: wsQuota,
		WorkItemID:    "quota-item",
		MaxIterations: 5,
		Pacing: PacingConfig{
			Enabled:       true,
			QuotaMaxTurns: 1,
			QuotaWindow:   10 * time.Minute,
		},
		RunnerOverride: quotaRunner,
	})
	if err != nil {
		t.Fatalf("solo quota run failed: %v", err)
	}
	if waits != 1 || resQuota.Turns != 2 || resQuota.Run.Status != "done" {
		t.Fatalf("expected 1 quota wait + 2 turns + done, got waits=%d turns=%d status=%s", waits, resQuota.Turns, resQuota.Run.Status)
	}

	// 3. SafeBypass grants 1 read-only probe turn before human-gate stop, and code-level seals any material claim.
	wsBypass := t.TempDir()
	_, err = wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: wsBypass,
		ID:            "bypass-item",
		Title:         "Bypass probe test",
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawProbePrompt bool
	bypassRunner := &mockRunner{
		turns: []func(context.Context, runner.RunTurnInput) (runner.RunTurnResult, error){
			func(_ context.Context, _ runner.RunTurnInput) (runner.RunTurnResult, error) {
				return runner.RunTurnResult{ParsedJSON: map[string]any{"outcome": "human-gate", "summary": "need decision", "selfReport": map[string]any{"progressMade": false}}}, nil
			},
			// Agent tries to claim outcome=success + progressMade=true during safe_bypass -> MUST be sealed down to human-gate!
			func(_ context.Context, in runner.RunTurnInput) (runner.RunTurnResult, error) {
				sawProbePrompt = strings.Contains(in.UserPrompt, "SAFE-BYPASS READ-ONLY PROBE")
				return runner.RunTurnResult{ParsedJSON: map[string]any{"outcome": "success", "summary": "attempted write", "selfReport": map[string]any{"progressMade": true}}}, nil
			},
		},
	}
	soloBypass := &SoloService{Workflow: wf}
	resBypass, err := soloBypass.Run(ctx, SoloRunInput{
		WorkspaceRoot:  wsBypass,
		WorkItemID:     "bypass-item",
		MaxIterations:  5,
		Pacing:         PacingConfig{Enabled: true, SafeBypass: true},
		RunnerOverride: bypassRunner,
	})
	if err != nil {
		t.Fatalf("solo bypass run failed: %v", err)
	}
	if !sawProbePrompt || resBypass.Turns != 2 || resBypass.Run.Status != "human-gate" || !strings.Contains(resBypass.FinalNote, "[safe-bypass]") {
		t.Fatalf("expected safe-bypass probe sealed to human-gate, got saw=%v turns=%d status=%s note=%s", sawProbePrompt, resBypass.Turns, resBypass.Run.Status, resBypass.FinalNote)
	}
}

type parallelTeamRunner struct {
	handler func(ctx context.Context, in runner.RunTurnInput) (runner.RunTurnResult, error)
}

func (p parallelTeamRunner) ID() string                          { return "mock-team" }
func (p parallelTeamRunner) CheckReadiness() runner.ReadinessCheck { return runner.ReadinessCheck{OK: true, Provider: "mock-team"} }
func (p parallelTeamRunner) RunTurn(ctx context.Context, in runner.RunTurnInput) (runner.RunTurnResult, error) {
	return p.handler(ctx, in)
}

func TestTeamParallelDAGAndMergeGateSuccess(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	wf := workflow.NewService()
	defer wf.Close()

	_, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: ws,
		ID:            "team-feature",
		Title:         "Feature delivery",
		Blueprint:     "feature",
	})
	if err != nil {
		t.Fatal(err)
	}

	var activeReviewers int32
	var maxConcurrentReviewers int32

	team := &TeamService{Workflow: wf}
	preview, err := team.Plan(ctx, TeamRunInput{WorkspaceRoot: ws, WorkItemID: "team-feature", Concurrency: 3})
	if err != nil {
		t.Fatalf("Team Plan failed: %v", err)
	}
	if len(preview.Waves) != 4 || len(preview.Waves[2]) != 2 {
		t.Fatalf("expected 4 waves with 2 parallel review nodes in wave 3, got %+v", preview.Waves)
	}

	res, err := team.Run(ctx, TeamRunInput{
		WorkspaceRoot: ws,
		WorkItemID:    "team-feature",
		Concurrency:   3,
		RunnerOverride: parallelTeamRunner{
			handler: func(_ context.Context, in runner.RunTurnInput) (runner.RunTurnResult, error) {
				if strings.HasPrefix(in.UserPrompt, "## jtao Team Node `implement`") {
					return runner.RunTurnResult{ParsedJSON: map[string]any{
						"status":       "pass",
						"summary":      "implemented feature",
						"filesTouched": []string{"internal/checkout/service.go"},
					}}, nil
				}
				if strings.HasPrefix(in.UserPrompt, "## jtao Team Node `review`") || strings.HasPrefix(in.UserPrompt, "## jtao Team Node `security`") {
					cur := atomic.AddInt32(&activeReviewers, 1)
					for {
						prev := atomic.LoadInt32(&maxConcurrentReviewers)
						if cur <= prev || atomic.CompareAndSwapInt32(&maxConcurrentReviewers, prev, cur) {
							break
						}
					}
					time.Sleep(40 * time.Millisecond)
					atomic.AddInt32(&activeReviewers, -1)
					return runner.RunTurnResult{ParsedJSON: map[string]any{
						"status":   "pass",
						"summary":  "review passed",
						"findings": []string{"looks clean"},
					}}, nil
				}
				return runner.RunTurnResult{ParsedJSON: map[string]any{
					"status":  "pass",
					"summary": "plan ready",
				}}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("Team Run failed: %v", err)
	}
	if !res.OK || res.WorkItem.Status != workflow.WorkDone {
		t.Fatalf("expected team feature done, got ok=%v status=%s note=%s", res.OK, res.WorkItem.Status, res.FinalNote)
	}
	if atomic.LoadInt32(&maxConcurrentReviewers) < 2 {
		t.Fatalf("expected review and security to execute concurrently, maxConcurrent=%d", maxConcurrentReviewers)
	}
}

func TestTeamMergeGateOwnershipViolationAndParallelConflict(t *testing.T) {
	// 1. Ownership violation blocks merge gate.
	resOwned := EvaluateMergeGate("writers", []TeamNodeRun{
		{
			NodeID:            "w1",
			Role:              "implementer",
			CanEditFiles:      true,
			OwnedPathPrefixes: []string{"internal/memory/"},
			Status:            "done",
			FilesTouched:      []string{"internal/workflow/dag.go"},
		},
	})
	if resOwned.OK || len(resOwned.OwnershipViolations) != 1 {
		t.Fatalf("expected ownership violation block, got %+v", resOwned)
	}

	// 2. Parallel conflict on same file blocks merge gate.
	resConflict := EvaluateMergeGate("writers", []TeamNodeRun{
		{
			NodeID:            "w1",
			Role:              "implementer",
			CanEditFiles:      true,
			OwnedPathPrefixes: []string{"internal/"},
			Status:            "done",
			FilesTouched:      []string{"internal/shared.go"},
		},
		{
			NodeID:            "w2",
			Role:              "implementer",
			CanEditFiles:      true,
			OwnedPathPrefixes: []string{"internal/"},
			Status:            "done",
			FilesTouched:      []string{"internal/shared.go"},
		},
	})
	if resConflict.OK || len(resConflict.Conflicts) != 1 || resConflict.Conflicts[0].File != "internal/shared.go" {
		t.Fatalf("expected file conflict block, got %+v", resConflict)
	}
}

func TestTeamAutoSkipReadOnlyReviewAndRetryBlockedWithInstabilityGuard(t *testing.T) {
	ctx := context.Background()
	wf := workflow.NewService()
	defer wf.Close()

	// 1. Auto-skip read-only review when implement produced 0 touched files.
	wsSkip := t.TempDir()
	_, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: wsSkip,
		ID:            "auto-skip-item",
		Title:         "No-edit change",
		Blueprint:     "bugfix",
	})
	if err != nil {
		t.Fatal(err)
	}
	var reviewCalled atomic.Int32
	team := &TeamService{Workflow: wf}
	resSkip, err := team.Run(ctx, TeamRunInput{
		WorkspaceRoot: wsSkip,
		WorkItemID:    "auto-skip-item",
		RunnerOverride: parallelTeamRunner{
			handler: func(_ context.Context, in runner.RunTurnInput) (runner.RunTurnResult, error) {
				if strings.HasPrefix(in.UserPrompt, "## jtao Team Node `review`") {
					reviewCalled.Add(1)
				}
				return runner.RunTurnResult{ParsedJSON: map[string]any{"status": "pass", "summary": "no file edits needed"}}, nil
			},
		},
	})
	if err != nil || !resSkip.OK {
		t.Fatalf("auto-skip run failed: err=%v res=%+v", err, resSkip)
	}
	if reviewCalled.Load() != 0 {
		t.Fatalf("expected read-only review to auto-complete without model call, called=%d", reviewCalled.Load())
	}

	// 2. RetryBlocked seeds completed nodes, retries only blocked node + merge gate, and triggers instability guard after 2 blocked runs.
	wsRetry := t.TempDir()
	_, err = wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: wsRetry,
		ID:            "retry-item",
		Title:         "Retry blocked test",
		Blueprint:     "bugfix",
	})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	calls := make(map[string]int)
	failImplement := true

	runHandler := parallelTeamRunner{
		handler: func(_ context.Context, in runner.RunTurnInput) (runner.RunTurnResult, error) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case strings.HasPrefix(in.UserPrompt, "## jtao Team Node `plan`"):
				calls["plan"]++
				return runner.RunTurnResult{ParsedJSON: map[string]any{"status": "pass", "summary": "plan ok"}}, nil
			case strings.HasPrefix(in.UserPrompt, "## jtao Team Node `implement`"):
				calls["implement"]++
				if failImplement {
					return runner.RunTurnResult{ParsedJSON: map[string]any{"status": "blocked", "summary": "missing spec", "blockers": []string{"need spec"}}}, nil
				}
				return runner.RunTurnResult{ParsedJSON: map[string]any{"status": "pass", "summary": "fixed", "filesTouched": []string{"internal/fix.go"}}}, nil
			default:
				calls["review"]++
				return runner.RunTurnResult{ParsedJSON: map[string]any{"status": "pass", "summary": "review ok"}}, nil
			}
		},
	}

	// First run: plan passes, implement blocks, review cascade-blocks.
	r1, err := team.Run(ctx, TeamRunInput{WorkspaceRoot: wsRetry, WorkItemID: "retry-item", RunnerOverride: runHandler})
	if err != nil || r1.OK {
		t.Fatalf("expected r1 blocked, got err=%v ok=%v", err, r1.OK)
	}
	if calls["plan"] != 1 || calls["implement"] != 1 || calls["review"] != 0 {
		t.Fatalf("unexpected call counts after r1: %+v", calls)
	}

	// Retry 1 (still failing implement): plan must be SEEDED (NOT re-called!).
	r2, err := team.RetryBlocked(ctx, TeamRetryInput{WorkspaceRoot: wsRetry, RunID: r1.Run.ID, RunnerOverride: runHandler})
	if err != nil || r2.OK || r2.SeededCount != 1 {
		t.Fatalf("expected r2 blocked with 1 seeded node, got err=%v ok=%v seeded=%d", err, r2.OK, r2.SeededCount)
	}
	if calls["plan"] != 1 || calls["implement"] != 2 {
		t.Fatalf("plan should not have re-run on retry-blocked: %+v", calls)
	}

	// Retry 2 without --force: instability guard must refuse blind retry after 2 consecutive blocked runs!
	if _, err := team.RetryBlocked(ctx, TeamRetryInput{WorkspaceRoot: wsRetry, RunID: r2.Run.ID, RunnerOverride: runHandler}); err == nil || !strings.Contains(err.Error(), "refusing blind retry-blocked") {
		t.Fatalf("expected instability guard refusal, got %v", err)
	}

	// Retry 2 with --force and fixed implement -> succeeds cleanly without re-running plan!
	failImplement = false
	r3, err := team.RetryBlocked(ctx, TeamRetryInput{WorkspaceRoot: wsRetry, RunID: r2.Run.ID, Force: true, RunnerOverride: runHandler})
	if err != nil || !r3.OK || r3.WorkItem.Status != workflow.WorkDone {
		t.Fatalf("expected forced retry to complete, got err=%v ok=%v status=%s note=%s", err, r3.OK, r3.WorkItem.Status, r3.FinalNote)
	}
	if calls["plan"] != 1 || calls["implement"] != 3 || calls["review"] != 1 {
		t.Fatalf("unexpected final call counts: %+v", calls)
	}
}
