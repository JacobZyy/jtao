package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/JacobZyy/jtao/internal/memory"
	"github.com/JacobZyy/jtao/internal/runner"
	"github.com/JacobZyy/jtao/internal/workflow"
)

type mockRunner struct {
	turns []func(ctx context.Context, in runner.RunTurnInput) (runner.RunTurnResult, error)
	idx   int
}

func (m *mockRunner) ID() string                     { return "mock-codex" }
func (m *mockRunner) CheckReadiness() runner.ReadinessCheck { return runner.ReadinessCheck{OK: true, Provider: "mock-codex"} }
func (m *mockRunner) RunTurn(ctx context.Context, in runner.RunTurnInput) (runner.RunTurnResult, error) {
	fn := m.turns[m.idx]
	m.idx++
	return fn(ctx, in)
}

type memoryAdapter struct {
	svc *memory.SQLiteService
}

func (a memoryAdapter) EnsureSession(ctx context.Context, ws, sessionID, agent, goal, workItemID string) (string, error) {
	sess, _, err := a.svc.EnsureSession(ctx, ws, sessionID, agent, filepath.Base(ws), goal, workItemID)
	return sess.ID, err
}

func (a memoryAdapter) RecallForTurn(ctx context.Context, ws, query, sessionID, agent, workItemID string) (string, error) {
	res, err := a.svc.RecallForTurn(ctx, memory.TurnRecallInput{
		WorkspaceRoot: ws,
		Message:       query,
		SessionID:     sessionID,
		Agent:         agent,
		WorkItemID:    workItemID,
	})
	return res.Text, err
}

func (a memoryAdapter) Checkpoint(ctx context.Context, ws, sessionID, workItemID, status, summary string, nextActions, artifacts []string) error {
	_, err := a.svc.AddCheckpoint(ctx, ws, memory.Checkpoint{
		SessionID:   sessionID,
		WorkItemID:  workItemID,
		Status:      status,
		Summary:     summary,
		NextActions: nextActions,
		Artifacts:   artifacts,
	})
	return err
}

func (a memoryAdapter) UpdateContinuity(ctx context.Context, ws, sessionID, workItemID, intent, summary string, touchedFiles, nextActions []string) error {
	_, err := a.svc.UpdateContinuity(ctx, ws, memory.Continuity{
		SessionID:    sessionID,
		WorkItemID:   workItemID,
		Intent:       intent,
		Summary:      summary,
		TouchedFiles: touchedFiles,
		NextActions:  nextActions,
	})
	return err
}

func (a memoryAdapter) StoreArtifact(ctx context.Context, ws, sessionID, workItemID, tool, content string) (string, error) {
	art, err := a.svc.StoreArtifact(ctx, ws, sessionID, workItemID, tool, content)
	return art.ID, err
}

func TestSoloGraphDrivenLoopWithRealReceiptRetryAndMemory(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()

	wf := workflow.NewService()
	defer wf.Close()
	mem := memory.NewService()
	defer mem.Close()

	// Create 2-node DAG: node-a requires a.ok to exist, node-b requires b.ok to exist.
	_, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: ws,
		ID:            "checkout-delivery",
		Title:         "Checkout Delivery",
		Objective:     "Deliver A then B with real command receipts",
		SetActive:     true,
		Nodes: []workflow.TaskNode{
			{
				ID:           "node-a",
				Title:        "Create a.ok",
				CanEditFiles: true,
				MaxRetries:   2,
				VerifySpec:   &workflow.VerifyCommandSpec{Command: "test", Args: []string{"-f", "a.ok"}, ExpectedExitCode: 0},
			},
			{
				ID:           "node-b",
				Title:        "Create b.ok",
				CanEditFiles: true,
				MaxRetries:   2,
				VerifySpec:   &workflow.VerifyCommandSpec{Command: "test", Args: []string{"-f", "b.ok"}, ExpectedExitCode: 0},
			},
		},
		Edges: []workflow.TaskEdge{{FromNodeID: "node-a", ToNodeID: "node-b"}},
	})
	if err != nil {
		t.Fatalf("CreateWorkItem failed: %v", err)
	}

	mock := &mockRunner{
		turns: []func(context.Context, runner.RunTurnInput) (runner.RunTurnResult, error){
			// Turn 1: agent claims success on node-a WITHOUT creating a.ok -> real receipt fails -> retry!
			func(_ context.Context, _ runner.RunTurnInput) (runner.RunTurnResult, error) {
				return runner.RunTurnResult{
					ExitCode: 0,
					ParsedJSON: map[string]any{
						"outcome":    "success",
						"summary":    "claimed node-a done early",
						"selfReport": map[string]any{"progressMade": true},
					},
				}, nil
			},
			// Turn 2: agent actually creates a.ok -> real receipt exitCode=0 -> advances to node-b!
			func(_ context.Context, in runner.RunTurnInput) (runner.RunTurnResult, error) {
				_ = os.WriteFile(filepath.Join(in.ExecutionCwd, "a.ok"), []byte("ok"), 0o644)
				return runner.RunTurnResult{
					ExitCode: 0,
					ParsedJSON: map[string]any{
						"outcome":    "success",
						"summary":    "created a.ok",
						"selfReport": map[string]any{"progressMade": true},
					},
				}, nil
			},
			// Turn 3: agent creates b.ok -> real receipt exitCode=0 -> entire WorkItem completes!
			func(_ context.Context, in runner.RunTurnInput) (runner.RunTurnResult, error) {
				_ = os.WriteFile(filepath.Join(in.ExecutionCwd, "b.ok"), []byte("ok"), 0o644)
				return runner.RunTurnResult{
					ExitCode: 0,
					ParsedJSON: map[string]any{
						"outcome":    "success",
						"summary":    "created b.ok",
						"selfReport": map[string]any{"progressMade": true},
					},
				}, nil
			},
		},
	}

	solo := &SoloService{
		Workflow: wf,
		Memory:   memoryAdapter{svc: mem},
	}
	res, err := solo.Run(ctx, SoloRunInput{
		WorkspaceRoot:  ws,
		WorkItemID:     "checkout-delivery",
		MaxIterations:  5,
		RunnerOverride: mock,
	})
	if err != nil {
		t.Fatalf("Solo Run failed: %v", err)
	}
	if res.Run.Status != "done" || res.Turns != 3 || res.WorkItem.Status != workflow.WorkDone {
		t.Fatalf("unexpected solo result: status=%s turns=%d wiStatus=%s note=%s", res.Run.Status, res.Turns, res.WorkItem.Status, res.FinalNote)
	}

	// Verify checkpoints were written to MemoryPort linked by workItemId.
	wiMem, err := mem.RecallForWorkItem(ctx, ws, "checkout-delivery", 4000)
	if err != nil || len(wiMem.Checkpoints) != 3 {
		t.Fatalf("expected 3 checkpoints in memory, got err=%v count=%d", err, len(wiMem.Checkpoints))
	}
}

func TestSoloFailClosedWhenProcessTreeSurvivesTimeout(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()

	wf := workflow.NewService()
	defer wf.Close()
	_, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: ws,
		ID:            "orphan-guard",
		Title:         "Orphan guard test",
	})
	if err != nil {
		t.Fatal(err)
	}

	mock := &mockRunner{
		turns: []func(context.Context, runner.RunTurnInput) (runner.RunTurnResult, error){
			func(_ context.Context, _ runner.RunTurnInput) (runner.RunTurnResult, error) {
				return runner.RunTurnResult{TimedOut: true, TreeAlive: true, ChildPID: 99999}, nil
			},
		},
	}
	solo := &SoloService{Workflow: wf}
	res, err := solo.Run(ctx, SoloRunInput{
		WorkspaceRoot:  ws,
		WorkItemID:     "orphan-guard",
		MaxIterations:  5,
		RunnerOverride: mock,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.Run.Status != "blocked" || res.Turns != 1 {
		t.Fatalf("expected immediate fail-closed blocked after 1 turn, got status=%s turns=%d", res.Run.Status, res.Turns)
	}
}

func TestOutputSchemasSatisfyStrictRequiredContract(t *testing.T) {
	schemas := map[string][]byte{
		"soloTurnOutputSchema": soloTurnOutputSchema,
		"teamNodeOutputSchema": teamNodeOutputSchema,
	}
	var assertStrict func(t *testing.T, path string, node map[string]any)
	assertStrict = func(t *testing.T, path string, node map[string]any) {
		t.Helper()
		props, _ := node["properties"].(map[string]any)
		if node["type"] == "object" && node["additionalProperties"] == false {
			rawReq, _ := node["required"].([]any)
			var req []string
			for _, item := range rawReq {
				if key, ok := item.(string); ok {
					req = append(req, key)
				}
			}
			for key := range props {
				if !slices.Contains(req, key) {
					t.Errorf("%s: property %q missing from required (%v)", path, key, req)
				}
			}
		}
		for key, child := range props {
			if childObj, ok := child.(map[string]any); ok {
				assertStrict(t, path+"."+key, childObj)
			}
		}
	}

	for name, raw := range schemas {
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("%s: invalid JSON: %v", name, err)
		}
		assertStrict(t, name, parsed)
	}
}

func TestNonZeroExitSurfacesFailureOutputExcerpt(t *testing.T) {
	res := runner.RunTurnResult{
		ExitCode:  2,
		RawOutput: "banner line\nerror: unexpected argument '--full-auto' found\n\nUsage: codex exec",
	}
	outcome, _, summary, _, _, _ := interpretTurnResult(res, nil)
	if outcome != OutcomeInfraRetry {
		t.Fatalf("expected OutcomeInfraRetry, got %s", outcome)
	}
	if !strings.Contains(summary, "unexpected argument '--full-auto'") {
		t.Fatalf("expected solo summary to include CLI error line, got %q", summary)
	}

	handoff := parseTeamNodeHandoff(res, nil)
	if handoff.Status != "blocked" {
		t.Fatalf("expected blocked handoff on non-zero exit, got %+v", handoff)
	}
	if !strings.Contains(handoff.Summary, "unexpected argument '--full-auto'") {
		t.Fatalf("expected team handoff summary to include CLI error line, got %q", handoff.Summary)
	}
}

func TestSoloResumeReopensBlockedNodeAndCompletes(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()

	wf := workflow.NewService()
	defer wf.Close()
	_, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: ws,
		ID:            "resume-blocked",
		Title:         "Resume blocked node",
		Nodes: []workflow.TaskNode{
			{ID: "n1", NodeKind: "plan", Title: "Plan", MaxRetries: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	solo := &SoloService{Workflow: wf}
	failingMock := &mockRunner{
		turns: []func(context.Context, runner.RunTurnInput) (runner.RunTurnResult, error){
			func(_ context.Context, _ runner.RunTurnInput) (runner.RunTurnResult, error) {
				return runner.RunTurnResult{ExitCode: 2, RawOutput: "error: unexpected argument '--full-auto'"}, nil
			},
			func(_ context.Context, _ runner.RunTurnInput) (runner.RunTurnResult, error) {
				return runner.RunTurnResult{ExitCode: 2, RawOutput: "error: unexpected argument '--full-auto'"}, nil
			},
		},
	}
	first, err := solo.Run(ctx, SoloRunInput{
		WorkspaceRoot:  ws,
		WorkItemID:     "resume-blocked",
		MaxIterations:  5,
		RunnerOverride: failingMock,
	})
	if err != nil {
		t.Fatalf("initial Run failed: %v", err)
	}
	if first.Run.Status != "human-gate" {
		t.Fatalf("expected initial run to pause in human-gate, got %s", first.Run.Status)
	}

	passingMock := &mockRunner{
		turns: []func(context.Context, runner.RunTurnInput) (runner.RunTurnResult, error){
			func(_ context.Context, _ runner.RunTurnInput) (runner.RunTurnResult, error) {
				return runner.RunTurnResult{
					ExitCode: 0,
					ParsedJSON: map[string]any{
						"outcome":    "success",
						"summary":    "plan done after fix",
						"selfReport": map[string]any{"progressMade": true},
					},
				}, nil
			},
		},
	}
	resumed, err := solo.Resume(ctx, ws, first.Run.ID, 5, 0, PacingConfig{}, passingMock)
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	if resumed.Run.Status != "done" || resumed.Turns != 1 {
		t.Fatalf("expected resumed run to reopen blocked node and finish done, got status=%s turns=%d note=%q",
			resumed.Run.Status, resumed.Turns, resumed.FinalNote)
	}
}

func TestSoloVerifySpecResolvesHomeCargoAndFastBlocksOnENOENT(t *testing.T) {
	ctx := context.Background()
	fakeHome := t.TempDir()
	cargoDir := filepath.Join(fakeHome, ".cargo", "bin")
	if err := os.MkdirAll(cargoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cargoDir, "cargo"), []byte("#!/bin/sh\necho 'test result: ok. 21 passed'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", fakeHome)
	t.Setenv("PATH", "/usr/bin:/bin")

	wf := workflow.NewService()
	defer wf.Close()

	// 1. Minimal PATH (/usr/bin:/bin) still finds cargo under $HOME/.cargo/bin and passes verifySpec.
	wsOK := t.TempDir()
	if _, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: wsOK,
		ID:            "cargo-fallback-ok",
		Title:         "Cargo fallback verification",
		Nodes: []workflow.TaskNode{
			{
				ID:           "incremental-stop-hook",
				Title:        "Run cargo test",
				CanEditFiles: true,
				MaxRetries:   2,
				VerifySpec: &workflow.VerifyCommandSpec{
					Command:          "cargo",
					Args:             []string{"test", "-p", "nlab", "--test", "quality"},
					ExpectedExitCode: 0,
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	okMock := &mockRunner{
		turns: []func(context.Context, runner.RunTurnInput) (runner.RunTurnResult, error){
			func(_ context.Context, _ runner.RunTurnInput) (runner.RunTurnResult, error) {
				return runner.RunTurnResult{
					ExitCode: 0,
					ParsedJSON: map[string]any{
						"outcome":    "success",
						"summary":    "implemented stop hook",
						"selfReport": map[string]any{"progressMade": true},
					},
				}, nil
			},
		},
	}
	solo := &SoloService{Workflow: wf}
	resOK, err := solo.Run(ctx, SoloRunInput{
		WorkspaceRoot:  wsOK,
		WorkItemID:     "cargo-fallback-ok",
		MaxIterations:  5,
		RunnerOverride: okMock,
	})
	if err != nil {
		t.Fatalf("Solo Run cargo fallback failed: %v", err)
	}
	if resOK.Run.Status != "done" || resOK.Turns != 1 || resOK.WorkItem.Status != workflow.WorkDone {
		t.Fatalf("expected solo run done in 1 turn via ~/.cargo/bin fallback, got status=%s turns=%d wi=%s note=%q",
			resOK.Run.Status, resOK.Turns, resOK.WorkItem.Status, resOK.FinalNote)
	}

	// 2. Truly missing verifySpec command fast-blocks on turn 1 without burning agent retries.
	wsMissing := t.TempDir()
	if _, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: wsMissing,
		ID:            "cargo-enoent-fast-block",
		Title:         "Missing verify binary fast block",
		Nodes: []workflow.TaskNode{
			{
				ID:           "n1",
				Title:        "Run missing command",
				CanEditFiles: true,
				MaxRetries:   2,
				VerifySpec: &workflow.VerifyCommandSpec{
					Command:          "jtao-nonexistent-cargo",
					Args:             []string{"test"},
					ExpectedExitCode: 0,
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	calls := 0
	enoentMock := &mockRunner{
		turns: []func(context.Context, runner.RunTurnInput) (runner.RunTurnResult, error){
			func(_ context.Context, _ runner.RunTurnInput) (runner.RunTurnResult, error) {
				calls++
				return runner.RunTurnResult{
					ExitCode: 0,
					ParsedJSON: map[string]any{
						"outcome":    "success",
						"summary":    "code ready",
						"selfReport": map[string]any{"progressMade": true},
					},
				}, nil
			},
			func(_ context.Context, _ runner.RunTurnInput) (runner.RunTurnResult, error) {
				calls++
				t.Fatalf("unexpected retry turn after verifySpec ENOENT")
				return runner.RunTurnResult{}, nil
			},
		},
	}

	resMissing, err := solo.Run(ctx, SoloRunInput{
		WorkspaceRoot:  wsMissing,
		WorkItemID:     "cargo-enoent-fast-block",
		MaxIterations:  5,
		RunnerOverride: enoentMock,
	})
	if err != nil {
		t.Fatalf("Solo Run ENOENT returned error: %v", err)
	}
	if calls != 1 || resMissing.Turns != 1 {
		t.Fatalf("expected 1 turn (no blind retries on ENOENT), got calls=%d turns=%d", calls, resMissing.Turns)
	}
	if resMissing.Run.Status != "blocked" {
		t.Fatalf("expected run status=blocked, got %s (note=%q)", resMissing.Run.Status, resMissing.FinalNote)
	}
	if !strings.Contains(resMissing.FinalNote, "not found") || !strings.Contains(resMissing.FinalNote, "jtao-nonexistent-cargo") {
		t.Fatalf("expected FinalNote to surface not found command detail, got %q", resMissing.FinalNote)
	}
	if strings.Contains(resMissing.FinalNote, "expected 0, got -1") {
		t.Fatalf("FinalNote should not degrade to exit-code mismatch only: %q", resMissing.FinalNote)
	}
	if resMissing.Run.SummaryText != resMissing.FinalNote {
		t.Fatalf("expected Run.SummaryText to match FinalNote, got summary=%q finalNote=%q", resMissing.Run.SummaryText, resMissing.FinalNote)
	}

	detail, err := wf.GetWorkItem(ctx, wsMissing, "cargo-enoent-fast-block")
	if err != nil || detail == nil || len(detail.Nodes) != 1 {
		t.Fatalf("GetWorkItem failed: err=%v detail=%+v", err, detail)
	}
	node := detail.Nodes[0]
	if node.Status != workflow.NodeBlocked || node.RetryCount != 0 {
		t.Fatalf("expected node blocked with retryCount=0, got status=%s retryCount=%d", node.Status, node.RetryCount)
	}
	var receiptID string
	for _, ref := range node.EvidenceRefs {
		if strings.HasPrefix(ref, "receipt:") {
			receiptID = ref
			break
		}
	}
	if receiptID == "" {
		t.Fatalf("expected persisted receipt ref on node, got %v", node.EvidenceRefs)
	}
	rec, err := wf.GetReceipt(ctx, wsMissing, receiptID)
	if err != nil || rec == nil {
		t.Fatalf("GetReceipt failed: err=%v rec=%+v", err, rec)
	}
	if rec.ExitCode != -1 || !strings.Contains(rec.ExecError, "not found") || !strings.Contains(rec.OutputExcerpt, "not found") {
		t.Fatalf("expected receipt to record not found execError and excerpt, got %+v", rec)
	}
}

func TestTeamVerifySpecFastBlocksAndSurfacesExecError(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "/usr/bin:/bin")

	wf := workflow.NewService()
	defer wf.Close()
	if _, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: ws,
		ID:            "team-enoent",
		Title:         "Team verify ENOENT",
		Nodes: []workflow.TaskNode{
			{
				ID:           "implement",
				Title:        "Implement with missing verify binary",
				Role:         "implementer",
				CanEditFiles: true,
				MaxRetries:   2,
				VerifySpec: &workflow.VerifyCommandSpec{
					Command:          "jtao-nonexistent-cargo",
					Args:             []string{"test"},
					ExpectedExitCode: 0,
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	team := &TeamService{Workflow: wf}
	res, err := team.Run(ctx, TeamRunInput{
		WorkspaceRoot: ws,
		WorkItemID:    "team-enoent",
		RunnerOverride: parallelTeamRunner{
			handler: func(_ context.Context, _ runner.RunTurnInput) (runner.RunTurnResult, error) {
				return runner.RunTurnResult{
					ExitCode: 0,
					ParsedJSON: map[string]any{
						"status":  "pass",
						"summary": "implemented feature",
					},
				}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("Team Run failed: %v", err)
	}
	if res.OK || res.Run.Status != "blocked" {
		t.Fatalf("expected team run blocked, got ok=%v status=%s note=%q", res.OK, res.Run.Status, res.FinalNote)
	}
	if !strings.Contains(res.FinalNote, "not found") || !strings.Contains(res.FinalNote, "jtao-nonexistent-cargo") {
		t.Fatalf("expected team FinalNote/SummaryText to surface missing command reason, got %q", res.FinalNote)
	}
	detail, err := wf.GetWorkItem(ctx, ws, "team-enoent")
	if err != nil || detail == nil || detail.Nodes[0].Status != workflow.NodeBlocked || detail.Nodes[0].RetryCount != 0 {
		t.Fatalf("expected team node blocked with 0 retries, got err=%v detail=%+v", err, detail)
	}
}
