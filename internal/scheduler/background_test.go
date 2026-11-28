package scheduler

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/JacobZyy/jtao/internal/workflow"
)

func TestInspectRunAndLiveProcessDetection(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	wf := workflow.NewService()
	defer wf.Close()

	if _, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: ws,
		ID:            "wi-inspect",
		Title:         "Inspect test",
		SetActive:     true,
		Nodes:         []workflow.TaskNode{{ID: "n1", Title: "Do work"}},
	}); err != nil {
		t.Fatal(err)
	}

	db, err := wf.DB(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	run := RunRecord{
		ID:            "run:solo:test-inspect",
		WorkItemID:    "wi-inspect",
		Mode:          "solo",
		Provider:      "codex",
		Status:        "running",
		MaxIterations: 10,
		SummaryText:   "iter 1 running",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := insertRun(ctx, db, run); err != nil {
		t.Fatal(err)
	}

	wfDir, err := workflow.ResolveWorkflowDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	logPath := RunLogPath(wfDir, run.ID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("line 1\nline 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Without a lease, a "running" row must report LiveProcess=false (crashed/orphaned detection).
	snapNoLease, err := InspectRun(ctx, wf, ws, "", "")
	if err != nil || snapNoLease == nil {
		t.Fatalf("InspectRun failed: err=%v snap=%+v", err, snapNoLease)
	}
	if snapNoLease.LiveProcess {
		t.Fatalf("expected LiveProcess=false when no lease exists")
	}
	if !strings.Contains(snapNoLease.LogTail, "line 2") || snapNoLease.LogPath != logPath {
		t.Fatalf("expected log tail and path, got path=%q tail=%q", snapNoLease.LogPath, snapNoLease.LogTail)
	}

	// 2. With a live lease held by current PID, InspectRun reports LiveProcess=true.
	lease, err := AcquireLease(ctx, db, "workitem:wi-inspect", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	snapLive, err := InspectRun(ctx, wf, ws, run.ID, "")
	if err != nil || snapLive == nil || !snapLive.LiveProcess {
		t.Fatalf("expected LiveProcess=true with active lease, got err=%v snap=%+v", err, snapLive)
	}

	// 3. ensureNoLiveLease blocks duplicate background launches on the same WorkItem.
	if err := ensureNoLiveLease(ctx, wf, ws, "wi-inspect"); err == nil {
		t.Fatalf("expected ensureNoLiveLease to reject duplicate active run")
	}
}

func TestRequestStopMarksRunAndSignalsDetachedWorker(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	wf := workflow.NewService()
	defer wf.Close()

	if _, err := wf.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
		WorkspaceRoot: ws,
		ID:            "wi-stop",
		Title:         "Stop test",
		SetActive:     true,
		Nodes:         []workflow.TaskNode{{ID: "n1", Title: "Sleep"}},
	}); err != nil {
		t.Fatal(err)
	}

	// Spawn a detached dummy sleeper in its own session to verify SIGTERM stops it cleanly.
	sleeper := exec.Command("sleep", "30")
	sleeper.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- sleeper.Wait() }()
	defer func() { _ = syscall.Kill(-sleeper.Process.Pid, syscall.SIGKILL) }()

	db, err := wf.DB(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	runID := "run:team:stop-test"
	if err := insertRun(ctx, db, RunRecord{
		ID:            runID,
		WorkItemID:    "wi-stop",
		Mode:          "team",
		Provider:      "codex",
		Status:        "running",
		MaxIterations: 3,
		CreatedAt:     now.Format(time.RFC3339Nano),
		UpdatedAt:     now.Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO run_leases (lease_key, run_id, owner_pid, hostname, acquired_at, heartbeat_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "workitem:wi-stop", runID, sleeper.Process.Pid, host,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Add(2*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	res, err := RequestStop(ctx, wf, ws, runID, "", "user-cancelled", true)
	if err != nil {
		t.Fatalf("RequestStop failed: %v", err)
	}
	if !res.Run.StopRequested || res.SignaledPID != sleeper.Process.Pid {
		t.Fatalf("expected stop requested & signaled pid=%d, got %+v", sleeper.Process.Pid, res)
	}

	select {
	case <-waitCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected detached sleeper pid=%d to exit after SIGTERM", sleeper.Process.Pid)
	}
}

func TestSpawnDetachedRunSurfacesImmediateFailure(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	wf := workflow.NewService()
	defer wf.Close()

	// Point BinaryPath at a tiny shell script that writes an error and exits 1.
	badBin := filepath.Join(t.TempDir(), "bad-jtao.sh")
	if err := os.WriteFile(badBin, []byte("#!/bin/sh\necho 'simulated startup boom' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := spawnDetachedRun(ctx, wf, ws, badBin, "run:solo:fail-fast", "wi-x", "solo", "codex", nil, false)
	if err == nil || !strings.Contains(err.Error(), "simulated startup boom") {
		t.Fatalf("expected immediate failure surfaced with log excerpt, got err=%v", err)
	}
}

func TestSpawnDetachedRunPreservesHostPathAndAppendsHomeCargo(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	wf := workflow.NewService()
	defer wf.Close()

	fakeHome := t.TempDir()
	cargoDir := filepath.Join(fakeHome, ".cargo", "bin")
	if err := os.MkdirAll(cargoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hostDir := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("PATH", hostDir+":/usr/bin:/bin")

	outFile := filepath.Join(t.TempDir(), "worker-path.txt")
	probeBin := filepath.Join(t.TempDir(), "probe-jtao.sh")
	script := "#!/bin/sh\nprintf '%s' \"$PATH\" > " + outFile + "\n"
	if err := os.WriteFile(probeBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := spawnDetachedRun(ctx, wf, ws, probeBin, "run:solo:path-probe", "wi-path", "solo", "codex", nil, false); err != nil {
		t.Fatalf("spawnDetachedRun failed: %v", err)
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read worker PATH output: %v", err)
	}
	parts := filepath.SplitList(string(raw))
	if len(parts) < 4 || parts[0] != hostDir || parts[1] != "/usr/bin" || parts[2] != "/bin" {
		t.Fatalf("expected host PATH order preserved in detached worker, got %v", parts)
	}
	hasCargo := false
	for _, p := range parts {
		if p == cargoDir {
			hasCargo = true
			break
		}
	}
	if !hasCargo {
		t.Fatalf("expected detached worker PATH to include %s, got %v", cargoDir, parts)
	}
}
