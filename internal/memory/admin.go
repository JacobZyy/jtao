package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Doctor checks SQLite integrity, WAL mode, FTS5 availability, and record counts.
func (s *SQLiteService) Doctor(ctx context.Context, workspaceRoot string) (DoctorReport, error) {
	db, memDir, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return DoctorReport{}, err
	}

	report := DoctorReport{
		DBPath: filepath.Join(memDir, "memory.db"),
	}
	_ = db.QueryRowContext(ctx, `PRAGMA journal_mode;`).Scan(&report.JournalMode)
	_ = db.QueryRowContext(ctx, `PRAGMA integrity_check;`).Scan(&report.IntegrityResult)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memo_events`).Scan(&report.MemoCount)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&report.SessionCount)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts`).Scan(&report.ArtifactCount)

	var ftsCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memo_fts`).Scan(&ftsCount); err == nil {
		report.FTS5Available = true
	}

	report.OK = strings.EqualFold(report.IntegrityResult, "ok") && report.FTS5Available
	return report, nil
}

// Hygiene inspects memory bloat, stale active sessions, expired private memos,
// and unreferenced artifact files. When apply=true, closes stale sessions and
// removes orphan artifact files on disk.
func (s *SQLiteService) Hygiene(ctx context.Context, workspaceRoot string, staleDays int, apply bool) (HygieneReport, error) {
	if staleDays <= 0 {
		staleDays = 14
	}
	db, memDir, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return HygieneReport{}, err
	}

	var rep HygieneReport
	_ = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM memo_events m
		WHERE m.archived = 0 AND m.claim_status = 'verified'
		  AND NOT EXISTS (SELECT 1 FROM memo_supersedes ms WHERE ms.target_id = m.id)
	`).Scan(&rep.ActiveMemos)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT target_id) FROM memo_supersedes`).Scan(&rep.SupersededMemos)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memo_events WHERE archived = 1`).Scan(&rep.ArchivedMemos)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memo_events WHERE archived = 0 AND claim_status = 'candidate'`).Scan(&rep.PendingCandidates)

	privateCutoff := s.now().Add(-agentPrivateTTL).UTC().Format(time.RFC3339Nano)
	_ = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM memo_events
		WHERE archived = 0 AND scope = 'agent_private' AND created_at < ?
	`, privateCutoff).Scan(&rep.ExpiredPrivateMemos)

	staleCutoff := s.now().Add(-time.Duration(staleDays) * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	_ = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sessions WHERE status = 'active' AND updated_at < ?
	`, staleCutoff).Scan(&rep.StaleActiveSessions)

	if apply && rep.StaleActiveSessions > 0 {
		res, err := db.ExecContext(ctx, `
			UPDATE sessions SET status = 'archived', updated_at = ?
			WHERE status = 'active' AND updated_at < ?
		`, s.nowISO(), staleCutoff)
		if err != nil {
			return rep, fmt.Errorf("archive stale sessions: %w", err)
		}
		rep.ArchivedStaleSessions, _ = res.RowsAffected()
	}

	// Detect orphan artifact files on disk not referenced by any artifacts row.
	referenced := make(map[string]struct{})
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT rel_path FROM artifacts`)
	if err == nil {
		for rows.Next() {
			var rel string
			if err := rows.Scan(&rel); err == nil {
				referenced[filepath.Base(rel)] = struct{}{}
			}
		}
		_ = rows.Close()
	}

	artDir := filepath.Join(memDir, "artifacts")
	if entries, err := os.ReadDir(artDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			if _, ok := referenced[entry.Name()]; ok {
				continue
			}
			rep.OrphanArtifactFiles++
			if apply {
				if err := os.Remove(filepath.Join(artDir, entry.Name())); err == nil {
					rep.CleanedOrphanFiles++
				}
			}
		}
	}

	return rep, nil
}

// RebuildIndex rebuilds all three FTS5 virtual tables from canonical SQLite rows.
func (s *SQLiteService) RebuildIndex(ctx context.Context, workspaceRoot string) error {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	statements := []string{
		`DELETE FROM memo_fts`,
		`INSERT INTO memo_fts (memo_id, search_tokens) SELECT id, search_tokens FROM memo_events`,
		`DELETE FROM session_events_fts`,
		`INSERT INTO session_events_fts (event_id, search_tokens) SELECT id, search_tokens FROM session_events`,
		`DELETE FROM session_checkpoints_fts`,
		`INSERT INTO session_checkpoints_fts (checkpoint_id, search_tokens) SELECT id, search_tokens FROM session_checkpoints`,
	}
	for _, stmt := range statements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("rebuild index (%s): %w", stmt, err)
		}
	}
	return tx.Commit()
}

// ExportJSONL exports all active and historical memo events as newline-delimited JSON.
func (s *SQLiteService) ExportJSONL(ctx context.Context, workspaceRoot string) (string, error) {
	memos, err := s.ListMemos(ctx, ListMemosInput{
		WorkspaceRoot:     workspaceRoot,
		IncludeSuperseded: true,
		IncludeArchived:   true,
		IncludeExpired:    true,
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, m := range memos {
		raw, err := json.Marshal(m)
		if err != nil {
			return "", err
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String(), nil
}
