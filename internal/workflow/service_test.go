package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicyClassifyExplicitIntentsAndNoProseGuessing(t *testing.T) {
	svc := NewService()
	defer svc.Close()

	// Free prose without explicit intent falls back to deterministic guarded (never guessed as read-only or planned).
	d := svc.ClassifyPolicy(PolicyClassifyInput{
		Message: "please explain why checkout fails and maybe refactor everything",
	})
	if d.Disposition != DispositionGuarded || d.Persistence != PersistNone || d.ExecutionHost != HostSingle {
		t.Fatalf("unexpected default guarded decision: %+v", d)
	}

	// Explicit /direct is direct.
	dDirect := svc.ClassifyPolicy(PolicyClassifyInput{Message: "/direct inspect config"})
	if dDirect.Disposition != DispositionDirect {
		t.Fatalf("expected direct, got %+v", dDirect)
	}

	// Explicit /team creates planned work targeting team host.
	dTeam := svc.ClassifyPolicy(PolicyClassifyInput{Message: "/team implement checkout"})
	if dTeam.Disposition != DispositionPlanned || dTeam.ExecutionHost != HostTeam || dTeam.Persistence != PersistCreate {
		t.Fatalf("unexpected team decision: %+v", dTeam)
	}

	// Resume without active work item is blocked.
	dMissing := svc.ClassifyPolicy(PolicyClassifyInput{Message: "继续"})
	if dMissing.Disposition != DispositionBlocked || dMissing.Continuation != "missing" {
		t.Fatalf("expected blocked missing resume, got %+v", dMissing)
	}

	// Same-session ack reuses active item; cross-session ack is noop.
	active := &WorkItem{ID: "wi-1", Status: WorkExecuting, Route: "harness", OriginClient: "codex", OriginSessionID: "sess-1"}
	dSame := svc.ClassifyPolicy(PolicyClassifyInput{Message: "好的", Client: "codex", SessionID: "sess-1", ActiveItem: active})
	if dSame.Persistence != PersistReuse || dSame.Continuation != "same-session-ack" {
		t.Fatalf("expected same-session reuse, got %+v", dSame)
	}
	dDiff := svc.ClassifyPolicy(PolicyClassifyInput{Message: "好的", Client: "codex", SessionID: "sess-2", ActiveItem: active})
	if dDiff.Disposition != DispositionNoop {
		t.Fatalf("expected cross-session ack noop, got %+v", dDiff)
	}
}

func TestDAGCycleValidationAndBlueprintExpansion(t *testing.T) {
	svc := NewService()
	defer svc.Close()

	// Cycle must fail.
	err := ValidateGraph(
		[]TaskNode{{ID: "a"}, {ID: "b"}},
		[]TaskEdge{{FromNodeID: "a", ToNodeID: "b"}, {FromNodeID: "b", ToNodeID: "a"}},
	)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}

	// Unsafe workItemId path traversal must fail closed.
	if _, err := CanonicalWorkItemID("../../etc/passwd"); err == nil {
		t.Fatalf("expected path traversal rejection")
	}

	// Feature blueprint expands into plan -> implement -> [review, security] -> merge.final-checks.
	nodes, edges, err := svc.ExpandBlueprint("feature", "Build payment gateway")
	if err != nil {
		t.Fatalf("ExpandBlueprint feature failed: %v", err)
	}
	if err := ValidateGraph(nodes, edges); err != nil {
		t.Fatalf("expanded feature graph invalid: %v", err)
	}
	hasMergeGate := false
	for _, n := range nodes {
		if n.NodeKind == NodeKindMergeGate && n.ID == "merge.final-checks" {
			hasMergeGate = true
		}
	}
	if !hasMergeGate {
		t.Fatalf("expected auto-synthesized merge.final-checks node, got %+v", nodes)
	}
}

func TestWorkItemReceiptGatedProgressionAndMarkdownProjection(t *testing.T) {
	ctx := context.Background()
	svc := NewService()
	defer svc.Close()
	ws := t.TempDir()

	specA := &VerifyCommandSpec{Command: "go", Args: []string{"test", "./a"}, ExpectedExitCode: 0}
	specB := &VerifyCommandSpec{Command: "go", Args: []string{"test", "./b"}, ExpectedExitCode: 0}

	detail, err := svc.CreateWorkItem(ctx, CreateWorkItemInput{
		WorkspaceRoot:  ws,
		ID:             "checkout-refactor",
		Title:          "Checkout refactor",
		Objective:      "Reject invalid checkout input then generate receipt",
		SetActive:      true,
		ExportMarkdown: true,
		Nodes: []TaskNode{
			{ID: "node-a", Title: "Reject invalid input", CanEditFiles: true, VerifySpec: specA, MaxRetries: 1},
			{ID: "node-b", Title: "Generate receipt", CanEditFiles: true, VerifySpec: specB, MaxRetries: 1},
		},
		Edges: []TaskEdge{{FromNodeID: "node-a", ToNodeID: "node-b"}},
	})
	if err != nil {
		t.Fatalf("CreateWorkItem failed: %v", err)
	}
	if detail.PlanMDPath == "" {
		t.Fatalf("expected exported markdown path")
	}
	if _, err := os.Stat(filepath.Join(ws, filepath.FromSlash(detail.PlanMDPath))); err != nil {
		t.Fatalf("markdown projection missing: %v", err)
	}

	// Only node-a is ready initially.
	ready, err := svc.ListReadyNodes(ctx, ws, detail.ID)
	if err != nil || len(ready) != 1 || ready[0].ID != "node-a" {
		t.Fatalf("expected only node-a ready, got err=%v ready=%+v", err, ready)
	}

	// Claim node-a.
	claimA, err := svc.ClaimNode(ctx, ws, detail.ID, "")
	if err != nil || claimA == nil || claimA.Node.ID != "node-a" || claimA.ExecutionToken == "" {
		t.Fatalf("ClaimNode failed: err=%v claim=%+v", err, claimA)
	}

	// Submitting with wrong token must be rejected.
	if _, err := svc.SubmitNodeResult(ctx, SubmitNodeResultInput{
		WorkspaceRoot:  ws,
		WorkItemID:     detail.ID,
		NodeID:         "node-a",
		ExecutionToken: "exec:forged",
		Status:         "pass",
	}); err == nil {
		t.Fatalf("expected forged token rejection")
	}

	// Submitting placeholder ref must be rejected.
	if _, err := svc.SubmitNodeResult(ctx, SubmitNodeResultInput{
		WorkspaceRoot:  ws,
		WorkItemID:     detail.ID,
		NodeID:         "node-a",
		ExecutionToken: claimA.ExecutionToken,
		Status:         "pass",
		EvidenceRefs:   []string{"receipt:placeholder"},
	}); err == nil {
		t.Fatalf("expected placeholder ref rejection")
	}

	// Real failing receipt -> retry within budget (maxRetries=1).
	failRec, err := svc.RecordReceipt(ctx, ws, ExecutionReceipt{
		WorkItemID: detail.ID,
		NodeID:     "node-a",
		Command:    "go",
		Args:       []string{"test", "./a"},
		Cwd:        ws,
		ExitCode:   1,
	})
	if err != nil {
		t.Fatalf("RecordReceipt failRec failed: %v", err)
	}
	retryRes, err := svc.SubmitNodeResult(ctx, SubmitNodeResultInput{
		WorkspaceRoot:  ws,
		WorkItemID:     detail.ID,
		NodeID:         "node-a",
		ExecutionToken: claimA.ExecutionToken,
		Status:         "pass",
		ReceiptID:      failRec.ID,
	})
	if err != nil || retryRes.Decision != DecisionRetry || retryRes.Node.RetryCount != 1 {
		t.Fatalf("expected retry decision, got err=%v res=%+v", err, retryRes)
	}

	// Re-claim node-a and submit real passing receipt -> unlocks node-b.
	claimA2, err := svc.ClaimNode(ctx, ws, detail.ID, "node-a")
	if err != nil || claimA2 == nil {
		t.Fatalf("re-claim node-a failed: %v", err)
	}
	passRecA, err := svc.RecordReceipt(ctx, ws, ExecutionReceipt{
		WorkItemID: detail.ID,
		NodeID:     "node-a",
		Command:    "go",
		Args:       []string{"test", "./a"},
		Cwd:        ws,
		ExitCode:   0,
	})
	if err != nil {
		t.Fatalf("RecordReceipt passRecA failed: %v", err)
	}
	advA, err := svc.SubmitNodeResult(ctx, SubmitNodeResultInput{
		WorkspaceRoot:  ws,
		WorkItemID:     detail.ID,
		NodeID:         "node-a",
		ExecutionToken: claimA2.ExecutionToken,
		Status:         "pass",
		ReceiptID:      passRecA.ID,
	})
	if err != nil || advA.Decision != DecisionContinue || len(advA.NextNodeIDs) != 1 || advA.NextNodeIDs[0] != "node-b" {
		t.Fatalf("expected continue -> node-b, got err=%v res=%+v", err, advA)
	}

	// DoneGate must still reject while node-b is pending.
	if _, err := svc.SetWorkItemStatus(ctx, ws, detail.ID, WorkDone, false); err == nil {
		t.Fatalf("expected DoneGate to block premature completion")
	}

	// Complete node-b -> entire WorkItem completes.
	claimB, err := svc.ClaimNode(ctx, ws, detail.ID, "")
	if err != nil || claimB == nil || claimB.Node.ID != "node-b" {
		t.Fatalf("claim node-b failed: err=%v claim=%+v", err, claimB)
	}
	passRecB, err := svc.RecordReceipt(ctx, ws, ExecutionReceipt{
		WorkItemID: detail.ID,
		NodeID:     "node-b",
		Command:    "go",
		Args:       []string{"test", "./b"},
		Cwd:        ws,
		ExitCode:   0,
	})
	if err != nil {
		t.Fatalf("RecordReceipt passRecB failed: %v", err)
	}
	advB, err := svc.SubmitNodeResult(ctx, SubmitNodeResultInput{
		WorkspaceRoot:  ws,
		WorkItemID:     detail.ID,
		NodeID:         "node-b",
		ExecutionToken: claimB.ExecutionToken,
		Status:         "pass",
		ReceiptID:      passRecB.ID,
	})
	if err != nil || advB.Decision != DecisionCompleted {
		t.Fatalf("expected completed, got err=%v res=%+v", err, advB)
	}

	gate, err := svc.EvaluateDoneGate(ctx, ws, detail.ID)
	if err != nil || !gate.OK || gate.NodesDone != 2 {
		t.Fatalf("expected passing DoneGate, got err=%v gate=%+v", err, gate)
	}
}
