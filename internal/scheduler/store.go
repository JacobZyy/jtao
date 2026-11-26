package scheduler

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/JacobZyy/jtao/internal/runner"
	"github.com/JacobZyy/jtao/internal/workflow"
)

// MemoryPort is the optional narrow interface Scheduler uses to talk to internal/memory
// without coupling scheduler logic to memory storage internals.
type MemoryPort interface {
	EnsureSession(ctx context.Context, workspaceRoot, sessionID, agent, goal, workItemID string) (string, error)
	RecallForTurn(ctx context.Context, workspaceRoot, query, sessionID, agent, workItemID string) (string, error)
	Checkpoint(ctx context.Context, workspaceRoot, sessionID, workItemID, status, summary string, nextActions, artifacts []string) error
	UpdateContinuity(ctx context.Context, workspaceRoot, sessionID, workItemID, intent, summary string, touchedFiles, nextActions []string) error
	StoreArtifact(ctx context.Context, workspaceRoot, sessionID, workItemID, tool, content string) (string, error)
}

// BackoffState tracks consecutive retryable failures.
type BackoffState struct {
	ConsecutiveFailures int    `json:"consecutiveFailures"`
	Until               string `json:"until,omitempty"`
	LastFailureClass    string `json:"lastFailureClass,omitempty"`
}

// RunRecord mirrors a row in workflow.db's runs table.
type RunRecord struct {
	ID               string                 `json:"id"`
	WorkItemID       string                 `json:"workItemId"`
	Mode             string                 `json:"mode"` // solo | team
	Provider         string                 `json:"provider"`
	Status           string                 `json:"status"`
	CurrentIteration int                    `json:"currentIteration"`
	MaxIterations    int                    `json:"maxIterations"`
	Worktree         *runner.WorktreeHandle `json:"worktree,omitempty"`
	Pacing           PacingState            `json:"pacing"`
	Backoff          BackoffState           `json:"backoff"`
	StopRequested    bool                   `json:"stopRequested"`
	StopReason       string                 `json:"stopReason,omitempty"`
	SummaryText      string                 `json:"summaryText,omitempty"`
	CreatedAt        string                 `json:"createdAt"`
	UpdatedAt        string                 `json:"updatedAt"`
}

// TurnRecord mirrors a row in workflow.db's run_turns table.
type TurnRecord struct {
	ID           string                `json:"id"`
	RunID        string                `json:"runId"`
	WorkItemID   string                `json:"workItemId"`
	NodeID       string                `json:"nodeId"`
	Iteration    int                   `json:"iteration"`
	PhaseState   string                `json:"phaseState"` // started | completed | aborted | crashed
	Outcome      Outcome               `json:"outcome,omitempty"`
	FailureClass string                `json:"failureClass,omitempty"`
	Material     bool                  `json:"material"`
	IsSafeBypass bool                  `json:"isSafeBypass,omitempty"`
	OutputRef    string                `json:"outputRef,omitempty"`
	ReceiptIDs   []string              `json:"receiptIds,omitempty"`
	FilesTouched []string              `json:"filesTouched,omitempty"`
	Handoff      *workflow.NodeHandoff `json:"handoff,omitempty"`
	ElapsedMs    int64                 `json:"elapsedMs"`
	StartedAt    string                `json:"startedAt"`
	FinishedAt   string                `json:"finishedAt,omitempty"`
}

func newRunID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return fmt.Sprintf("%s:%d-%s", prefix, time.Now().UTC().UnixMilli(), hex.EncodeToString(raw[:]))
}

func insertRun(ctx context.Context, db *sql.DB, r RunRecord) error {
	wtJSON := "null"
	if r.Worktree != nil {
		if b, err := json.Marshal(r.Worktree); err == nil {
			wtJSON = string(b)
		}
	}
	pacingJSON, _ := json.Marshal(r.Pacing)
	boJSON, _ := json.Marshal(r.Backoff)
	stopInt := 0
	if r.StopRequested {
		stopInt = 1
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO runs (
			id, work_item_id, mode, provider, status, current_iteration, max_iterations,
			worktree_json, pacing_json, backoff_json, stop_requested, stop_reason, summary_text,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, r.ID, r.WorkItemID, r.Mode, r.Provider, r.Status, r.CurrentIteration, r.MaxIterations,
		wtJSON, string(pacingJSON), string(boJSON), stopInt, r.StopReason, r.SummaryText, r.CreatedAt, r.UpdatedAt)
	return err
}

func updateRun(ctx context.Context, db *sql.DB, r RunRecord) error {
	wtJSON := "null"
	if r.Worktree != nil {
		if b, err := json.Marshal(r.Worktree); err == nil {
			wtJSON = string(b)
		}
	}
	pacingJSON, _ := json.Marshal(r.Pacing)
	boJSON, _ := json.Marshal(r.Backoff)
	stopInt := 0
	if r.StopRequested {
		stopInt = 1
	}
	_, err := db.ExecContext(ctx, `
		UPDATE runs SET
			status = ?, current_iteration = ?, max_iterations = ?,
			worktree_json = ?, pacing_json = ?, backoff_json = ?, stop_requested = ?,
			stop_reason = ?, summary_text = ?, updated_at = ?
		WHERE id = ?
	`, r.Status, r.CurrentIteration, r.MaxIterations,
		wtJSON, string(pacingJSON), string(boJSON), stopInt, r.StopReason, r.SummaryText, r.UpdatedAt, r.ID)
	return err
}

func loadRun(ctx context.Context, db *sql.DB, runID string) (*RunRecord, error) {
	var r RunRecord
	var wtJSON, pacingJSON, boJSON string
	var stopInt int
	err := db.QueryRowContext(ctx, `
		SELECT id, work_item_id, mode, provider, status, current_iteration, max_iterations,
		       worktree_json, pacing_json, backoff_json, stop_requested, stop_reason, summary_text,
		       created_at, updated_at
		FROM runs WHERE id = ?
	`, runID).Scan(
		&r.ID, &r.WorkItemID, &r.Mode, &r.Provider, &r.Status, &r.CurrentIteration, &r.MaxIterations,
		&wtJSON, &pacingJSON, &boJSON, &stopInt, &r.StopReason, &r.SummaryText, &r.CreatedAt, &r.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.StopRequested = stopInt != 0
	if wtJSON != "" && wtJSON != "null" {
		var wt runner.WorktreeHandle
		if err := json.Unmarshal([]byte(wtJSON), &wt); err == nil {
			r.Worktree = &wt
		}
	}
	if pacingJSON != "" && pacingJSON != "null" {
		_ = json.Unmarshal([]byte(pacingJSON), &r.Pacing)
	}
	if boJSON != "" && boJSON != "null" {
		_ = json.Unmarshal([]byte(boJSON), &r.Backoff)
	}
	return &r, nil
}

func latestRunForWorkItem(ctx context.Context, db *sql.DB, workItemID string) (*RunRecord, error) {
	var runID string
	err := db.QueryRowContext(ctx, `SELECT id FROM runs WHERE work_item_id = ? ORDER BY updated_at DESC LIMIT 1`, workItemID).Scan(&runID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return loadRun(ctx, db, runID)
}

func recordTurnStart(ctx context.Context, db *sql.DB, t TurnRecord) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO run_turns (
			id, run_id, work_item_id, node_id, iteration, phase_state, started_at
		) VALUES (?, ?, ?, ?, ?, 'started', ?)
	`, t.ID, t.RunID, t.WorkItemID, t.NodeID, t.Iteration, t.StartedAt)
	return err
}

func recordTurnFinish(ctx context.Context, db *sql.DB, t TurnRecord) error {
	matInt := 0
	if t.Material {
		matInt = 1
	}
	bypassInt := 0
	if t.IsSafeBypass {
		bypassInt = 1
	}
	recJSON, _ := json.Marshal(t.ReceiptIDs)
	if len(t.ReceiptIDs) == 0 {
		recJSON = []byte("[]")
	}
	filesJSON, _ := json.Marshal(t.FilesTouched)
	if len(t.FilesTouched) == 0 {
		filesJSON = []byte("[]")
	}
	handoffJSON := "null"
	if t.Handoff != nil {
		if b, err := json.Marshal(t.Handoff); err == nil {
			handoffJSON = string(b)
		}
	}
	_, err := db.ExecContext(ctx, `
		UPDATE run_turns SET
			phase_state = ?, outcome = ?, failure_class = ?, material = ?, is_safe_bypass = ?,
			output_ref = ?, receipt_ids_json = ?, files_touched_json = ?, handoff_json = ?,
			elapsed_ms = ?, finished_at = ?
		WHERE id = ?
	`, t.PhaseState, string(t.Outcome), t.FailureClass, matInt, bypassInt,
		t.OutputRef, string(recJSON), string(filesJSON), handoffJSON, t.ElapsedMs, t.FinishedAt, t.ID)
	return err
}

func loadRunTurns(ctx context.Context, db *sql.DB, runID string) ([]TurnRecord, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, run_id, work_item_id, node_id, iteration, phase_state, outcome,
		       failure_class, material, is_safe_bypass, output_ref, receipt_ids_json,
		       files_touched_json, handoff_json, elapsed_ms, started_at, finished_at
		FROM run_turns WHERE run_id = ? ORDER BY iteration ASC
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TurnRecord
	for rows.Next() {
		var t TurnRecord
		var outcomeStr, recJSON, filesJSON, handoffJSON string
		var matInt, bypassInt int
		if err := rows.Scan(
			&t.ID, &t.RunID, &t.WorkItemID, &t.NodeID, &t.Iteration, &t.PhaseState, &outcomeStr,
			&t.FailureClass, &matInt, &bypassInt, &t.OutputRef, &recJSON,
			&filesJSON, &handoffJSON, &t.ElapsedMs, &t.StartedAt, &t.FinishedAt,
		); err != nil {
			return nil, err
		}
		t.Outcome = Outcome(outcomeStr)
		t.Material = matInt != 0
		t.IsSafeBypass = bypassInt != 0
		_ = json.Unmarshal([]byte(recJSON), &t.ReceiptIDs)
		_ = json.Unmarshal([]byte(filesJSON), &t.FilesTouched)
		if handoffJSON != "" && handoffJSON != "null" {
			var h workflow.NodeHandoff
			if err := json.Unmarshal([]byte(handoffJSON), &h); err == nil {
				t.Handoff = &h
			}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func markIncompleteTurnsCrashed(ctx context.Context, db *sql.DB, runID, nowISO string) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE run_turns SET phase_state = 'crashed', outcome = 'infra-retry', finished_at = ?
		WHERE run_id = ? AND phase_state = 'started'
	`, nowISO, runID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// LeaseInfo describes an active SQLite run lease in workflow.db.
type LeaseInfo struct {
	LeaseKey  string `json:"leaseKey"`
	RunID     string `json:"runId"`
	OwnerPID  int    `json:"ownerPid"`
	Hostname  string `json:"hostname"`
	Alive     bool   `json:"alive"`
	ExpiresAt string `json:"expiresAt"`
}

// RunSnapshot is the single authoritative status view of a Solo or Team run.
type RunSnapshot struct {
	Run         RunRecord                `json:"run"`
	Turns       []TurnRecord             `json:"turns,omitempty"`
	Lease       *LeaseInfo               `json:"lease,omitempty"`
	LiveProcess bool                     `json:"liveProcess"`
	WorkItem    *workflow.WorkItemDetail `json:"workItem,omitempty"`
	DoneGate    *workflow.DoneGateResult `json:"doneGate,omitempty"`
	LogPath     string                   `json:"logPath,omitempty"`
	LogTail     string                   `json:"logTail,omitempty"`
}

// StopRunResult captures the outcome of a stop control request.
type StopRunResult struct {
	Run         RunRecord `json:"run"`
	SignaledPID int       `json:"signaledPid,omitempty"`
	Immediate   bool      `json:"immediate"`
}

// RunLogPath returns the canonical detached worker log path for a run inside <wfDir>/logs/.
func RunLogPath(wfDir, runID string) string {
	replacer := strings.NewReplacer(":", "_", "/", "_", "\\", "_", " ", "_")
	return filepath.Join(wfDir, "logs", replacer.Replace(runID)+".log")
}

func loadLeaseByKey(ctx context.Context, db *sql.DB, leaseKey string) (*LeaseInfo, error) {
	var info LeaseInfo
	err := db.QueryRowContext(ctx, `
		SELECT lease_key, run_id, owner_pid, hostname, expires_at
		FROM run_leases WHERE lease_key = ?
	`, leaseKey).Scan(&info.LeaseKey, &info.RunID, &info.OwnerPID, &info.Hostname, &info.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	info.Alive = isLeaseLive(info)
	return &info, nil
}

func loadLeaseByRunID(ctx context.Context, db *sql.DB, runID string) (*LeaseInfo, error) {
	var info LeaseInfo
	err := db.QueryRowContext(ctx, `
		SELECT lease_key, run_id, owner_pid, hostname, expires_at
		FROM run_leases WHERE run_id = ? LIMIT 1
	`, runID).Scan(&info.LeaseKey, &info.RunID, &info.OwnerPID, &info.Hostname, &info.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	info.Alive = isLeaseLive(info)
	return &info, nil
}

func isLeaseLive(info LeaseInfo) bool {
	expTime, parseErr := time.Parse(time.RFC3339Nano, info.ExpiresAt)
	if parseErr != nil || !expTime.After(time.Now().UTC()) {
		return false
	}
	host, _ := os.Hostname()
	if info.Hostname != host {
		return true
	}
	return isPIDAlive(info.OwnerPID)
}

func latestWorkspaceRun(ctx context.Context, db *sql.DB) (*RunRecord, error) {
	var runID string
	err := db.QueryRowContext(ctx, `SELECT id FROM runs ORDER BY updated_at DESC LIMIT 1`).Scan(&runID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return loadRun(ctx, db, runID)
}

// LatestRun resolves a run by runID, else latest for workItemID, else most recently updated workspace run.
func LatestRun(ctx context.Context, wf *workflow.SQLiteService, workspaceRoot, runID, workItemID string) (*RunRecord, error) {
	db, err := wf.DB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	if id := strings.TrimSpace(runID); id != "" {
		return loadRun(ctx, db, id)
	}
	if wi := strings.TrimSpace(workItemID); wi != "" {
		return latestRunForWorkItem(ctx, db, wi)
	}
	return latestWorkspaceRun(ctx, db)
}

// InspectRun returns a unified snapshot of a Solo or Team run, including lease liveness,
// turns, WorkItem DAG state, DoneGate, and detached worker log tail.
func InspectRun(ctx context.Context, wf *workflow.SQLiteService, workspaceRoot, runID, workItemID string) (*RunSnapshot, error) {
	run, err := LatestRun(ctx, wf, workspaceRoot, runID, workItemID)
	if err != nil || run == nil {
		return nil, err
	}
	db, err := wf.DB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	turns, err := loadRunTurns(ctx, db, run.ID)
	if err != nil {
		return nil, err
	}
	lease, _ := loadLeaseByRunID(ctx, db, run.ID)
	wi, _ := wf.GetWorkItem(ctx, workspaceRoot, run.WorkItemID)
	var gate *workflow.DoneGateResult
	if wi != nil {
		if g, gErr := wf.EvaluateDoneGate(ctx, workspaceRoot, wi.ID); gErr == nil {
			gate = &g
		}
	}

	logPath := ""
	logTail := ""
	if wfDir, dirErr := workflow.ResolveWorkflowDir(workspaceRoot); dirErr == nil {
		candidate := RunLogPath(wfDir, run.ID)
		if tail, ok := readTailText(candidate, 2048); ok {
			logPath = candidate
			logTail = tail
		}
	}

	return &RunSnapshot{
		Run:         *run,
		Turns:       turns,
		Lease:       lease,
		LiveProcess: lease != nil && lease.Alive,
		WorkItem:    wi,
		DoneGate:    gate,
		LogPath:     logPath,
		LogTail:     logTail,
	}, nil
}

// RequestStop sets stop_requested on a Solo or Team run, and when immediate is true
// (or the run is a Team run), sends SIGTERM to its local leased worker process group.
func RequestStop(ctx context.Context, wf *workflow.SQLiteService, workspaceRoot, runID, workItemID, reason string, immediate bool) (*StopRunResult, error) {
	run, err := LatestRun(ctx, wf, workspaceRoot, runID, workItemID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("no matching run found")
	}
	db, err := wf.DB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(reason) == "" {
		reason = "operator-request"
	}
	run.StopRequested = true
	run.StopReason = reason
	run.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := updateRun(ctx, db, *run); err != nil {
		return nil, err
	}

	signaledPID := 0
	if immediate || run.Mode == "team" {
		lease, _ := loadLeaseByRunID(ctx, db, run.ID)
		host, _ := os.Hostname()
		if lease != nil && lease.Alive && lease.Hostname == host && lease.OwnerPID > 0 {
			// Signal the detached worker process group cleanly; jtao traps SIGTERM via signal.NotifyContext.
			if err := syscall.Kill(-lease.OwnerPID, syscall.SIGTERM); err == nil {
				signaledPID = lease.OwnerPID
			} else if err := syscall.Kill(lease.OwnerPID, syscall.SIGTERM); err == nil {
				signaledPID = lease.OwnerPID
			}
		}
	}

	return &StopRunResult{
		Run:         *run,
		SignaledPID: signaledPID,
		Immediate:   immediate || run.Mode == "team",
	}, nil
}

func readTailText(path string, maxBytes int64) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return "", false
	}
	size := info.Size()
	if size == 0 {
		return "", true
	}
	offset := int64(0)
	if size > maxBytes {
		offset = size - maxBytes
	}
	buf := make([]byte, size-offset)
	n, err := f.ReadAt(buf, offset)
	if err != nil && n == 0 {
		return "", true
	}
	return strings.TrimSpace(string(buf[:n])), true
}
