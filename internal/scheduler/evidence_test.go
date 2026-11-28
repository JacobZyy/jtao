package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/JacobZyy/jtao/internal/runner"
	"github.com/JacobZyy/jtao/internal/workflow"
)

func TestSanitizeEvidenceRefsForSubmit(t *testing.T) {
	t.Run("filters free text when runner receipt exists", func(t *testing.T) {
		res := sanitizeEvidenceRefsForSubmit(
			[]string{"file:scripts/check-structure.mjs", "node scripts/check-structure.mjs：exit=0"},
			[]string{"receipt:1795773782375-c93ccb34984d0b5faeca010b"},
		)
		wantRefs := []string{
			"file:scripts/check-structure.mjs",
			"receipt:1795773782375-c93ccb34984d0b5faeca010b",
		}
		if !slices.Equal(res.Refs, wantRefs) {
			t.Fatalf("expected refs %v, got %v", wantRefs, res.Refs)
		}
		wantDropped := []string{"node scripts/check-structure.mjs：exit=0"}
		if !slices.Equal(res.DroppedFreeText, wantDropped) {
			t.Fatalf("expected dropped %v, got %v", wantDropped, res.DroppedFreeText)
		}
	})

	t.Run("preserves free text for rejection when no valid evidence exists", func(t *testing.T) {
		res := sanitizeEvidenceRefsForSubmit(
			[]string{"node scripts/check-structure.mjs：exit=0"},
			nil,
		)
		if !slices.Equal(res.Refs, []string{"node scripts/check-structure.mjs：exit=0"}) {
			t.Fatalf("expected unfiltered invalid ref, got %v", res.Refs)
		}
		if len(res.DroppedFreeText) != 0 {
			t.Fatalf("expected nothing dropped when no valid evidence exists, got %v", res.DroppedFreeText)
		}
	})

	t.Run("never filters placeholder refs even when real receipt exists", func(t *testing.T) {
		res := sanitizeEvidenceRefsForSubmit(
			[]string{"receipt:placeholder"},
			[]string{"receipt:1795773782375-c93ccb34984d0b5faeca010b"},
		)
		if !slices.Contains(res.Refs, "receipt:placeholder") {
			t.Fatalf("expected placeholder ref to be preserved for rejection, got %v", res.Refs)
		}
		if len(res.DroppedFreeText) != 0 {
			t.Fatalf("expected placeholder not to be dropped as free text, got %v", res.DroppedFreeText)
		}
	})
}

func TestSoloFiltersFreeTextEvidenceRefsWhenRealReceiptPresent(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()

	wf := workflow.NewService()
	defer wf.Close()
	_, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: ws,
		ID:            "solo-free-text-ref",
		Title:         "Solo free-text evidence ref tolerance",
		Nodes: []workflow.TaskNode{
			{
				ID:           "node-1",
				Title:        "Create marker and verify",
				CanEditFiles: true,
				MaxRetries:   1,
				VerifySpec:   &workflow.VerifyCommandSpec{Command: "test", Args: []string{"-f", "ok.txt"}, ExpectedExitCode: 0},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	mock := &mockRunner{
		turns: []func(context.Context, runner.RunTurnInput) (runner.RunTurnResult, error){
			func(_ context.Context, in runner.RunTurnInput) (runner.RunTurnResult, error) {
				if err := os.WriteFile(filepath.Join(in.ExecutionCwd, "ok.txt"), []byte("ok"), 0o644); err != nil {
					t.Fatal(err)
				}
				return runner.RunTurnResult{
					ExitCode: 0,
					ParsedJSON: map[string]any{
						"outcome":      "success",
						"summary":      "created ok.txt and verified structure",
						"evidenceRefs": []any{"node scripts/check-structure.mjs：exit=0", "file:ok.txt"},
						"selfReport":   map[string]any{"progressMade": true},
					},
				}, nil
			},
		},
	}

	solo := &SoloService{Workflow: wf}
	res, err := solo.Run(ctx, SoloRunInput{
		WorkspaceRoot:  ws,
		WorkItemID:     "solo-free-text-ref",
		MaxIterations:  3,
		RunnerOverride: mock,
	})
	if err != nil {
		t.Fatalf("Solo Run failed: %v", err)
	}
	if res.Run.Status != "done" || res.WorkItem.Status != workflow.WorkDone {
		t.Fatalf("expected done after filtering free-text ref with real receipt, got run=%s wi=%s note=%q",
			res.Run.Status, res.WorkItem.Status, res.FinalNote)
	}

	detail, err := wf.GetWorkItem(ctx, ws, "solo-free-text-ref")
	if err != nil || detail == nil || len(detail.Nodes) != 1 {
		t.Fatalf("GetWorkItem failed: err=%v detail=%+v", err, detail)
	}
	node := detail.Nodes[0]
	hasReceipt := false
	hasFile := false
	for _, ref := range node.EvidenceRefs {
		if strings.HasPrefix(ref, "receipt:") {
			hasReceipt = true
		}
		if ref == "file:ok.txt" {
			hasFile = true
		}
		if strings.Contains(ref, "check-structure.mjs") {
			t.Fatalf("expected invalid free-text ref to be excluded from persisted evidenceRefs, got %v", node.EvidenceRefs)
		}
	}
	if !hasReceipt || !hasFile {
		t.Fatalf("expected real receipt and file:ok.txt in persisted evidenceRefs, got %v", node.EvidenceRefs)
	}
	if node.Handoff == nil || len(node.Handoff.Findings) == 0 || !strings.Contains(node.Handoff.Findings[0], "node scripts/check-structure.mjs：exit=0") {
		t.Fatalf("expected dropped free-text ref to be recorded in handoff.Findings, got %+v", node.Handoff)
	}
}

func TestSoloRejectsInvalidOrPlaceholderEvidenceRefsWithoutValidEvidence(t *testing.T) {
	cases := []struct {
		name         string
		verifySpec   *workflow.VerifyCommandSpec
		evidenceRefs []any
	}{
		{
			name:         "no receipt and only free-text ref",
			evidenceRefs: []any{"node scripts/check-structure.mjs：exit=0"},
		},
		{
			name:         "no receipt and only placeholder ref",
			evidenceRefs: []any{"receipt:placeholder"},
		},
		{
			name:         "real receipt cannot mask placeholder ref",
			verifySpec:   &workflow.VerifyCommandSpec{Command: "test", Args: []string{"-f", "ok.txt"}, ExpectedExitCode: 0},
			evidenceRefs: []any{"receipt:placeholder"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ws := t.TempDir()

			wf := workflow.NewService()
			defer wf.Close()
			_, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
				WorkspaceRoot: ws,
				ID:            "solo-reject-invalid",
				Title:         "Reject invalid refs",
				Nodes: []workflow.TaskNode{
					{
						ID:           "node-1",
						Title:        "Node",
						CanEditFiles: true,
						VerifySpec:   tc.verifySpec,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}

			mock := &mockRunner{
				turns: []func(context.Context, runner.RunTurnInput) (runner.RunTurnResult, error){
					func(_ context.Context, in runner.RunTurnInput) (runner.RunTurnResult, error) {
						if tc.verifySpec != nil {
							_ = os.WriteFile(filepath.Join(in.ExecutionCwd, "ok.txt"), []byte("ok"), 0o644)
						}
						return runner.RunTurnResult{
							ExitCode: 0,
							ParsedJSON: map[string]any{
								"outcome":      "success",
								"summary":      "done",
								"evidenceRefs": tc.evidenceRefs,
								"selfReport":   map[string]any{"progressMade": true},
							},
						}, nil
					},
				},
			}

			solo := &SoloService{Workflow: wf}
			res, err := solo.Run(ctx, SoloRunInput{
				WorkspaceRoot:  ws,
				WorkItemID:     "solo-reject-invalid",
				MaxIterations:  3,
				RunnerOverride: mock,
			})
			if err != nil {
				t.Fatalf("Solo Run returned unexpected err: %v", err)
			}
			if res.Run.Status != "blocked" {
				t.Fatalf("expected run blocked on invalid/placeholder refs, got status=%s note=%q", res.Run.Status, res.FinalNote)
			}
			if !strings.Contains(res.FinalNote, "invalid or placeholder evidence ref") {
				t.Fatalf("expected rejection detail in FinalNote, got %q", res.FinalNote)
			}
		})
	}
}

func TestTeamFiltersFreeTextWithReceiptAndRejectsInvalidWithoutValidEvidence(t *testing.T) {
	ctx := context.Background()
	wf := workflow.NewService()
	defer wf.Close()

	// 1. With real receipt, Team filters non-scheme free text and completes cleanly.
	wsOK := t.TempDir()
	_, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: wsOK,
		ID:            "team-free-text-ok",
		Title:         "Team free-text tolerance",
		Nodes: []workflow.TaskNode{
			{
				ID:           "impl",
				Title:        "Implement with verifySpec",
				Role:         "implementer",
				CanEditFiles: true,
				VerifySpec:   &workflow.VerifyCommandSpec{Command: "test", Args: []string{"-f", "done.ok"}, ExpectedExitCode: 0},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	team := &TeamService{Workflow: wf}
	resOK, err := team.Run(ctx, TeamRunInput{
		WorkspaceRoot: wsOK,
		WorkItemID:    "team-free-text-ok",
		RunnerOverride: parallelTeamRunner{
			handler: func(_ context.Context, in runner.RunTurnInput) (runner.RunTurnResult, error) {
				if err := os.WriteFile(filepath.Join(in.ExecutionCwd, "done.ok"), []byte("ok"), 0o644); err != nil {
					t.Fatal(err)
				}
				return runner.RunTurnResult{
					ExitCode: 0,
					ParsedJSON: map[string]any{
						"status":       "pass",
						"summary":      "implemented",
						"filesTouched": []string{"done.ok"},
						"evidenceRefs": []any{"node scripts/check-structure.mjs：exit=0"},
					},
				}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("Team Run failed: %v", err)
	}
	if !resOK.OK || resOK.WorkItem.Status != workflow.WorkDone {
		t.Fatalf("expected team run done, got ok=%v status=%s note=%q", resOK.OK, resOK.WorkItem.Status, resOK.FinalNote)
	}
	if len(resOK.NodeRuns) != 1 || resOK.NodeRuns[0].Handoff == nil || len(resOK.NodeRuns[0].Handoff.Findings) == 0 {
		t.Fatalf("expected dropped ref finding in team handoff, got %+v", resOK.NodeRuns)
	}

	// 2. Without real receipt and with only invalid free text, Team strictly rejects and blocks.
	wsReject := t.TempDir()
	_, err = wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: wsReject,
		ID:            "team-invalid-reject",
		Title:         "Team reject invalid evidence ref",
		Nodes: []workflow.TaskNode{
			{
				ID:           "plan",
				Title:        "Plan node",
				Role:         "planner",
				CanEditFiles: false,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resReject, err := team.Run(ctx, TeamRunInput{
		WorkspaceRoot: wsReject,
		WorkItemID:    "team-invalid-reject",
		RunnerOverride: parallelTeamRunner{
			handler: func(_ context.Context, _ runner.RunTurnInput) (runner.RunTurnResult, error) {
				return runner.RunTurnResult{
					ExitCode: 0,
					ParsedJSON: map[string]any{
						"status":       "pass",
						"summary":      "plan done",
						"evidenceRefs": []any{"node scripts/check-structure.mjs：exit=0"},
					},
				}, nil
			},
		},
	})
	if err != nil {
		t.Fatalf("Team Run returned unexpected err: %v", err)
	}
	if resReject.OK || len(resReject.NodeRuns) != 1 || resReject.NodeRuns[0].Status != "blocked" {
		t.Fatalf("expected team node blocked on invalid-only evidenceRefs, got ok=%v runs=%+v", resReject.OK, resReject.NodeRuns)
	}
	if !strings.Contains(resReject.NodeRuns[0].Reason, "invalid or placeholder evidence ref") {
		t.Fatalf("expected invalid ref rejection reason, got %q", resReject.NodeRuns[0].Reason)
	}
}

func TestPromptsAndSchemasEnforceSchemeValueEvidenceRefsContract(t *testing.T) {
	node := workflow.TaskNode{
		ID:         "n1",
		Title:      "Check structure",
		Role:       "implementer",
		VerifySpec: &workflow.VerifyCommandSpec{Command: "node", Args: []string{"scripts/check-structure.mjs"}, ExpectedExitCode: 0},
	}

	prompts := map[string]string{
		"solo": buildSoloTurnPrompt(node, 1, "", false),
		"team": buildTeamNodePrompt(node, nil),
	}
	for name, prompt := range prompts {
		for _, mustContain := range []string{"scheme:value", "file:path/to/file", "receipt:<id>", "verifySpec", "："} {
			if !strings.Contains(prompt, mustContain) {
				t.Errorf("%s prompt missing %q constraint:\n%s", name, mustContain, prompt)
			}
		}
	}

	schemas := map[string][]byte{
		"soloTurnOutputSchema": soloTurnOutputSchema,
		"teamNodeOutputSchema": teamNodeOutputSchema,
	}
	for name, raw := range schemas {
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("%s invalid JSON: %v", name, err)
		}
		props, _ := parsed["properties"].(map[string]any)
		ev, _ := props["evidenceRefs"].(map[string]any)
		desc, _ := ev["description"].(string)
		for _, mustContain := range []string{"scheme:value", "file:path/to/file", "receipt:<id>", "verifySpec"} {
			if !strings.Contains(desc, mustContain) {
				t.Errorf("%s evidenceRefs description missing %q: %q", name, mustContain, desc)
			}
		}
		items, _ := ev["items"].(map[string]any)
		if _, hasPattern := items["pattern"]; hasPattern {
			t.Errorf("%s evidenceRefs.items must not use unsupported strict-mode keyword 'pattern'", name)
		}
		itemDesc, _ := items["description"].(string)
		if !strings.Contains(itemDesc, "scheme:value") {
			t.Errorf("%s evidenceRefs.items description missing 'scheme:value': %q", name, itemDesc)
		}
	}
}
