package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// RecordReceipt stores an immutable local command execution receipt in SQLite
// and writes an audit JSON sidecar under <workflowDir>/receipts/.
func (s *SQLiteService) RecordReceipt(ctx context.Context, workspaceRoot string, r ExecutionReceipt) (ExecutionReceipt, error) {
	if strings.TrimSpace(r.Command) == "" {
		return ExecutionReceipt{}, fmt.Errorf("receipt command is required")
	}
	if strings.TrimSpace(r.Cwd) == "" {
		return ExecutionReceipt{}, fmt.Errorf("receipt cwd is required")
	}

	db, wfDir, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return ExecutionReceipt{}, err
	}

	if strings.TrimSpace(r.ID) == "" {
		r.ID = newToken("receipt")
	}
	if !strings.HasPrefix(r.ID, "receipt:") {
		r.ID = "receipt:" + r.ID
	}
	if strings.TrimSpace(r.CreatedAt) == "" {
		r.CreatedAt = s.nowISO()
	}
	absCwd, err := filepath.Abs(r.Cwd)
	if err == nil {
		r.Cwd = absCwd
	}

	timedOutInt := 0
	if r.TimedOut {
		timedOutInt = 1
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO execution_receipts (
			id, work_item_id, node_id, run_id, command, args_json, cwd,
			exit_code, timed_out, duration_ms, stdout_sha256, stderr_sha256,
			output_excerpt, artifact_ref, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		r.ID, r.WorkItemID, r.NodeID, r.RunID, r.Command, marshalJSON(r.Args, "[]"), r.Cwd,
		r.ExitCode, timedOutInt, r.DurationMs, r.StdoutSHA256, r.StderrSHA256,
		r.OutputExcerpt, r.ArtifactRef, r.CreatedAt,
	)
	if err != nil {
		return ExecutionReceipt{}, fmt.Errorf("insert execution_receipts: %w", err)
	}

	// Best-effort audit JSON sidecar for human inspection.
	safeName := strings.ReplaceAll(r.ID, ":", "_") + ".json"
	if raw, err := json.MarshalIndent(r, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(wfDir, "receipts", safeName), append(raw, '\n'), 0o644)
	}

	return r, nil
}

// GetReceipt loads a recorded execution receipt by ID.
func (s *SQLiteService) GetReceipt(ctx context.Context, workspaceRoot, receiptID string) (*ExecutionReceipt, error) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	var r ExecutionReceipt
	var argsJSON string
	var timedOutInt int
	err = db.QueryRowContext(ctx, `
		SELECT id, work_item_id, node_id, run_id, command, args_json, cwd,
		       exit_code, timed_out, duration_ms, stdout_sha256, stderr_sha256,
		       output_excerpt, artifact_ref, created_at
		FROM execution_receipts WHERE id = ?
	`, strings.TrimSpace(receiptID)).Scan(
		&r.ID, &r.WorkItemID, &r.NodeID, &r.RunID, &r.Command, &argsJSON, &r.Cwd,
		&r.ExitCode, &timedOutInt, &r.DurationMs, &r.StdoutSHA256, &r.StderrSHA256,
		&r.OutputExcerpt, &r.ArtifactRef, &r.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Args = unmarshalStrings(argsJSON)
	r.TimedOut = timedOutInt != 0
	return &r, nil
}

// VerifyReceipt validates that receiptID exists, ran the exact command/args,
// and produced the expected exit code without timing out.
func (s *SQLiteService) VerifyReceipt(ctx context.Context, workspaceRoot, receiptID string, spec VerifyCommandSpec) (ReceiptVerifyResult, error) {
	ref := strings.TrimSpace(receiptID)
	if !strings.HasPrefix(ref, "receipt:") {
		return ReceiptVerifyResult{OK: false, Reason: fmt.Sprintf("invalid receipt reference %q: must start with receipt:", receiptID)}, nil
	}

	r, err := s.GetReceipt(ctx, workspaceRoot, ref)
	if err != nil {
		return ReceiptVerifyResult{}, err
	}
	if r == nil {
		return ReceiptVerifyResult{OK: false, Reason: fmt.Sprintf("receipt not found: %s", ref)}, nil
	}
	if r.Command != spec.Command {
		return ReceiptVerifyResult{OK: false, Reason: fmt.Sprintf("receipt command mismatch: expected %q, got %q", spec.Command, r.Command), Receipt: r}, nil
	}
	expectedArgs := spec.Args
	if expectedArgs == nil {
		expectedArgs = []string{}
	}
	actualArgs := r.Args
	if actualArgs == nil {
		actualArgs = []string{}
	}
	if !reflect.DeepEqual(expectedArgs, actualArgs) {
		return ReceiptVerifyResult{OK: false, Reason: fmt.Sprintf("receipt args mismatch: expected %v, got %v", expectedArgs, actualArgs), Receipt: r}, nil
	}
	if r.TimedOut {
		return ReceiptVerifyResult{OK: false, Reason: "receipt command timed out", Receipt: r}, nil
	}
	if r.ExitCode != spec.ExpectedExitCode {
		return ReceiptVerifyResult{OK: false, Reason: fmt.Sprintf("receipt exit code mismatch: expected %d, got %d", spec.ExpectedExitCode, r.ExitCode), Receipt: r}, nil
	}
	return ReceiptVerifyResult{OK: true, Receipt: r}, nil
}
