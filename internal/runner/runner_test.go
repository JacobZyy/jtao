package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestExecProcessGroupTimeoutKillsSubtreeCleanly(t *testing.T) {
	ctx := context.Background()
	// Spawn a shell that spawns a sleeping child; timeout must terminate the entire process group.
	res := ExecProcessGroup(ctx, "sh", []string{"-c", "sleep 30 & wait"}, t.TempDir(), nil, 250*time.Millisecond)
	if !res.TimedOut {
		t.Fatalf("expected TimedOut=true, got %+v", res)
	}
	if res.TreeAlive {
		t.Fatalf("expected TreeAlive=false after process group kill, got %+v", res)
	}
}

func TestReceiptExecutorRunsAndRecords(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()

	var captured ReceiptRecord
	exec := ReceiptExecutor{
		Sink: func(_ context.Context, _ string, rec ReceiptRecord) (string, error) {
			captured = rec
			return "receipt:test-1", nil
		},
	}

	out, err := exec.RunAndRecord(ctx, ReceiptExecInput{
		WorkspaceRoot: ws,
		ExecutionCwd:  ws,
		WorkItemID:    "wi-1",
		NodeID:        "node-a",
		Command:       "sh",
		Args:          []string{"-c", "echo verified-ok"},
	})
	if err != nil {
		t.Fatalf("RunAndRecord failed: %v", err)
	}
	if out.ReceiptID != "receipt:test-1" || captured.ExitCode != 0 || captured.StdoutSHA256 == "" {
		t.Fatalf("unexpected receipt output: %+v captured=%+v", out, captured)
	}
}

func TestWorktreeLifecyclePreserveAndCleanup(t *testing.T) {
	repo := initTempGitRepo(t)
	mgr := WorktreeManager{}

	// 1. Clean worktree with status=done should be automatically cleaned up.
	wtClean, err := mgr.Prepare(repo, "run-clean", "HEAD")
	if err != nil {
		t.Fatalf("Prepare clean failed: %v", err)
	}
	finalClean, err := mgr.Finalize(repo, wtClean, "done", true, false)
	if err != nil {
		t.Fatalf("Finalize clean failed: %v", err)
	}
	if finalClean.Preserved || finalClean.Path != "" {
		t.Fatalf("expected clean worktree removed, got %+v", finalClean)
	}

	// 2. Dirty worktree must be preserved for inspection/resume.
	wtDirty, err := mgr.Prepare(repo, "run-dirty", "HEAD")
	if err != nil {
		t.Fatalf("Prepare dirty failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtDirty.Path, "change.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write change failed: %v", err)
	}
	changes, err := mgr.DetectChanges(wtDirty)
	if err != nil || !changes.Changed || len(changes.Files) != 1 {
		t.Fatalf("expected detected change, got err=%v changes=%+v", err, changes)
	}
	finalDirty, err := mgr.Finalize(repo, wtDirty, "done", true, false)
	if err != nil {
		t.Fatalf("Finalize dirty failed: %v", err)
	}
	if !finalDirty.Preserved || finalDirty.Path == "" {
		t.Fatalf("expected dirty worktree preserved, got %+v", finalDirty)
	}
	// Clean up temp worktree after asserting preservation.
	_, _ = mgr.Finalize(repo, finalDirty, "done", true, true)
}

func TestExtractJSONObject(t *testing.T) {
	fenced := "Here is my report:\n```json\n{\"outcome\":\"success\",\"summary\":\"done\"}\n```\n"
	obj := ExtractJSONObject(fenced)
	if obj == nil || obj["outcome"] != "success" {
		t.Fatalf("expected parsed JSON from fenced block, got %+v", obj)
	}
}

func TestResolveBinaryAndAugmentedEnvFallback(t *testing.T) {
	fakeHome := t.TempDir()
	binDir := filepath.Join(fakeHome, ".vite-plus", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(binDir, "jtao-fake-cli")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", fakeHome)
	t.Setenv("PATH", "/usr/bin:/bin")

	resolved, err := ResolveBinary("jtao-fake-cli")
	if err != nil {
		t.Fatalf("expected fallback resolution in ~/.vite-plus/bin, got err=%v", err)
	}
	if resolved != fakeBin {
		t.Fatalf("expected %s, got %s", fakeBin, resolved)
	}

	env := BuildAugmentedEnv([]string{"PATH=/usr/bin:/bin"}, binDir)
	found := false
	for _, kv := range env {
		if kv == "PATH=/usr/bin:/bin:"+binDir || (len(kv) > 5 && kv[:5] == "PATH=" && filepath.SplitList(kv[5:])[2] == binDir) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected augmented PATH to include %s, got %v", binDir, env)
	}
}

func initTempGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmds := [][]string{
		{"init"},
		{"config", "user.email", "test@jtao.local"},
		{"config", "user.name", "jtao-test"},
	}
	for _, args := range cmds {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v (%s)", args, err, string(out))
		}
	}
	return dir
}
