package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// RecallQuery searches across unified memory sources (memo, checkpoint, event)
// and always preserves each record's real ID for downstream feedback or supersede calls.
func (s *SQLiteService) RecallQuery(ctx context.Context, in RecallQueryInput) ([]RecallHit, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 8
	}
	sources := normalizeRecallSources(in.Sources)

	var hits []RecallHit

	if sources["memo"] {
		memoHits, err := s.SearchMemos(ctx, SearchMemosInput{
			WorkspaceRoot: in.WorkspaceRoot,
			Query:         in.Query,
			Space:         in.Space,
			Agent:         in.Agent,
			WorkItemID:    in.WorkItemID,
			Limit:         limit,
		})
		if err != nil {
			return nil, err
		}
		for _, mh := range memoHits {
			hits = append(hits, RecallHit{
				ID:         mh.Event.ID,
				Source:     "memo",
				Text:       mh.Event.Text,
				Score:      mh.Score,
				Refs:       mh.Event.Refs,
				WorkItemID: mh.Event.WorkItemID,
				CreatedAt:  mh.Event.CreatedAt,
			})
		}
	}

	ftsQuery := BuildFTSQuery(in.Query)
	if ftsQuery != "" && (sources["checkpoint"] || sources["event"]) {
		db, _, err := s.openDB(ctx, in.WorkspaceRoot)
		if err != nil {
			return nil, err
		}

		if sources["checkpoint"] {
			cpHits, err := searchCheckpointsFTS(ctx, db, ftsQuery, in.WorkItemID, limit)
			if err != nil {
				return nil, err
			}
			hits = append(hits, cpHits...)
		}
		if sources["event"] {
			evHits, err := searchEventsFTS(ctx, db, ftsQuery, in.WorkItemID, limit)
			if err != nil {
				return nil, err
			}
			hits = append(hits, evHits...)
		}
	}

	sort.Slice(hits, func(i, j int) bool {
		if math.Abs(hits[i].Score-hits[j].Score) < matchScoreEpsilon {
			return hits[i].CreatedAt > hits[j].CreatedAt
		}
		return hits[i].Score > hits[j].Score
	})

	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// RecallForTurn executes hit-gated turn recall for prompt injection.
// Unlike AIOS's hook bug, 0 hits ALWAYS produces empty Text so no noise is injected.
func (s *SQLiteService) RecallForTurn(ctx context.Context, in TurnRecallInput) (TurnRecallResult, error) {
	if strings.TrimSpace(in.Message) == "" {
		return TurnRecallResult{Status: "skipped"}, nil
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 5
	}
	budget := in.BudgetChars
	if budget <= 0 {
		budget = 1600
	}

	hits, err := s.RecallQuery(ctx, RecallQueryInput{
		WorkspaceRoot: in.WorkspaceRoot,
		Query:         in.Message,
		Sources:       []string{"memo", "checkpoint"},
		Space:         in.Space,
		Agent:         in.Agent,
		WorkItemID:    in.WorkItemID,
		Limit:         limit,
	})
	if err != nil {
		return TurnRecallResult{}, err
	}
	if len(hits) == 0 {
		return TurnRecallResult{Status: "no-hits", Hits: 0, Text: ""}, nil
	}

	var b strings.Builder
	b.WriteString("## Recalled Project Memory\n")
	var kept []RecallHit
	var exposedMemoIDs []string

	for _, hit := range hits {
		line := fmt.Sprintf("- [%s|%s] %s\n", hit.Source, hit.ID, strings.TrimSpace(hit.Text))
		if b.Len()+len(line) > budget && len(kept) > 0 {
			break
		}
		b.WriteString(line)
		kept = append(kept, hit)
		if hit.Source == "memo" {
			exposedMemoIDs = append(exposedMemoIDs, hit.ID)
		}
	}

	if in.SessionID != "" && len(exposedMemoIDs) > 0 {
		s.recordExposures(ctx, in.WorkspaceRoot, in.SessionID, exposedMemoIDs)
	}

	return TurnRecallResult{
		Status:     "ok",
		Hits:       len(kept),
		Items:      kept,
		ExposedIDs: exposedMemoIDs,
		Text:       strings.TrimSpace(b.String()),
	}, nil
}

// RecallForWorkItem collects pinned rules, memos, checkpoints, continuity, and handoff tied to a workItemId.
func (s *SQLiteService) RecallForWorkItem(ctx context.Context, workspaceRoot, workItemID string, maxChars int) (WorkItemRecallResult, error) {
	workItemID = strings.TrimSpace(workItemID)
	if workItemID == "" {
		return WorkItemRecallResult{}, fmt.Errorf("workItemId is required")
	}
	if maxChars <= 0 {
		maxChars = 4000
	}

	memos, err := s.ListMemos(ctx, ListMemosInput{
		WorkspaceRoot: workspaceRoot,
		WorkItemID:    workItemID,
		ClaimStatus:   ClaimVerified,
		Limit:         12,
	})
	if err != nil {
		return WorkItemRecallResult{}, err
	}

	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return WorkItemRecallResult{}, err
	}

	checkpoints, err := listWorkItemCheckpoints(ctx, db, workItemID, 6)
	if err != nil {
		return WorkItemRecallResult{}, err
	}
	cont := latestWorkItemContinuity(ctx, db, workItemID)
	handoff := latestWorkItemHandoff(ctx, db, workItemID)
	pinned, _ := s.PinRender(ctx, workspaceRoot, defaultSpace, maxChars/3)

	var b strings.Builder
	fmt.Fprintf(&b, "## WorkItem Memory (%s)\n", workItemID)
	if pinned.Text != "" {
		b.WriteString("\n" + pinned.Text + "\n")
	}
	if len(memos) > 0 {
		b.WriteString("\n### Memos\n")
		for _, m := range memos {
			fmt.Fprintf(&b, "- [%s] %s\n", m.ID, m.Text)
		}
	}
	if cont != nil && cont.Summary != "" {
		b.WriteString("\n### Latest Continuity\n" + cont.Summary + "\n")
	}
	if handoff != nil && handoff.Summary != "" {
		fmt.Fprintf(&b, "\n### Latest Handoff (%s)\n%s\n", handoff.Status, handoff.Summary)
	}
	if len(checkpoints) > 0 {
		b.WriteString("\n### Recent Checkpoints\n")
		for _, cp := range checkpoints {
			fmt.Fprintf(&b, "- (%s) %s\n", cp.Status, cp.Summary)
		}
	}

	var pinnedPtr *PinRenderResult
	if pinned.Text != "" {
		pinnedPtr = &pinned
	}
	return WorkItemRecallResult{
		WorkItemID:  workItemID,
		Pinned:      pinnedPtr,
		Memos:       memos,
		Checkpoints: checkpoints,
		Continuity:  cont,
		Handoff:     handoff,
		Text:        clipRunes(b.String(), maxChars),
	}, nil
}

func (s *SQLiteService) recordExposures(ctx context.Context, workspaceRoot, sessionID string, memoIDs []string) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return
	}
	now := s.nowISO()
	for _, id := range memoIDs {
		_, _ = db.ExecContext(ctx, `INSERT INTO memo_exposures (session_id, memo_id, created_at) VALUES (?, ?, ?)`, sessionID, id, now)
	}
}

func normalizeRecallSources(sources []string) map[string]bool {
	if len(sources) == 0 {
		return map[string]bool{"memo": true, "checkpoint": true}
	}
	out := make(map[string]bool, len(sources))
	for _, src := range sources {
		out[strings.ToLower(strings.TrimSpace(src))] = true
	}
	return out
}

func searchCheckpointsFTS(ctx context.Context, db queryable, ftsQuery, workItemID string, limit int) ([]RecallHit, error) {
	sqlStr := `
		SELECT c.id, c.summary, c.artifacts_json, c.work_item_id, c.created_at, bm25(session_checkpoints_fts) AS rank
		FROM session_checkpoints_fts f
		JOIN session_checkpoints c ON c.id = f.checkpoint_id
		WHERE session_checkpoints_fts MATCH ?
	`
	args := []any{ftsQuery}
	if strings.TrimSpace(workItemID) != "" {
		sqlStr += " AND c.work_item_id = ?"
		args = append(args, strings.TrimSpace(workItemID))
	}
	sqlStr += " ORDER BY rank ASC LIMIT ?"
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RecallHit
	for rows.Next() {
		var id, summary, artJSON, wi, createdAt string
		var rank float64
		if err := rows.Scan(&id, &summary, &artJSON, &wi, &createdAt, &rank); err != nil {
			return nil, err
		}
		out = append(out, RecallHit{
			ID:         id,
			Source:     "checkpoint",
			Text:       summary,
			Score:      0.8 + math.Abs(rank)*0.8,
			Refs:       unmarshalStringSlice(artJSON),
			WorkItemID: wi,
			CreatedAt:  createdAt,
		})
	}
	return out, rows.Err()
}

func searchEventsFTS(ctx context.Context, db queryable, ftsQuery, workItemID string, limit int) ([]RecallHit, error) {
	sqlStr := `
		SELECT e.id, e.text, e.refs_json, e.work_item_id, e.created_at, bm25(session_events_fts) AS rank
		FROM session_events_fts f
		JOIN session_events e ON e.id = f.event_id
		WHERE session_events_fts MATCH ?
	`
	args := []any{ftsQuery}
	if strings.TrimSpace(workItemID) != "" {
		sqlStr += " AND e.work_item_id = ?"
		args = append(args, strings.TrimSpace(workItemID))
	}
	sqlStr += " ORDER BY rank ASC LIMIT ?"
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RecallHit
	for rows.Next() {
		var id, text, refsJSON, wi, createdAt string
		var rank float64
		if err := rows.Scan(&id, &text, &refsJSON, &wi, &createdAt, &rank); err != nil {
			return nil, err
		}
		out = append(out, RecallHit{
			ID:         id,
			Source:     "event",
			Text:       clipRunes(text, 400),
			Score:      0.6 + math.Abs(rank)*0.6,
			Refs:       unmarshalStringSlice(refsJSON),
			WorkItemID: wi,
			CreatedAt:  createdAt,
		})
	}
	return out, rows.Err()
}

func listWorkItemCheckpoints(ctx context.Context, db queryable, workItemID string, limit int) ([]Checkpoint, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, session_id, seq, work_item_id, status, summary, next_json, artifacts_json, verify_result, created_at
		FROM session_checkpoints
		WHERE work_item_id = ?
		ORDER BY created_at DESC LIMIT ?
	`, workItemID, limit)
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

func latestWorkItemContinuity(ctx context.Context, db queryable, workItemID string) *Continuity {
	rows, err := db.QueryContext(ctx, `
		SELECT session_id, work_item_id, intent, summary, touched_json, next_json, updated_at
		FROM session_continuity WHERE work_item_id = ?
		ORDER BY updated_at DESC LIMIT 1
	`, workItemID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	if !rows.Next() {
		return nil
	}
	var c Continuity
	var touchedJSON, nextJSON string
	if err := rows.Scan(&c.SessionID, &c.WorkItemID, &c.Intent, &c.Summary, &touchedJSON, &nextJSON, &c.UpdatedAt); err != nil {
		return nil
	}
	c.TouchedFiles = unmarshalStringSlice(touchedJSON)
	c.NextActions = unmarshalStringSlice(nextJSON)
	return &c
}

func latestWorkItemHandoff(ctx context.Context, db queryable, workItemID string) *HandoffPacket {
	rows, err := db.QueryContext(ctx, `
		SELECT packet_json FROM session_handoffs WHERE work_item_id = ?
		ORDER BY updated_at DESC LIMIT 1
	`, workItemID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	if !rows.Next() {
		return nil
	}
	var raw string
	if err := rows.Scan(&raw); err != nil {
		return nil
	}
	var h HandoffPacket
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		return nil
	}
	return &h
}

type queryable interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sqlRows, error)
}
