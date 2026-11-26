package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultReceiptTimeout   = 10 * time.Minute
	defaultOffloadThreshold = 2048
)

// OffloadHook persists large outputs externally (e.g. via internal/memory Artifact)
// and returns a short artifact reference such as "artifact:...".
type OffloadHook func(ctx context.Context, workspaceRoot, sessionID, workItemID, tool, content string) (string, error)

// ReceiptRecord is the runner-owned execution proof payload passed to a ReceiptSink.
type ReceiptRecord struct {
	WorkItemID    string
	NodeID        string
	RunID         string
	Command       string
	Args          []string
	Cwd           string
	ExitCode      int
	TimedOut      bool
	DurationMs    int64
	StdoutSHA256  string
	StderrSHA256  string
	OutputExcerpt string
	ArtifactRef   string
}

// ReceiptSink persists a ReceiptRecord and returns its canonical receipt ID ("receipt:...").
type ReceiptSink func(ctx context.Context, workspaceRoot string, rec ReceiptRecord) (string, error)

// ReceiptExecInput describes a verification command to execute and attest.
type ReceiptExecInput struct {
	WorkspaceRoot string
	ExecutionCwd  string
	WorkItemID    string
	NodeID        string
	RunID         string
	SessionID     string
	Command       string
	Args          []string
	Timeout       time.Duration
	Env           []string
}

// ReceiptExecOutput returns both the recorded receipt ID and full process telemetry.
type ReceiptExecOutput struct {
	ReceiptID string        `json:"receiptId"`
	Record    ReceiptRecord `json:"record"`
	Result    ProcessResult `json:"-"`
}

// ReceiptExecutor runs real verification commands and emits tamper-evident receipts.
type ReceiptExecutor struct {
	Sink    ReceiptSink
	Offload OffloadHook
}

// RunAndRecord executes the command inside executionCwd, hashes outputs, optionally
// offloads large output, and persists the receipt via Sink.
func (e ReceiptExecutor) RunAndRecord(ctx context.Context, in ReceiptExecInput) (ReceiptExecOutput, error) {
	if strings.TrimSpace(in.Command) == "" {
		return ReceiptExecOutput{}, fmt.Errorf("receipt command is required")
	}
	cwd := strings.TrimSpace(in.ExecutionCwd)
	if cwd == "" {
		cwd = in.WorkspaceRoot
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return ReceiptExecOutput{}, fmt.Errorf("resolve execution cwd: %w", err)
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = defaultReceiptTimeout
	}

	cmdName := in.Command
	extraDir := ""
	if !strings.ContainsRune(cmdName, filepath.Separator) {
		if resolved, err := ResolveBinary(cmdName); err == nil {
			cmdName = resolved
			extraDir = filepath.Dir(resolved)
		}
	}
	env := BuildAugmentedEnv(in.Env, extraDir)

	procRes := ExecProcessGroup(ctx, cmdName, in.Args, absCwd, env, timeout)
	combined := strings.TrimSpace(procRes.Stdout + "\n" + procRes.Stderr)

	artifactRef := ""
	if e.Offload != nil && len(combined) >= defaultOffloadThreshold {
		if ref, err := e.Offload(ctx, in.WorkspaceRoot, in.SessionID, in.WorkItemID, "receipt:"+in.Command, combined); err == nil {
			artifactRef = ref
		}
	}

	rec := ReceiptRecord{
		WorkItemID:    in.WorkItemID,
		NodeID:        in.NodeID,
		RunID:         in.RunID,
		Command:       in.Command,
		Args:          append([]string(nil), in.Args...),
		Cwd:           absCwd,
		ExitCode:      procRes.ExitCode,
		TimedOut:      procRes.TimedOut,
		DurationMs:    procRes.ElapsedMs,
		StdoutSHA256:  hashText(procRes.Stdout),
		StderrSHA256:  hashText(procRes.Stderr),
		OutputExcerpt: clipText(combined, 800),
		ArtifactRef:   artifactRef,
	}

	receiptID := ""
	if e.Sink != nil {
		receiptID, err = e.Sink(ctx, in.WorkspaceRoot, rec)
		if err != nil {
			return ReceiptExecOutput{Record: rec, Result: procRes}, fmt.Errorf("persist receipt: %w", err)
		}
	}

	return ReceiptExecOutput{
		ReceiptID: receiptID,
		Record:    rec,
		Result:    procRes,
	}, nil
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func clipText(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "\n...(truncated)"
}
