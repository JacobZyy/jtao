package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// CreateSession creates a new session in the workspace.
func (s *SQLiteService) CreateSession(ctx context.Context, workspaceRoot, agent, project, goal, workItemID string) (Session, error) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return Session{}, err
	}
	agent = strings.TrimSpace(agent)
	if agent == "" {
		agent = "codex"
	}
	project = strings.TrimSpace(project)
	if project == "" {
		project = filepath.Base(workspaceRoot)
	}

	now := s.nowISO()
	sess := Session{
		ID:         newPrefixedID("sess"),
		Agent:      agent,
		Project:    project,
		Goal:       strings.TrimSpace(goal),
		WorkItemID: strings.TrimSpace(workItemID),
		Status:     "active",
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO sessions (id, agent, project, goal, work_item_id, status, tags_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '[]', ?, ?)
	`, sess.ID, sess.Agent, sess.Project, sess.Goal, sess.WorkItemID, sess.Status, sess.CreatedAt, sess.UpdatedAt)
	if err != nil {
		return Session{}, fmt.Errorf("insert session: %w", err)
	}
	return sess, nil
}

// EnsureSession returns an existing session by ID or creates one idempotently.
func (s *SQLiteService) EnsureSession(ctx context.Context, workspaceRoot, sessionID, agent, project, goal, workItemID string) (Session, bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID != "" {
		existing, err := s.GetSession(ctx, workspaceRoot, sessionID)
		if err != nil {
			return Session{}, false, err
		}
		if existing != nil {
			return *existing, false, nil
		}
		db, _, err := s.openDB(ctx, workspaceRoot)
		if err != nil {
			return Session{}, false, err
		}
		if strings.TrimSpace(agent) == "" {
			agent = "codex"
		}
		if strings.TrimSpace(project) == "" {
			project = filepath.Base(workspaceRoot)
		}
		now := s.nowISO()
		sess := Session{
			ID:         sessionID,
			Agent:      strings.TrimSpace(agent),
			Project:    strings.TrimSpace(project),
			Goal:       strings.TrimSpace(goal),
			WorkItemID: strings.TrimSpace(workItemID),
			Status:     "active",
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		_, err = db.ExecContext(ctx, `
			INSERT INTO sessions (id, agent, project, goal, work_item_id, status, tags_json, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, '[]', ?, ?)
		`, sess.ID, sess.Agent, sess.Project, sess.Goal, sess.WorkItemID, sess.Status, sess.CreatedAt, sess.UpdatedAt)
		if err != nil {
			return Session{}, false, fmt.Errorf("ensure session insert: %w", err)
		}
		return sess, true, nil
	}

	sess, err := s.CreateSession(ctx, workspaceRoot, agent, project, goal, workItemID)
	return sess, err == nil, err
}

// GetSession retrieves a session by ID.
func (s *SQLiteService) GetSession(ctx context.Context, workspaceRoot, sessionID string) (*Session, error) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	var sess Session
	var tagsJSON string
	err = db.QueryRowContext(ctx, `
		SELECT id, agent, project, goal, work_item_id, status, tags_json, created_at, updated_at
		FROM sessions WHERE id = ?
	`, sessionID).Scan(
		&sess.ID, &sess.Agent, &sess.Project, &sess.Goal, &sess.WorkItemID,
		&sess.Status, &tagsJSON, &sess.CreatedAt, &sess.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sess.Tags = unmarshalStringSlice(tagsJSON)
	return &sess, nil
}

// LatestSession returns the most recently updated session (optionally filtered by agent).
func (s *SQLiteService) LatestSession(ctx context.Context, workspaceRoot, agent string) (*Session, error) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	query := `SELECT id, agent, project, goal, work_item_id, status, tags_json, created_at, updated_at FROM sessions`
	var args []any
	if strings.TrimSpace(agent) != "" {
		query += ` WHERE agent = ?`
		args = append(args, strings.TrimSpace(agent))
	}
	query += ` ORDER BY updated_at DESC LIMIT 1`

	var sess Session
	var tagsJSON string
	err = db.QueryRowContext(ctx, query, args...).Scan(
		&sess.ID, &sess.Agent, &sess.Project, &sess.Goal, &sess.WorkItemID,
		&sess.Status, &tagsJSON, &sess.CreatedAt, &sess.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sess.Tags = unmarshalStringSlice(tagsJSON)
	return &sess, nil
}

// ListSessions lists recent sessions in reverse update order.
func (s *SQLiteService) ListSessions(ctx context.Context, workspaceRoot string, limit int) ([]Session, error) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, agent, project, goal, work_item_id, status, tags_json, created_at, updated_at
		FROM sessions ORDER BY updated_at DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		var tagsJSON string
		if err := rows.Scan(
			&sess.ID, &sess.Agent, &sess.Project, &sess.Goal, &sess.WorkItemID,
			&sess.Status, &tagsJSON, &sess.CreatedAt, &sess.UpdatedAt,
		); err != nil {
			return nil, err
		}
		sess.Tags = unmarshalStringSlice(tagsJSON)
		out = append(out, sess)
	}
	return out, rows.Err()
}

// AppendEvent appends a timeline event to a session and indexes it in FTS5.
func (s *SQLiteService) AppendEvent(ctx context.Context, workspaceRoot string, ev SessionEvent) (SessionEvent, error) {
	if strings.TrimSpace(ev.SessionID) == "" {
		return SessionEvent{}, fmt.Errorf("sessionId is required")
	}
	if strings.TrimSpace(ev.Text) == "" {
		return SessionEvent{}, fmt.Errorf("event text is required")
	}
	if strings.TrimSpace(ev.Role) == "" {
		ev.Role = "assistant"
	}
	if strings.TrimSpace(ev.Kind) == "" {
		ev.Kind = "note"
	}

	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return SessionEvent{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return SessionEvent{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var nextSeq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM session_events WHERE session_id = ?`, ev.SessionID).Scan(&nextSeq); err != nil {
		return SessionEvent{}, err
	}
	ev.ID = newPrefixedID("ev")
	ev.Seq = nextSeq
	ev.CreatedAt = s.nowISO()
	tokens := TokenizeForSearch(ev.Text + " " + strings.Join(ev.Refs, " ") + " " + ev.WorkItemID)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_events (
			id, session_id, seq, role, kind, text, search_tokens,
			refs_json, work_item_id, turn_id, outcome, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, ev.ID, ev.SessionID, ev.Seq, ev.Role, ev.Kind, ev.Text, tokens,
		marshalStringSlice(ev.Refs), ev.WorkItemID, ev.TurnID, ev.Outcome, ev.CreatedAt); err != nil {
		return SessionEvent{}, fmt.Errorf("insert session_events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_events_fts (event_id, search_tokens) VALUES (?, ?)`, ev.ID, tokens); err != nil {
		return SessionEvent{}, fmt.Errorf("insert session_events_fts: %w", err)
	}
	_, _ = tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, ev.CreatedAt, ev.SessionID)

	if err := tx.Commit(); err != nil {
		return SessionEvent{}, err
	}
	return ev, nil
}

// AddCheckpoint appends a structured checkpoint to a session and indexes it in FTS5.
func (s *SQLiteService) AddCheckpoint(ctx context.Context, workspaceRoot string, cp Checkpoint) (Checkpoint, error) {
	if strings.TrimSpace(cp.SessionID) == "" {
		return Checkpoint{}, fmt.Errorf("sessionId is required")
	}
	if strings.TrimSpace(cp.Summary) == "" {
		return Checkpoint{}, fmt.Errorf("checkpoint summary is required")
	}
	if strings.TrimSpace(cp.Status) == "" {
		cp.Status = "running"
	}

	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return Checkpoint{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Checkpoint{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var nextSeq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM session_checkpoints WHERE session_id = ?`, cp.SessionID).Scan(&nextSeq); err != nil {
		return Checkpoint{}, err
	}
	cp.ID = newPrefixedID("cp")
	cp.Seq = nextSeq
	cp.CreatedAt = s.nowISO()
	tokens := TokenizeForSearch(cp.Summary + " " + strings.Join(cp.NextActions, " ") + " " + cp.WorkItemID)

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_checkpoints (
			id, session_id, seq, work_item_id, status, summary, search_tokens,
			next_json, artifacts_json, verify_result, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, cp.ID, cp.SessionID, cp.Seq, cp.WorkItemID, cp.Status, cp.Summary, tokens,
		marshalStringSlice(cp.NextActions), marshalStringSlice(cp.Artifacts), cp.VerifyResult, cp.CreatedAt); err != nil {
		return Checkpoint{}, fmt.Errorf("insert session_checkpoints: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_checkpoints_fts (checkpoint_id, search_tokens) VALUES (?, ?)`, cp.ID, tokens); err != nil {
		return Checkpoint{}, fmt.Errorf("insert session_checkpoints_fts: %w", err)
	}
	_, _ = tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, cp.CreatedAt, cp.SessionID)

	if err := tx.Commit(); err != nil {
		return Checkpoint{}, err
	}
	return cp, nil
}

// GetContinuity returns the latest continuity summary for a session.
func (s *SQLiteService) GetContinuity(ctx context.Context, workspaceRoot, sessionID string) (*Continuity, error) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	var c Continuity
	var touchedJSON, nextJSON string
	err = db.QueryRowContext(ctx, `
		SELECT session_id, work_item_id, intent, summary, touched_json, next_json, updated_at
		FROM session_continuity WHERE session_id = ?
	`, sessionID).Scan(&c.SessionID, &c.WorkItemID, &c.Intent, &c.Summary, &touchedJSON, &nextJSON, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.TouchedFiles = unmarshalStringSlice(touchedJSON)
	c.NextActions = unmarshalStringSlice(nextJSON)
	return &c, nil
}

// UpdateContinuity upserts compact continuity summary for a session.
func (s *SQLiteService) UpdateContinuity(ctx context.Context, workspaceRoot string, c Continuity) (Continuity, error) {
	if strings.TrimSpace(c.SessionID) == "" {
		return Continuity{}, fmt.Errorf("sessionId is required")
	}
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return Continuity{}, err
	}
	c.UpdatedAt = s.nowISO()
	_, err = db.ExecContext(ctx, `
		INSERT INTO session_continuity (session_id, work_item_id, intent, summary, touched_json, next_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			work_item_id = excluded.work_item_id,
			intent = excluded.intent,
			summary = excluded.summary,
			touched_json = excluded.touched_json,
			next_json = excluded.next_json,
			updated_at = excluded.updated_at
	`, c.SessionID, c.WorkItemID, c.Intent, c.Summary, marshalStringSlice(c.TouchedFiles), marshalStringSlice(c.NextActions), c.UpdatedAt)
	if err != nil {
		return Continuity{}, fmt.Errorf("upsert session_continuity: %w", err)
	}
	return c, nil
}

// WriteHandoff stores a structured handoff packet for a session.
func (s *SQLiteService) WriteHandoff(ctx context.Context, workspaceRoot string, h HandoffPacket) (HandoffPacket, error) {
	if strings.TrimSpace(h.SessionID) == "" {
		return HandoffPacket{}, fmt.Errorf("sessionId is required")
	}
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return HandoffPacket{}, err
	}
	h.UpdatedAt = s.nowISO()
	raw, err := json.Marshal(h)
	if err != nil {
		return HandoffPacket{}, err
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO session_handoffs (session_id, work_item_id, packet_json, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			work_item_id = excluded.work_item_id,
			packet_json = excluded.packet_json,
			updated_at = excluded.updated_at
	`, h.SessionID, h.WorkItemID, string(raw), h.UpdatedAt)
	if err != nil {
		return HandoffPacket{}, fmt.Errorf("upsert session_handoffs: %w", err)
	}
	return h, nil
}

// ReadHandoff retrieves the structured handoff packet for a session.
func (s *SQLiteService) ReadHandoff(ctx context.Context, workspaceRoot, sessionID string) (*HandoffPacket, error) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	var raw string
	err = db.QueryRowContext(ctx, `SELECT packet_json FROM session_handoffs WHERE session_id = ?`, sessionID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var h HandoffPacket
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// PackSession builds a compact recovery context bundle for a session.
func (s *SQLiteService) PackSession(ctx context.Context, workspaceRoot, sessionID string, maxChars int) (SessionPack, error) {
	if maxChars <= 0 {
		maxChars = 4000
	}
	sess, err := s.GetSession(ctx, workspaceRoot, sessionID)
	if err != nil {
		return SessionPack{}, err
	}
	if sess == nil {
		return SessionPack{}, fmt.Errorf("session not found: %s", sessionID)
	}

	pinned, _ := s.PinRender(ctx, workspaceRoot, defaultSpace, maxChars/3)
	cont, _ := s.GetContinuity(ctx, workspaceRoot, sessionID)
	handoff, _ := s.ReadHandoff(ctx, workspaceRoot, sessionID)
	checkpoints, _ := s.listSessionCheckpoints(ctx, workspaceRoot, sessionID, 5)

	var memos []MemoEvent
	if sess.WorkItemID != "" {
		memos, _ = s.ListMemos(ctx, ListMemosInput{
			WorkspaceRoot: workspaceRoot,
			WorkItemID:    sess.WorkItemID,
			ClaimStatus:   ClaimVerified,
			Limit:         8,
		})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## Session Context Pack (%s)\n", sess.ID)
	fmt.Fprintf(&b, "- agent: %s | project: %s | status: %s\n", sess.Agent, sess.Project, sess.Status)
	if sess.WorkItemID != "" {
		fmt.Fprintf(&b, "- workItem: %s\n", sess.WorkItemID)
	}
	if sess.Goal != "" {
		fmt.Fprintf(&b, "- goal: %s\n", sess.Goal)
	}
	if pinned.Text != "" {
		b.WriteString("\n")
		b.WriteString(pinned.Text)
		b.WriteString("\n")
	}
	if cont != nil && cont.Summary != "" {
		b.WriteString("\n### Continuity\n")
		b.WriteString(cont.Summary + "\n")
		if len(cont.NextActions) > 0 {
			b.WriteString("Next: " + strings.Join(cont.NextActions, " | ") + "\n")
		}
	}
	if handoff != nil && handoff.Summary != "" {
		b.WriteString("\n### Latest Handoff\n")
		fmt.Fprintf(&b, "[%s] %s\n", handoff.Status, handoff.Summary)
	}
	if len(checkpoints) > 0 {
		b.WriteString("\n### Recent Checkpoints\n")
		for _, cp := range checkpoints {
			fmt.Fprintf(&b, "- (%s) %s\n", cp.Status, cp.Summary)
		}
	}
	if len(memos) > 0 {
		b.WriteString("\n### Linked Project Memos\n")
		for _, m := range memos {
			fmt.Fprintf(&b, "- [%s] %s\n", m.ID, m.Text)
		}
	}

	text := clipRunes(b.String(), maxChars)
	var pinnedPtr *PinRenderResult
	if pinned.Text != "" {
		pinnedPtr = &pinned
	}
	return SessionPack{
		Session:     sess,
		Pinned:      pinnedPtr,
		Continuity:  cont,
		Handoff:     handoff,
		Checkpoints: checkpoints,
		Memos:       memos,
		Text:        text,
	}, nil
}

// CloseSession marks a session as closed.
func (s *SQLiteService) CloseSession(ctx context.Context, workspaceRoot, sessionID string) error {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return err
	}
	res, err := db.ExecContext(ctx, `UPDATE sessions SET status = 'closed', updated_at = ? WHERE id = ?`, s.nowISO(), sessionID)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	return nil
}

func (s *SQLiteService) listSessionCheckpoints(ctx context.Context, workspaceRoot, sessionID string, limit int) ([]Checkpoint, error) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, session_id, seq, work_item_id, status, summary, next_json, artifacts_json, verify_result, created_at
		FROM session_checkpoints
		WHERE session_id = ?
		ORDER BY seq DESC LIMIT ?
	`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Checkpoint
	for rows.Next() {
		var cp Checkpoint
		var nextJSON, artJSON string
		if err := rows.Scan(
			&cp.ID, &cp.SessionID, &cp.Seq, &cp.WorkItemID, &cp.Status, &cp.Summary,
			&nextJSON, &artJSON, &cp.VerifyResult, &cp.CreatedAt,
		); err != nil {
			return nil, err
		}
		cp.NextActions = unmarshalStringSlice(nextJSON)
		cp.Artifacts = unmarshalStringSlice(artJSON)
		out = append(out, cp)
	}
	return out, rows.Err()
}

func clipRunes(text string, maxChars int) string {
	if maxChars <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	return string(runes[:maxChars]) + "\n...(truncated)"
}
