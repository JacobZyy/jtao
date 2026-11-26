package scheduler

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/JacobZyy/jtao/internal/runner"
	"github.com/JacobZyy/jtao/internal/workflow"
)

const (
	backgroundStartVerifyTimeout = 3 * time.Second
	backgroundStartPollInterval  = 25 * time.Millisecond
)

// BackgroundSoloInput configures an asynchronous detached Solo loop launch.
type BackgroundSoloInput struct {
	WorkspaceRoot  string
	WorkItemID     string
	Provider       string
	UseWorktree    bool
	BaseRef        string
	MaxIterations  int
	TimeoutSec     int
	Unattended     bool
	Quota          int
	QuotaWindowMin int
	CadenceBaseSec int
	CadenceMaxSec  int
	QuietThreshold int
	SafeBypass     *bool
	BinaryPath     string
}

// BackgroundTeamInput configures an asynchronous detached Team DAG launch.
type BackgroundTeamInput struct {
	WorkspaceRoot string
	WorkItemID    string
	Provider      string
	Concurrency   int
	UseWorktree   bool
	BaseRef       string
	Snapshot      bool
	TimeoutSec    int
	BinaryPath    string
}

// BackgroundResumeInput configures an asynchronous resume of a Solo or Team run.
type BackgroundResumeInput struct {
	WorkspaceRoot string
	RunID         string
	WorkItemID    string
	Provider      string
	MaxIterations int
	Unattended    bool
	Concurrency   int
	UseWorktree   bool
	Force         bool
	BinaryPath    string
}

// BackgroundStartResult reports the handle of a detached background run immediately after launch.
type BackgroundStartResult struct {
	RunID      string `json:"runId"`
	WorkItemID string `json:"workItemId"`
	Mode       string `json:"mode"` // solo | team
	Provider   string `json:"provider"`
	PID        int    `json:"pid"`
	Status     string `json:"status"`
	LogPath    string `json:"logPath"`
	Summary    string `json:"summary"`
}

// StartBackgroundSolo runs fast preflight checks and launches a detached `jtao solo run` process.
func StartBackgroundSolo(ctx context.Context, wf *workflow.SQLiteService, in BackgroundSoloInput) (BackgroundStartResult, error) {
	ws, wi, err := resolveTargetWorkItem(ctx, wf, in.WorkspaceRoot, in.WorkItemID)
	if err != nil {
		return BackgroundStartResult{}, err
	}
	provider := normalizeProvider(in.Provider)

	// Fast synchronous preflight (zero model calls).
	soloSvc := &SoloService{Workflow: wf}
	rep, err := soloSvc.DryRun(ctx, SoloRunInput{
		WorkspaceRoot: ws,
		WorkItemID:    wi.ID,
		Provider:      provider,
	})
	if err != nil {
		return BackgroundStartResult{}, err
	}
	if rep.Verdict == "blocked" {
		return BackgroundStartResult{}, fmt.Errorf("solo preflight blocked: %s", strings.Join(rep.Reasons, "; "))
	}
	if err := ensureNoLiveLease(ctx, wf, ws, wi.ID); err != nil {
		return BackgroundStartResult{}, err
	}

	runID := newRunID("run:solo")
	args := []string{
		"--workspace", ws,
		"--json",
		"solo", "run",
		"--work-item", wi.ID,
		"--run-id", runID,
		"--provider", provider,
	}
	if in.UseWorktree {
		args = append(args, "--worktree")
		if strings.TrimSpace(in.BaseRef) != "" {
			args = append(args, "--base-ref", strings.TrimSpace(in.BaseRef))
		}
	}
	if in.MaxIterations > 0 {
		args = append(args, "--max-iterations", strconv.Itoa(in.MaxIterations))
	}
	if in.TimeoutSec > 0 {
		args = append(args, "--timeout-sec", strconv.Itoa(in.TimeoutSec))
	}
	if in.Unattended {
		args = append(args, "--unattended")
	}
	if in.Quota > 0 {
		args = append(args, "--quota", strconv.Itoa(in.Quota))
	}
	if in.QuotaWindowMin > 0 {
		args = append(args, "--quota-window-min", strconv.Itoa(in.QuotaWindowMin))
	}
	if in.CadenceBaseSec > 0 {
		args = append(args, "--cadence-base-sec", strconv.Itoa(in.CadenceBaseSec))
	}
	if in.CadenceMaxSec > 0 {
		args = append(args, "--cadence-max-sec", strconv.Itoa(in.CadenceMaxSec))
	}
	if in.QuietThreshold > 0 {
		args = append(args, "--quiet-threshold", strconv.Itoa(in.QuietThreshold))
	}
	if in.SafeBypass != nil {
		args = append(args, fmt.Sprintf("--safe-bypass=%v", *in.SafeBypass))
	}

	return spawnDetachedRun(ctx, wf, ws, in.BinaryPath, runID, wi.ID, "solo", provider, args, false)
}

// StartBackgroundTeam validates DAG ownership and readiness, then launches a detached `jtao team run` process.
func StartBackgroundTeam(ctx context.Context, wf *workflow.SQLiteService, in BackgroundTeamInput) (BackgroundStartResult, error) {
	ws, wi, err := resolveTargetWorkItem(ctx, wf, in.WorkspaceRoot, in.WorkItemID)
	if err != nil {
		return BackgroundStartResult{}, err
	}
	provider := normalizeProvider(in.Provider)

	teamSvc := &TeamService{Workflow: wf}
	if _, err := teamSvc.Plan(ctx, TeamRunInput{
		WorkspaceRoot: ws,
		WorkItemID:    wi.ID,
		Concurrency:   in.Concurrency,
	}); err != nil {
		return BackgroundStartResult{}, fmt.Errorf("team preflight failed: %w", err)
	}
	ag, err := runner.ResolveRunner(provider)
	if err != nil {
		return BackgroundStartResult{}, err
	}
	if check := ag.CheckReadiness(); !check.OK {
		return BackgroundStartResult{}, fmt.Errorf("provider readiness failed: %s", check.Reason)
	}
	if err := ensureNoLiveLease(ctx, wf, ws, wi.ID); err != nil {
		return BackgroundStartResult{}, err
	}

	runID := newRunID("run:team")
	args := []string{
		"--workspace", ws,
		"--json",
		"team", "run",
		"--work-item", wi.ID,
		"--run-id", runID,
		"--provider", provider,
	}
	if in.Concurrency > 0 {
		args = append(args, "--concurrency", strconv.Itoa(in.Concurrency))
	}
	if in.UseWorktree {
		args = append(args, "--worktree")
		if strings.TrimSpace(in.BaseRef) != "" {
			args = append(args, "--base-ref", strings.TrimSpace(in.BaseRef))
		}
	}
	if in.Snapshot {
		args = append(args, "--snapshot")
	}
	if in.TimeoutSec > 0 {
		args = append(args, "--timeout-sec", strconv.Itoa(in.TimeoutSec))
	}

	return spawnDetachedRun(ctx, wf, ws, in.BinaryPath, runID, wi.ID, "team", provider, args, false)
}

// ResumeBackgroundRun resumes a stopped/blocked/crashed Solo run or retries blocked nodes of a Team run in background.
func ResumeBackgroundRun(ctx context.Context, wf *workflow.SQLiteService, in BackgroundResumeInput) (BackgroundStartResult, error) {
	ws, err := filepath.Abs(strings.TrimSpace(in.WorkspaceRoot))
	if err != nil {
		return BackgroundStartResult{}, fmt.Errorf("resolve workspace: %w", err)
	}
	prior, err := LatestRun(ctx, wf, ws, in.RunID, in.WorkItemID)
	if err != nil {
		return BackgroundStartResult{}, err
	}
	if prior == nil {
		return BackgroundStartResult{}, fmt.Errorf("no prior run found to resume")
	}
	if err := ensureNoLiveLease(ctx, wf, ws, prior.WorkItemID); err != nil {
		return BackgroundStartResult{}, err
	}

	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		provider = normalizeProvider(prior.Provider)
	}
	ag, err := runner.ResolveRunner(provider)
	if err != nil {
		return BackgroundStartResult{}, err
	}
	if check := ag.CheckReadiness(); !check.OK {
		return BackgroundStartResult{}, fmt.Errorf("provider readiness failed: %s", check.Reason)
	}

	if prior.Mode == "team" {
		// Dry-run retry-blocked first to catch empty blocked set or instability guard immediately.
		teamSvc := &TeamService{Workflow: wf}
		if _, err := teamSvc.RetryBlocked(ctx, TeamRetryInput{
			WorkspaceRoot: ws,
			RunID:         prior.ID,
			Provider:      provider,
			Concurrency:   in.Concurrency,
			UseWorktree:   in.UseWorktree,
			Force:         in.Force,
			DryRun:        true,
		}); err != nil {
			return BackgroundStartResult{}, err
		}

		nextRunID := newRunID("run:team")
		args := []string{
			"--workspace", ws,
			"--json",
			"team", "retry-blocked",
			"--run", prior.ID,
			"--next-run-id", nextRunID,
			"--provider", provider,
		}
		if in.Concurrency > 0 {
			args = append(args, "--concurrency", strconv.Itoa(in.Concurrency))
		}
		if in.UseWorktree {
			args = append(args, "--worktree")
		}
		if in.Force {
			args = append(args, "--force")
		}
		return spawnDetachedRun(ctx, wf, ws, in.BinaryPath, nextRunID, prior.WorkItemID, "team", provider, args, false)
	}

	args := []string{
		"--workspace", ws,
		"--json",
		"solo", "resume",
		"--run", prior.ID,
	}
	if in.MaxIterations > 0 {
		args = append(args, "--max-iterations", strconv.Itoa(in.MaxIterations))
	}
	if in.Unattended {
		args = append(args, "--unattended")
	}
	return spawnDetachedRun(ctx, wf, ws, in.BinaryPath, prior.ID, prior.WorkItemID, "solo", provider, args, true)
}

func spawnDetachedRun(
	ctx context.Context,
	wf *workflow.SQLiteService,
	workspaceRoot string,
	explicitBin string,
	runID string,
	workItemID string,
	mode string,
	provider string,
	args []string,
	expectExistingRow bool,
) (BackgroundStartResult, error) {
	binPath, err := resolveJtaoExecutable(explicitBin)
	if err != nil {
		return BackgroundStartResult{}, err
	}
	wfDir, err := workflow.ResolveWorkflowDir(workspaceRoot)
	if err != nil {
		return BackgroundStartResult{}, err
	}
	logPath := RunLogPath(wfDir, runID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return BackgroundStartResult{}, fmt.Errorf("create workflow log directory: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return BackgroundStartResult{}, fmt.Errorf("open background log file: %w", err)
	}

	cmd := exec.Command(binPath, args...)
	cmd.Dir = workspaceRoot
	cmd.Env = runner.BuildAugmentedEnv(os.Environ(), filepath.Dir(binPath))
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return BackgroundStartResult{}, fmt.Errorf("spawn detached jtao worker: %w", err)
	}
	pid := cmd.Process.Pid
	_ = logFile.Close()

	// Always reap the child in a background goroutine so long-lived MCP servers never leak zombies.
	exitCh := make(chan error, 1)
	go func() {
		exitCh <- cmd.Wait()
	}()

	db, err := wf.DB(ctx, workspaceRoot)
	if err != nil {
		return BackgroundStartResult{}, err
	}

	deadline := time.NewTimer(backgroundStartVerifyTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(backgroundStartPollInterval)
	defer ticker.Stop()

	for {
		select {
		case waitErr := <-exitCh:
			rec, _ := loadRun(ctx, db, runID)
			if waitErr != nil {
				tail, _ := readTailText(logPath, 1024)
				return BackgroundStartResult{}, fmt.Errorf("background %s run exited immediately (%v): %s", mode, waitErr, tail)
			}
			status := "done"
			summary := fmt.Sprintf("background %s run finished quickly", mode)
			if rec != nil {
				status = rec.Status
				if rec.SummaryText != "" {
					summary = rec.SummaryText
				}
			}
			return BackgroundStartResult{
				RunID:      runID,
				WorkItemID: workItemID,
				Mode:       mode,
				Provider:   provider,
				PID:        pid,
				Status:     status,
				LogPath:    logPath,
				Summary:    summary,
			}, nil
		case <-ticker.C:
			rec, _ := loadRun(ctx, db, runID)
			if rec != nil && (!expectExistingRow || rec.Status == "running" || rec.Status == "done") {
				return BackgroundStartResult{
					RunID:      runID,
					WorkItemID: workItemID,
					Mode:       mode,
					Provider:   provider,
					PID:        pid,
					Status:     rec.Status,
					LogPath:    logPath,
					Summary:    fmt.Sprintf("detached %s worker started (pid=%d); inspect via scheduler_status", mode, pid),
				}, nil
			}
		case <-deadline.C:
			return BackgroundStartResult{
				RunID:      runID,
				WorkItemID: workItemID,
				Mode:       mode,
				Provider:   provider,
				PID:        pid,
				Status:     "starting",
				LogPath:    logPath,
				Summary:    fmt.Sprintf("detached %s worker spawned (pid=%d); run record initializing", mode, pid),
			}, nil
		case <-ctx.Done():
			return BackgroundStartResult{}, ctx.Err()
		}
	}
}

func resolveTargetWorkItem(ctx context.Context, wf *workflow.SQLiteService, workspaceRoot, workItemID string) (string, *workflow.WorkItemDetail, error) {
	ws, err := filepath.Abs(strings.TrimSpace(workspaceRoot))
	if err != nil {
		return "", nil, fmt.Errorf("resolve workspace: %w", err)
	}
	var detail *workflow.WorkItemDetail
	if id := strings.TrimSpace(workItemID); id != "" {
		detail, err = wf.GetWorkItem(ctx, ws, id)
	} else {
		detail, err = wf.GetActiveWorkItem(ctx, ws)
	}
	if err != nil {
		return "", nil, err
	}
	if detail == nil {
		return "", nil, fmt.Errorf("no matching or active work item found; create one via workflow_work_create first")
	}
	return ws, detail, nil
}

func ensureNoLiveLease(ctx context.Context, wf *workflow.SQLiteService, workspaceRoot, workItemID string) error {
	db, err := wf.DB(ctx, workspaceRoot)
	if err != nil {
		return err
	}
	lease, err := loadLeaseByKey(ctx, db, "workitem:"+workItemID)
	if err != nil {
		return err
	}
	if lease != nil && lease.Alive {
		return fmt.Errorf("work item %s already has a live background run %s (pid=%d); inspect via scheduler_status or stop via scheduler_stop",
			workItemID, lease.RunID, lease.OwnerPID)
	}
	return nil
}

func normalizeProvider(p string) string {
	trimmed := strings.ToLower(strings.TrimSpace(p))
	if trimmed == "" {
		return "codex"
	}
	return trimmed
}

func resolveJtaoExecutable(explicit string) (string, error) {
	if p := strings.TrimSpace(explicit); p != "" {
		return p, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve jtao executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}
