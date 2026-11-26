package hook

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JacobZyy/jtao/internal/memory"
	"github.com/JacobZyy/jtao/internal/workflow"
)

func TestUserPromptSubmitZeroHitSilenceAndHitInjection(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	mem := memory.NewService()
	defer mem.Close()
	wf := workflow.NewService()
	defer wf.Close()

	// 0 hits + no active WorkItem -> must return `{}` silently AND never create .jtao/ in untouched repos.
	out, err := HandleUserPromptSubmit(ctx, mem, wf, ws, []byte(`{"prompt":"hello unrelated xyz","client":"codex"}`))
	if err != nil {
		t.Fatalf("HandleUserPromptSubmit zero-hit failed: %v", err)
	}
	if strings.TrimSpace(string(out)) != "{}" {
		t.Fatalf("expected silent {}, got %s", string(out))
	}
	if _, err := os.Stat(filepath.Join(ws, ".jtao")); !os.IsNotExist(err) {
		t.Fatalf("global hook must not pollute uninitialized workspace with .jtao/")
	}

	// Write a matching memo -> should inject recalled block without unconsumed <!--memory:--> tag.
	_, err = mem.WriteMemo(ctx, memory.WriteMemoInput{
		WorkspaceRoot: ws,
		Text:          "数据库迁移必须通过 workflow.db 的单事务完成",
	})
	if err != nil {
		t.Fatal(err)
	}

	outHit, err := HandleUserPromptSubmit(ctx, mem, wf, ws, []byte(`{"prompt":"数据库迁移 workflow.db","client":"codex"}`))
	if err != nil {
		t.Fatalf("HandleUserPromptSubmit hit failed: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(outHit, &parsed); err != nil {
		t.Fatalf("invalid hook JSON: %v (%s)", err, string(outHit))
	}
	ctxText, _ := parsed["additionalContext"].(string)
	if !strings.Contains(ctxText, "数据库迁移") {
		t.Fatalf("expected recalled memo in additionalContext, got %q", ctxText)
	}
	if strings.Contains(ctxText, "<!--memory:") {
		t.Fatalf("must not emit unconsumed <!--memory:--> instruction for native chat: %q", ctxText)
	}

	// Active WorkItem must NOT hide workspace-wide memos during UserPromptSubmit recall.
	if _, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: ws,
		ID:            "active-wi",
		Title:         "Active work",
		SetActive:     true,
	}); err != nil {
		t.Fatal(err)
	}
	outBoth, err := HandleUserPromptSubmit(ctx, mem, wf, ws, []byte(`{"prompt":"数据库迁移 workflow.db","client":"codex"}`))
	if err != nil {
		t.Fatalf("HandleUserPromptSubmit with active WorkItem failed: %v", err)
	}
	if !strings.Contains(string(outBoth), "active-wi") || !strings.Contains(string(outBoth), "数据库迁移") {
		t.Fatalf("expected both active WorkItem and general memo recalled, got %s", string(outBoth))
	}
}

func TestSessionStartPinInjectionAndPostToolOffload(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	mem := memory.NewService()
	defer mem.Close()
	wf := workflow.NewService()
	defer wf.Close()

	if _, err := mem.PinAppend(ctx, ws, "default", "- Never skip go test."); err != nil {
		t.Fatal(err)
	}
	startOut, err := HandleSessionStart(ctx, mem, wf, ws, []byte(`{}`))
	if err != nil || !strings.Contains(string(startOut), "Never skip go test.") {
		t.Fatalf("expected pinned rule in SessionStart output, got err=%v out=%s", err, string(startOut))
	}

	big, _ := json.Marshal(map[string]string{
		"tool_name":   "bash",
		"tool_output": strings.Repeat("output line\n", 300),
	})
	offOut, err := HandlePostToolUse(ctx, mem, ws, big)
	if err != nil || !strings.Contains(string(offOut), `"offloaded":true`) {
		t.Fatalf("expected post-tool offload, got err=%v out=%s", err, string(offOut))
	}
}

func TestHookSurfacesBackgroundRunStatus(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	mem := memory.NewService()
	defer mem.Close()
	wf := workflow.NewService()
	defer wf.Close()

	if _, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: ws,
		ID:            "wi-bg",
		Title:         "Background feature",
		SetActive:     true,
		Nodes:         []workflow.TaskNode{{ID: "n1", Title: "Build"}},
	}); err != nil {
		t.Fatal(err)
	}
	db, err := wf.DB(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Insert a running row with no lease -> should be surfaced as orphaned so operator can resume.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO runs (
			id, work_item_id, mode, provider, status, current_iteration, max_iterations,
			worktree_json, pacing_json, backoff_json, stop_requested, stop_reason, summary_text,
			created_at, updated_at
		) VALUES (?, ?, 'solo', 'codex', 'running', 2, 10, 'null', '{}', '{}', 0, '', 'running step 2', ?, ?)
	`, "run:solo:orphan", "wi-bg", now, now); err != nil {
		t.Fatal(err)
	}

	outStart, err := HandleSessionStart(ctx, mem, wf, ws, []byte(`{}`))
	if err != nil || !strings.Contains(string(outStart), "Background jtao Run (orphaned)") || !strings.Contains(string(outStart), "run:solo:orphan") {
		t.Fatalf("expected orphaned background run surfaced on SessionStart, got err=%v out=%s", err, string(outStart))
	}

	// Mark run as recently done -> surfaced on both SessionStart and UserPromptSubmit.
	if _, err := db.ExecContext(ctx, `UPDATE runs SET status = 'done', summary_text = 'all 3 nodes passed', updated_at = ? WHERE id = ?`, now, "run:solo:orphan"); err != nil {
		t.Fatal(err)
	}
	outPrompt, err := HandleUserPromptSubmit(ctx, mem, wf, ws, []byte(`{"prompt":"检查一下状态","client":"codex"}`))
	if err != nil || !strings.Contains(string(outPrompt), "Background jtao Run (done)") || !strings.Contains(string(outPrompt), "all 3 nodes passed") {
		t.Fatalf("expected done background run surfaced on UserPromptSubmit, got err=%v out=%s", err, string(outPrompt))
	}
}
