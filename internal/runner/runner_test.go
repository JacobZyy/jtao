package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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

func TestReceiptExecutorResolvesCargoFromHomeFallbackAndPreservesPathPriority(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	fakeHome := t.TempDir()
	cargoDir := filepath.Join(fakeHome, ".cargo", "bin")
	fnmDir := filepath.Join(fakeHome, ".fnm", "current", "bin")
	for _, dir := range []string{cargoDir, fnmDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fakeCargo := filepath.Join(cargoDir, "cargo")
	if err := os.WriteFile(fakeCargo, []byte("#!/bin/sh\necho cargo-from-home\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", fakeHome)
	t.Setenv("PATH", "/usr/bin:/bin")

	var captured ReceiptRecord
	exec := ReceiptExecutor{
		Sink: func(_ context.Context, _ string, rec ReceiptRecord) (string, error) {
			captured = rec
			return "receipt:cargo-home", nil
		},
	}

	out, err := exec.RunAndRecord(ctx, ReceiptExecInput{
		WorkspaceRoot: ws,
		ExecutionCwd:  ws,
		WorkItemID:    "wi-cargo",
		NodeID:        "n1",
		Command:       "cargo",
		Args:          []string{"--version"},
	})
	if err != nil {
		t.Fatalf("RunAndRecord failed: %v", err)
	}
	if out.Record.ExitCode != 0 || captured.ExecError != "" || !strings.Contains(captured.OutputExcerpt, "cargo-from-home") {
		t.Fatalf("expected cargo resolved from $HOME/.cargo/bin with exit 0, got rec=%+v", captured)
	}

	// Original PATH priority must win over fallback dirs and stay deduplicated.
	customDir := t.TempDir()
	customCargo := filepath.Join(customDir, "cargo")
	if err := os.WriteFile(customCargo, []byte("#!/bin/sh\necho cargo-from-custom\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	outCustom, err := exec.RunAndRecord(ctx, ReceiptExecInput{
		WorkspaceRoot: ws,
		ExecutionCwd:  ws,
		Command:       "cargo",
		Env:           []string{"HOME=" + fakeHome, "PATH=" + customDir + ":/usr/bin:/bin:" + cargoDir},
	})
	if err != nil {
		t.Fatalf("RunAndRecord custom PATH failed: %v", err)
	}
	if outCustom.Record.ExitCode != 0 || !strings.Contains(outCustom.Record.OutputExcerpt, "cargo-from-custom") {
		t.Fatalf("expected existing PATH entry to take precedence over fallback, got %+v", outCustom.Record)
	}

	aug := BuildAugmentedEnv([]string{"HOME=" + fakeHome, "PATH=/usr/bin:/bin:" + cargoDir}, "")
	pathVal := envLookup(aug, "PATH")
	parts := filepath.SplitList(pathVal)
	if len(parts) < 4 || parts[0] != "/usr/bin" || parts[1] != "/bin" || parts[2] != cargoDir {
		t.Fatalf("expected original PATH prefix preserved, got %v", parts)
	}
	cargoCount := 0
	for _, p := range parts {
		if p == cargoDir {
			cargoCount++
		}
	}
	if cargoCount != 1 {
		t.Fatalf("expected %s deduplicated in PATH, got %v", cargoDir, parts)
	}
	if !slices.Contains(parts, fnmDir) {
		t.Fatalf("expected %s appended to PATH, got %v", fnmDir, parts)
	}
}

func TestReceiptExecutorRecordsStartupErrors(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("PATH", "/usr/bin:/bin")

	var captured ReceiptRecord
	exec := ReceiptExecutor{
		Sink: func(_ context.Context, _ string, rec ReceiptRecord) (string, error) {
			captured = rec
			return "receipt:err-1", nil
		},
	}

	// 1. Command not found in PATH.
	outMissing, err := exec.RunAndRecord(ctx, ReceiptExecInput{
		WorkspaceRoot: ws,
		ExecutionCwd:  ws,
		Command:       "jtao-definitely-missing-cmd",
		Args:          []string{"test"},
	})
	if err != nil {
		t.Fatalf("RunAndRecord should persist failed receipt without error, got %v", err)
	}
	if outMissing.Record.ExitCode != -1 || !outMissing.Result.StartFailed {
		t.Fatalf("expected exitCode=-1 and StartFailed=true, got rec=%+v res=%+v", outMissing.Record, outMissing.Result)
	}
	if !strings.Contains(captured.ExecError, "not found") || !strings.Contains(captured.ExecError, "jtao-definitely-missing-cmd") {
		t.Fatalf("expected ExecError to report command not found, got %q", captured.ExecError)
	}
	if !strings.Contains(captured.OutputExcerpt, "not found") || captured.StderrSHA256 == hashText("") {
		t.Fatalf("expected non-empty error excerpt and stderr hash, got excerpt=%q sha=%q", captured.OutputExcerpt, captured.StderrSHA256)
	}

	// 2. Nonexistent working directory.
	missingCwd := filepath.Join(ws, "does-not-exist")
	outCwd, err := exec.RunAndRecord(ctx, ReceiptExecInput{
		WorkspaceRoot: ws,
		ExecutionCwd:  missingCwd,
		Command:       "sh",
		Args:          []string{"-c", "echo hi"},
	})
	if err != nil {
		t.Fatalf("RunAndRecord missing cwd failed: %v", err)
	}
	if outCwd.Record.ExitCode != -1 || !strings.Contains(outCwd.Record.ExecError, "working directory") {
		t.Fatalf("expected working directory error, got %+v", outCwd.Record)
	}

	// 3. Non-executable file (permission denied).
	noExec := filepath.Join(ws, "not-exec.sh")
	if err := os.WriteFile(noExec, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outPerm, err := exec.RunAndRecord(ctx, ReceiptExecInput{
		WorkspaceRoot: ws,
		ExecutionCwd:  ws,
		Command:       noExec,
	})
	if err != nil {
		t.Fatalf("RunAndRecord permission denied failed: %v", err)
	}
	if outPerm.Record.ExitCode != -1 || !strings.Contains(outPerm.Record.ExecError, "permission denied") {
		t.Fatalf("expected permission denied error, got %+v", outPerm.Record)
	}
}

func TestCodexRunnerUsesSandboxFlagAndIgnoresErrorJSONOnNonZeroExit(t *testing.T) {
	binDir := t.TempDir()
	fakeCodex := filepath.Join(binDir, "codex")
	script := `#!/bin/sh
out=""
prev=""
for arg in "$@"; do
  if [ "$arg" = "--full-auto" ]; then
    echo "error: unexpected argument '--full-auto' found" >&2
    exit 2
  fi
  if [ "$prev" = "--output-last-message" ]; then
    out="$arg"
  fi
  prev="$arg"
done
if [ "${FAKE_CODEX_FAIL:-0}" = "1" ]; then
  echo 'ERROR: {"error":{"message":"invalid_json_schema"},"status":400}' >&2
  exit 1
fi
printf '{"outcome":"success","summary":"ok","selfReport":{"progressMade":true}}' > "$out"
`
	if err := os.WriteFile(fakeCodex, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ws := t.TempDir()

	res, err := (CodexRunner{}).RunTurn(context.Background(), RunTurnInput{
		WorkspaceRoot: ws,
		ExecutionCwd:  ws,
		UserPrompt:    "hello",
		OutputSchema:  []byte(`{"type":"object"}`),
	})
	if err != nil {
		t.Fatalf("RunTurn returned error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d (raw=%q)", res.ExitCode, res.RawOutput)
	}
	if slices.Contains(res.Args, "--full-auto") {
		t.Fatalf("did not expect deprecated --full-auto in args: %v", res.Args)
	}
	sandboxIdx := slices.Index(res.Args, "--sandbox")
	if sandboxIdx < 0 || sandboxIdx+1 >= len(res.Args) || res.Args[sandboxIdx+1] != "workspace-write" {
		t.Fatalf("expected --sandbox workspace-write in args: %v", res.Args)
	}
	if res.ParsedJSON == nil || res.ParsedJSON["outcome"] != "success" {
		t.Fatalf("expected parsed JSON from --output-last-message, got %+v", res.ParsedJSON)
	}

	failRes, err := (CodexRunner{}).RunTurn(context.Background(), RunTurnInput{
		WorkspaceRoot: ws,
		ExecutionCwd:  ws,
		UserPrompt:    "hello",
		Env:           append(os.Environ(), "FAKE_CODEX_FAIL=1"),
	})
	if err != nil {
		t.Fatalf("RunTurn failure path returned error: %v", err)
	}
	if failRes.ExitCode != 1 {
		t.Fatalf("expected exit 1, got %d", failRes.ExitCode)
	}
	if failRes.ParsedJSON != nil {
		t.Fatalf("non-zero exit must not parse stderr error JSON as turn result: %+v", failRes.ParsedJSON)
	}
	if !strings.Contains(failRes.RawOutput, "invalid_json_schema") {
		t.Fatalf("expected stderr detail preserved in RawOutput, got %q", failRes.RawOutput)
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
