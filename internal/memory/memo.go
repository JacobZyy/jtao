package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	agentPrivateTTL       = 30 * 24 * time.Hour
	matchScoreEpsilon     = 0.2
	defaultPinRenderChars = 2000
	maxMemoEntities       = 24
)

var validScopes = map[Scope]bool{
	ScopeProjectShared:  true,
	ScopeAgentPrivate:   true,
	ScopeAgentEphemeral: true,
}

var validClaimStatuses = map[ClaimStatus]bool{
	ClaimVerified:  true,
	ClaimCandidate: true,
	ClaimObserved:  true,
}

var validConfidences = map[string]bool{
	"":       true,
	"high":   true,
	"medium": true,
}

// WriteMemo validates, scans, indexes, and appends a memo event.
func (s *SQLiteService) WriteMemo(ctx context.Context, in WriteMemoInput) (WriteMemoResult, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return WriteMemoResult{}, fmt.Errorf("memo text is required")
	}

	scan := s.ScanContent(text)
	if scan.Blocked && !in.AllowSecretBypass {
		return WriteMemoResult{ScanFindings: scan.Findings}, fmt.Errorf("memo blocked by secret scanner: %s", scan.Findings[0].Detail)
	}

	in, err := normalizeWriteMemoInput(in)
	if err != nil {
		return WriteMemoResult{}, err
	}

	db, _, err := s.openDB(ctx, in.WorkspaceRoot)
	if err != nil {
		return WriteMemoResult{}, err
	}

	// Idempotent deduplication by (space, source_ref) before taking write tx.
	if in.SourceRef != "" {
		existing, err := s.findBySourceRef(ctx, db, in.Space, in.SourceRef)
		if err != nil {
			return WriteMemoResult{}, err
		}
		if existing != nil {
			return WriteMemoResult{
				Event:        *existing,
				Deduplicated: true,
				ScanFindings: scan.Findings,
			}, nil
		}
	}

	var hints []SupersedeHint
	if in.ReturnSupersedeHints && len(in.Supersedes) == 0 {
		hints, _ = s.findSupersedeHints(ctx, in.WorkspaceRoot, in.Space, in.Agent, text)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return WriteMemoResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	event, err := s.insertMemoTx(ctx, tx, in, "")
	if err != nil {
		return WriteMemoResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return WriteMemoResult{}, err
	}

	return WriteMemoResult{
		Event:          event,
		Deduplicated:   false,
		SupersedeHints: hints,
		ScanFindings:   scan.Findings,
	}, nil
}

func normalizeWriteMemoInput(in WriteMemoInput) (WriteMemoInput, error) {
	in.Text = strings.TrimSpace(in.Text)
	in.Space = normalizeSpace(in.Space)
	if in.Scope == "" {
		in.Scope = ScopeProjectShared
	}
	if !validScopes[in.Scope] {
		return in, fmt.Errorf("invalid memo scope: %q", in.Scope)
	}
	if in.ClaimStatus == "" {
		in.ClaimStatus = ClaimVerified
	}
	if !validClaimStatuses[in.ClaimStatus] {
		return in, fmt.Errorf("invalid claim status: %q", in.ClaimStatus)
	}
	in.Confidence = strings.ToLower(strings.TrimSpace(in.Confidence))
	if !validConfidences[in.Confidence] {
		return in, fmt.Errorf("invalid confidence %q: must be high, medium, or empty", in.Confidence)
	}
	if len(in.Entities) > maxMemoEntities {
		return in, fmt.Errorf("entities exceed limit of %d", maxMemoEntities)
	}
	in.EvidenceRef = strings.TrimSpace(in.EvidenceRef)
	in.WorkItemID = strings.TrimSpace(in.WorkItemID)
	in.Agent = strings.TrimSpace(in.Agent)
	in.SourceRef = strings.TrimSpace(in.SourceRef)
	if in.Provenance.ProducerType == "" {
		in.Provenance.ProducerType = "local_manual"
	}
	if in.Provenance.SourceHash == "" {
		in.Provenance.SourceHash = sha256Hex(in.Text)
	}
	return in, nil
}

func (s *SQLiteService) insertMemoTx(ctx context.Context, tx *sql.Tx, in WriteMemoInput, promotionOf string) (MemoEvent, error) {
	now := s.nowISO()
	validAt := strings.TrimSpace(in.ValidAt)
	if validAt == "" {
		validAt = now
	}

	if err := validateSupersedesTx(ctx, tx, in); err != nil {
		return MemoEvent{}, err
	}

	var nextSeq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM memo_events WHERE space = ?`, in.Space).Scan(&nextSeq); err != nil {
		return MemoEvent{}, fmt.Errorf("compute next memo seq: %w", err)
	}

	id := newPrefixedID("memo:" + in.Space)
	searchCorpus := buildMemoSearchCorpus(in.Text, in.Entities, in.Refs, in.WorkItemID)
	searchTokens := TokenizeForSearch(searchCorpus)
	provBytes, err := json.Marshal(in.Provenance)
	if err != nil {
		return MemoEvent{}, fmt.Errorf("marshal provenance: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO memo_events (
			id, seq, space, scope, agent, work_item_id, text, search_tokens,
			refs_json, entities_json, evidence_ref, confidence, claim_status,
			archived, promotion_of, provenance_json, valid_at, created_at, source_ref
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?)
	`,
		id, nextSeq, in.Space, string(in.Scope), in.Agent, in.WorkItemID, in.Text, searchTokens,
		marshalStringSlice(in.Refs), marshalStringSlice(in.Entities), in.EvidenceRef, in.Confidence, string(in.ClaimStatus),
		promotionOf, string(provBytes), validAt, now, in.SourceRef,
	)
	if err != nil {
		return MemoEvent{}, fmt.Errorf("insert memo_events: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO memo_fts (memo_id, search_tokens) VALUES (?, ?)`, id, searchTokens); err != nil {
		return MemoEvent{}, fmt.Errorf("insert memo_fts: %w", err)
	}

	for _, targetID := range in.Supersedes {
		targetID = strings.TrimSpace(targetID)
		if targetID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO memo_supersedes (source_id, target_id) VALUES (?, ?)`, id, targetID); err != nil {
			return MemoEvent{}, fmt.Errorf("insert memo_supersedes: %w", err)
		}
	}

	return MemoEvent{
		ID:          id,
		Seq:         nextSeq,
		Space:       in.Space,
		Scope:       in.Scope,
		Agent:       in.Agent,
		WorkItemID:  in.WorkItemID,
		Text:        in.Text,
		Refs:        in.Refs,
		Entities:    in.Entities,
		EvidenceRef: in.EvidenceRef,
		Confidence:  in.Confidence,
		ClaimStatus: in.ClaimStatus,
		Archived:    false,
		PromotionOf: promotionOf,
		Supersedes:  in.Supersedes,
		Provenance:  in.Provenance,
		ValidAt:     validAt,
		CreatedAt:   now,
		SourceRef:   in.SourceRef,
	}, nil
}

func validateSupersedesTx(ctx context.Context, tx *sql.Tx, in WriteMemoInput) error {
	for _, targetID := range in.Supersedes {
		targetID = strings.TrimSpace(targetID)
		if targetID == "" {
			continue
		}
		var targetSpace, targetScope, targetAgent string
		err := tx.QueryRowContext(ctx, `SELECT space, scope, agent FROM memo_events WHERE id = ?`, targetID).Scan(&targetSpace, &targetScope, &targetAgent)
		if err == sql.ErrNoRows {
			return fmt.Errorf("supersedes target not found: %s", targetID)
		}
		if err != nil {
			return err
		}
		if targetSpace != in.Space {
			return fmt.Errorf("cross-space supersede forbidden: target %s belongs to space %q, not %q", targetID, targetSpace, in.Space)
		}
		if Scope(targetScope) == ScopeAgentPrivate && targetAgent != "" && in.Agent != "" && targetAgent != in.Agent {
			return fmt.Errorf("cross-agent private supersede forbidden: target %s belongs to %q", targetID, targetAgent)
		}
	}
	return nil
}

func buildMemoSearchCorpus(text string, entities, refs []string, workItemID string) string {
	parts := []string{text}
	parts = append(parts, entities...)
	parts = append(parts, refs...)
	if workItemID != "" {
		parts = append(parts, workItemID)
	}
	return strings.Join(parts, " ")
}

func (s *SQLiteService) findBySourceRef(ctx context.Context, db *sql.DB, space, sourceRef string) (*MemoEvent, error) {
	row := db.QueryRowContext(ctx, memoSelectBase+` WHERE space = ? AND source_ref = ? ORDER BY seq DESC LIMIT 1`, space, sourceRef)
	ev, err := scanMemoRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ev.Supersedes, _ = loadSupersedes(ctx, db, ev.ID)
	return &ev, nil
}

func (s *SQLiteService) findSupersedeHints(ctx context.Context, workspaceRoot, space, agent, text string) ([]SupersedeHint, error) {
	hits, err := s.SearchMemos(ctx, SearchMemosInput{
		WorkspaceRoot: workspaceRoot,
		Query:         text,
		Space:         space,
		Agent:         agent,
		Limit:         3,
	})
	if err != nil || len(hits) == 0 {
		return nil, err
	}
	hints := make([]SupersedeHint, 0, len(hits))
	for _, h := range hits {
		hints = append(hints, SupersedeHint{
			ID:    h.Event.ID,
			Text:  h.Event.Text,
			Score: h.Score,
		})
	}
	return hints, nil
}

// GetMemo loads a single memo event by ID.
func (s *SQLiteService) GetMemo(ctx context.Context, workspaceRoot, memoID string) (*MemoEvent, error) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	row := db.QueryRowContext(ctx, memoSelectBase+` WHERE id = ?`, memoID)
	ev, err := scanMemoRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ev.Supersedes, _ = loadSupersedes(ctx, db, ev.ID)
	return &ev, nil
}

// ListMemos returns active (non-superseded, non-archived, non-expired) memos by default.
func (s *SQLiteService) ListMemos(ctx context.Context, in ListMemosInput) ([]MemoEvent, error) {
	db, _, err := s.openDB(ctx, in.WorkspaceRoot)
	if err != nil {
		return nil, err
	}

	var clauses []string
	var args []any

	if in.Space != "" {
		clauses = append(clauses, "m.space = ?")
		args = append(args, normalizeSpace(in.Space))
	}
	if in.Scope != "" {
		clauses = append(clauses, "m.scope = ?")
		args = append(args, string(in.Scope))
	}
	if in.WorkItemID != "" {
		clauses = append(clauses, "m.work_item_id = ?")
		args = append(args, strings.TrimSpace(in.WorkItemID))
	}
	if in.ClaimStatus != "" {
		clauses = append(clauses, "m.claim_status = ?")
		args = append(args, string(in.ClaimStatus))
	}
	if !in.IncludeArchived {
		clauses = append(clauses, "m.archived = 0")
	}
	if !in.IncludeSuperseded {
		clauses = append(clauses, "NOT EXISTS (SELECT 1 FROM memo_supersedes ms WHERE ms.target_id = m.id)")
	}
	if in.Agent != "" {
		clauses = append(clauses, "(m.scope = 'project_shared' OR m.agent = ? OR m.agent = '')")
		args = append(args, in.Agent)
	}

	query := memoSelectBase + " AS m"
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY m.created_at DESC, m.seq DESC"
	if in.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", in.Limit)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list memos: %w", err)
	}
	defer rows.Close()

	cutoff := s.now().Add(-agentPrivateTTL)
	var out []MemoEvent
	for rows.Next() {
		ev, err := scanMemoRow(rows)
		if err != nil {
			return nil, err
		}
		if !in.IncludeExpired && isExpiredPrivateMemo(ev, cutoff) {
			continue
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SearchMemos scores active memos using FTS5 BM25 + 1-hop entity spread + usefulness boost + recency tie-break.
func (s *SQLiteService) SearchMemos(ctx context.Context, in SearchMemosInput) ([]MemoSearchHit, error) {
	db, _, err := s.openDB(ctx, in.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 10
	}

	ftsQuery := BuildFTSQuery(in.Query)
	if ftsQuery == "" {
		return nil, nil
	}

	spaceFilter := ""
	if in.Space != "" {
		spaceFilter = normalizeSpace(in.Space)
	}

	// Step 1: FTS5 BM25 candidate retrieval over active memos.
	sqlStr := `
		SELECT m.id, m.seq, m.space, m.scope, m.agent, m.work_item_id, m.text,
		       m.refs_json, m.entities_json, m.evidence_ref, m.confidence, m.claim_status,
		       m.archived, m.promotion_of, m.provenance_json, m.valid_at, m.created_at, m.source_ref,
		       bm25(memo_fts) AS rank
		FROM memo_fts f
		JOIN memo_events m ON m.id = f.memo_id
		WHERE memo_fts MATCH ?
		  AND m.archived = 0
		  AND m.claim_status = 'verified'
		  AND NOT EXISTS (SELECT 1 FROM memo_supersedes ms WHERE ms.target_id = m.id)
	`
	args := []any{ftsQuery}
	if spaceFilter != "" {
		sqlStr += " AND m.space = ?"
		args = append(args, spaceFilter)
	}
	if strings.TrimSpace(in.WorkItemID) != "" {
		sqlStr += " AND m.work_item_id = ?"
		args = append(args, strings.TrimSpace(in.WorkItemID))
	}
	if strings.TrimSpace(in.Agent) != "" {
		sqlStr += " AND (m.scope = 'project_shared' OR m.agent = ? OR m.agent = '')"
		args = append(args, strings.TrimSpace(in.Agent))
	}
	sqlStr += " ORDER BY rank ASC LIMIT ?"
	args = append(args, limit*4)

	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("search memo_fts: %w", err)
	}

	cutoff := s.now().Add(-agentPrivateTTL)
	queryTerms := ExtractQueryTerms(in.Query)
	queryTermSet := make(map[string]struct{}, len(queryTerms))
	for _, t := range queryTerms {
		queryTermSet[t] = struct{}{}
	}

	byID := make(map[string]*MemoSearchHit)
	directEntities := make(map[string]struct{})

	for rows.Next() {
		ev, rank, err := scanMemoRowWithRank(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		if !in.IncludeAll && isExpiredPrivateMemo(ev, cutoff) {
			continue
		}
		// SQLite bm25() returns negative scores where more negative = better match.
		baseScore := 1.0 + math.Abs(rank)
		reason := "fts"
		for _, ent := range ev.Entities {
			normEnt := strings.ToLower(strings.TrimSpace(ent))
			if normEnt == "" {
				continue
			}
			directEntities[normEnt] = struct{}{}
			if _, ok := queryTermSet[normEnt]; ok {
				baseScore += 1.0
				reason = "fts+entity"
			}
		}
		byID[ev.ID] = &MemoSearchHit{
			Event:       ev,
			Score:       baseScore,
			MatchReason: reason,
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	// Step 2: 1-hop entity spread boost (+0.5) to active memos sharing entities with direct hits.
	if len(directEntities) > 0 {
		active, err := s.ListMemos(ctx, ListMemosInput{
			WorkspaceRoot: in.WorkspaceRoot,
			Space:         spaceFilter,
			Agent:         in.Agent,
			WorkItemID:    in.WorkItemID,
			ClaimStatus:   ClaimVerified,
			Limit:         200,
		})
		if err == nil {
			for _, ev := range active {
				matched := false
				for _, ent := range ev.Entities {
					if _, ok := directEntities[strings.ToLower(strings.TrimSpace(ent))]; ok {
						matched = true
						break
					}
				}
				if !matched {
					continue
				}
				if hit, exists := byID[ev.ID]; exists {
					hit.Score += 0.5
				} else {
					byID[ev.ID] = &MemoSearchHit{
						Event:       ev,
						Score:       0.5,
						MatchReason: "entity-spread",
					}
				}
			}
		}
	}

	if len(byID) == 0 {
		return nil, nil
	}

	// Step 3: Feedback usefulness boost from memo_feedback counts.
	feedbackCounts := loadFeedbackCounts(ctx, db)
	hits := make([]MemoSearchHit, 0, len(byID))
	for id, hit := range byID {
		if count := feedbackCounts[id]; count > 0 {
			hit.Score += math.Log1p(float64(count)) * 0.6
		}
		hits = append(hits, *hit)
	}

	// Step 4: Sort by score, breaking near-ties (< 0.2) by newer validAt/createdAt.
	sort.Slice(hits, func(i, j int) bool {
		diff := math.Abs(hits[i].Score - hits[j].Score)
		if diff < matchScoreEpsilon {
			if hits[i].Event.ValidAt != hits[j].Event.ValidAt {
				return hits[i].Event.ValidAt > hits[j].Event.ValidAt
			}
			return hits[i].Event.CreatedAt > hits[j].Event.CreatedAt
		}
		return hits[i].Score > hits[j].Score
	})

	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// MarkUseful records adoption feedback for recalled memos, optionally intersecting
// with an allowed exposure set.
func (s *SQLiteService) MarkUseful(ctx context.Context, in FeedbackInput) (FeedbackResult, error) {
	db, _, err := s.openDB(ctx, in.WorkspaceRoot)
	if err != nil {
		return FeedbackResult{}, err
	}

	var allowed map[string]struct{}
	if len(in.AllowedIDs) > 0 {
		allowed = make(map[string]struct{}, len(in.AllowedIDs))
		for _, id := range in.AllowedIDs {
			allowed[strings.TrimSpace(id)] = struct{}{}
		}
	}

	now := s.nowISO()
	var res FeedbackResult
	for _, rawID := range in.MemoIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if allowed != nil {
			if _, ok := allowed[id]; !ok {
				res.IgnoredIDs = append(res.IgnoredIDs, id)
				continue
			}
		}
		var exists int
		if err := db.QueryRowContext(ctx, `SELECT 1 FROM memo_events WHERE id = ?`, id).Scan(&exists); err != nil {
			res.IgnoredIDs = append(res.IgnoredIDs, id)
			continue
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO memo_feedback (memo_id, session_id, agent, turn_id, created_at)
			VALUES (?, ?, ?, ?, ?)
		`, id, in.SessionID, in.Agent, in.TurnID, now); err != nil {
			return res, fmt.Errorf("insert memo_feedback: %w", err)
		}
		res.AcceptedIDs = append(res.AcceptedIDs, id)
	}
	return res, nil
}

// PinGet retrieves the pinned content for a space.
func (s *SQLiteService) PinGet(ctx context.Context, workspaceRoot, space string) (*PinEntry, error) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	space = normalizeSpace(space)
	var entry PinEntry
	err = db.QueryRowContext(ctx, `SELECT space, content, content_hash, updated_at FROM pinned_entries WHERE space = ?`, space).
		Scan(&entry.Space, &entry.Content, &entry.ContentHash, &entry.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &entry, nil
}

// PinSet replaces a space's pinned content after running secret scanning.
func (s *SQLiteService) PinSet(ctx context.Context, workspaceRoot, space, content string) (PinEntry, error) {
	if scan := s.ScanContent(content); scan.Blocked {
		return PinEntry{}, fmt.Errorf("pinned content blocked by secret scanner: %s", scan.Findings[0].Detail)
	}
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return PinEntry{}, err
	}
	space = normalizeSpace(space)
	trimmed := strings.TrimSpace(content)
	entry := PinEntry{
		Space:       space,
		Content:     trimmed,
		ContentHash: sha256Hex(trimmed),
		UpdatedAt:   s.nowISO(),
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO pinned_entries (space, content, content_hash, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(space) DO UPDATE SET
			content = excluded.content,
			content_hash = excluded.content_hash,
			updated_at = excluded.updated_at
	`, entry.Space, entry.Content, entry.ContentHash, entry.UpdatedAt)
	if err != nil {
		return PinEntry{}, fmt.Errorf("upsert pinned_entries: %w", err)
	}
	return entry, nil
}

// PinAppend appends a new line to a space's pinned entries.
func (s *SQLiteService) PinAppend(ctx context.Context, workspaceRoot, space, line string) (PinEntry, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return PinEntry{}, fmt.Errorf("pin line is required")
	}
	existing, err := s.PinGet(ctx, workspaceRoot, space)
	if err != nil {
		return PinEntry{}, err
	}
	next := line
	if existing != nil && existing.Content != "" {
		next = existing.Content + "\n" + line
	}
	return s.PinSet(ctx, workspaceRoot, space, next)
}

// PinRender produces a window-capped pinned context block ready for session startup injection.
func (s *SQLiteService) PinRender(ctx context.Context, workspaceRoot, space string, maxChars int) (PinRenderResult, error) {
	space = normalizeSpace(space)
	if maxChars <= 0 {
		maxChars = defaultPinRenderChars
	}
	entry, err := s.PinGet(ctx, workspaceRoot, space)
	if err != nil || entry == nil || entry.Content == "" {
		return PinRenderResult{Space: space}, err
	}
	text := entry.Content
	truncated := false
	runes := []rune(text)
	if len(runes) > maxChars {
		text = string(runes[:maxChars]) + "\n...(truncated)"
		truncated = true
	}
	return PinRenderResult{
		Space:       space,
		Text:        "## Pinned Project Memory\n\n" + text,
		ContentHash: entry.ContentHash,
		Truncated:   truncated,
	}, nil
}

const memoSelectBase = `
SELECT id, seq, space, scope, agent, work_item_id, text,
       refs_json, entities_json, evidence_ref, confidence, claim_status,
       archived, promotion_of, provenance_json, valid_at, created_at, source_ref
FROM memo_events`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanMemoRow(row rowScanner) (MemoEvent, error) {
	var ev MemoEvent
	var scopeStr, statusStr, refsJSON, entitiesJSON, provJSON string
	var archivedInt int
	err := row.Scan(
		&ev.ID, &ev.Seq, &ev.Space, &scopeStr, &ev.Agent, &ev.WorkItemID, &ev.Text,
		&refsJSON, &entitiesJSON, &ev.EvidenceRef, &ev.Confidence, &statusStr,
		&archivedInt, &ev.PromotionOf, &provJSON, &ev.ValidAt, &ev.CreatedAt, &ev.SourceRef,
	)
	if err != nil {
		return MemoEvent{}, err
	}
	ev.Scope = Scope(scopeStr)
	ev.ClaimStatus = ClaimStatus(statusStr)
	ev.Archived = archivedInt != 0
	ev.Refs = unmarshalStringSlice(refsJSON)
	ev.Entities = unmarshalStringSlice(entitiesJSON)
	_ = json.Unmarshal([]byte(provJSON), &ev.Provenance)
	return ev, nil
}

func scanMemoRowWithRank(row rowScanner) (MemoEvent, float64, error) {
	var ev MemoEvent
	var scopeStr, statusStr, refsJSON, entitiesJSON, provJSON string
	var archivedInt int
	var rank float64
	err := row.Scan(
		&ev.ID, &ev.Seq, &ev.Space, &scopeStr, &ev.Agent, &ev.WorkItemID, &ev.Text,
		&refsJSON, &entitiesJSON, &ev.EvidenceRef, &ev.Confidence, &statusStr,
		&archivedInt, &ev.PromotionOf, &provJSON, &ev.ValidAt, &ev.CreatedAt, &ev.SourceRef,
		&rank,
	)
	if err != nil {
		return MemoEvent{}, 0, err
	}
	ev.Scope = Scope(scopeStr)
	ev.ClaimStatus = ClaimStatus(statusStr)
	ev.Archived = archivedInt != 0
	ev.Refs = unmarshalStringSlice(refsJSON)
	ev.Entities = unmarshalStringSlice(entitiesJSON)
	_ = json.Unmarshal([]byte(provJSON), &ev.Provenance)
	return ev, rank, nil
}

func loadSupersedes(ctx context.Context, db *sql.DB, sourceID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT target_id FROM memo_supersedes WHERE source_id = ?`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return nil, err
		}
		ids = append(ids, target)
	}
	return ids, rows.Err()
}

func loadFeedbackCounts(ctx context.Context, db *sql.DB) map[string]int {
	counts := make(map[string]int)
	rows, err := db.QueryContext(ctx, `SELECT memo_id, COUNT(*) FROM memo_feedback GROUP BY memo_id`)
	if err != nil {
		return counts
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var cnt int
		if err := rows.Scan(&id, &cnt); err == nil {
			counts[id] = cnt
		}
	}
	return counts
}

func isExpiredPrivateMemo(ev MemoEvent, cutoff time.Time) bool {
	if ev.Scope != ScopeAgentPrivate {
		return false
	}
	createdAt, err := time.Parse(time.RFC3339Nano, ev.CreatedAt)
	if err != nil {
		return false
	}
	return createdAt.Before(cutoff)
}
