package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	DefaultTurnTimeout = 30 * time.Minute
	MinTurnTimeout     = 1 * time.Second
	MaxTurnTimeout     = 6 * time.Hour
	killGraceWindow    = 1500 * time.Millisecond
)

// ProcessResult captures full execution and termination telemetry for a managed process tree.
type ProcessResult struct {
	ExitCode      int
	TimedOut      bool
	Aborted       bool
	StartFailed   bool
	TreeAlive     bool
	KillEscalated bool
	ChildPID      int
	Stdout        string
	Stderr        string
	ElapsedMs     int64
	Err           error
}

// NormalizeTimeout clamps a turn timeout into [MinTurnTimeout, MaxTurnTimeout].
func NormalizeTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultTurnTimeout
	}
	if d < MinTurnTimeout {
		return MinTurnTimeout
	}
	if d > MaxTurnTimeout {
		return MaxTurnTimeout
	}
	return d
}

// ExecProcessGroup runs command in its own POSIX process group (Setpgid: true),
// enforces timeout/cancellation via SIGTERM -> SIGKILL to -pgid, and probes
// process-group liveness so callers can fail closed if orphan processes survive.
func ExecProcessGroup(ctx context.Context, command string, args []string, cwd string, env []string, timeout time.Duration) ProcessResult {
	start := time.Now()
	effectiveTimeout := timeout
	if effectiveTimeout <= 0 {
		effectiveTimeout = DefaultTurnTimeout
	}

	cmd := exec.Command(command, args...)
	cmd.Dir = cwd
	if len(env) > 0 {
		cmd.Env = env
	} else {
		cmd.Env = os.Environ()
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return ProcessResult{
			ExitCode:    -1,
			StartFailed: true,
			ElapsedMs:   time.Since(start).Milliseconds(),
			Err:         fmt.Errorf("start %s: %w", command, err),
		}
	}

	pid := cmd.Process.Pid
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	timer := time.NewTimer(effectiveTimeout)
	defer timer.Stop()

	var waitErr error
	timedOut := false
	aborted := false
	killEscalated := false

	select {
	case waitErr = <-waitCh:
	case <-timer.C:
		timedOut = true
		killEscalated, waitErr = terminateProcessGroup(pid, waitCh)
	case <-ctx.Done():
		aborted = true
		killEscalated, waitErr = terminateProcessGroup(pid, waitCh)
	}

	treeAlive := isProcessGroupAlive(pid)
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}

	return ProcessResult{
		ExitCode:      exitCode,
		TimedOut:      timedOut,
		Aborted:       aborted,
		TreeAlive:     treeAlive,
		KillEscalated: killEscalated,
		ChildPID:      pid,
		Stdout:        stdoutBuf.String(),
		Stderr:        stderrBuf.String(),
		ElapsedMs:     time.Since(start).Milliseconds(),
		Err:           waitErr,
	}
}

func terminateProcessGroup(pgid int, waitCh <-chan error) (bool, error) {
	// Graceful SIGTERM to entire process group first.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)

	grace := time.NewTimer(killGraceWindow)
	defer grace.Stop()
	select {
	case err := <-waitCh:
		// Ensure any lingering grandchildren in the group are also gone.
		if isProcessGroupAlive(pgid) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			time.Sleep(100 * time.Millisecond)
			return true, err
		}
		return false, err
	case <-grace.C:
	}

	// Escalate to SIGKILL on entire process group.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	killWait := time.NewTimer(1 * time.Second)
	defer killWait.Stop()
	select {
	case err := <-waitCh:
		return true, err
	case <-killWait.C:
		return true, fmt.Errorf("process group %d did not exit after SIGKILL", pgid)
	}
}

func isProcessGroupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
