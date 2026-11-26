package scheduler

import (
	"context"
	"os"
	"path/filepath"
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
