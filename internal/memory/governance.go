package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type scanRule struct {
	id       string
	severity string
	pattern  *regexp.Regexp
	detail   string
}

var contentScanRules = []scanRule{
	{
		id:       "secret-private-key",
		severity: "block",
		pattern:  regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
		detail:   "private key block detected",
	},
	{
		id:       "secret-openai-key",
		severity: "block",
		pattern:  regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
		detail:   "API secret key pattern (sk-*) detected",
	},
	{
		id:       "secret-github-token",
		severity: "block",
		pattern:  regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b`),
		detail:   "GitHub access token detected",
	},
	{
		id:       "secret-aws-key",
		severity: "block",
		pattern:  regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		detail:   "AWS access key ID detected",
	},
	{
		id:       "secret-bearer-token",
		severity: "block",
		pattern:  regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._-]{24,}\b`),
		detail:   "hardcoded Bearer token detected",
	},
	{
		id:       "prompt-injection-tag",
		severity: "warn",
		pattern:  regexp.MustCompile(`(?i)</?(?:system|assistant|tool_call|instructions?)\b`),
		detail:   "prompt-like XML tag detected in memory content",
	},
}

// ScanContent checks text for leaked credentials or prompt injection tags.
func (s *SQLiteService) ScanContent(text string) ScanResult {
	var findings []ScanFinding
	blocked := false
	for _, rule := range contentScanRules {
		if !rule.pattern.MatchString(text) {
			continue
		}
		findings = append(findings, ScanFinding{
			RuleID:   rule.id,
			Severity: rule.severity,
			Detail:   rule.detail,
		})
		if rule.severity == "block" {
			blocked = true
		}
	}
	return ScanResult{Blocked: blocked, Findings: findings}
}

// ListCandidates returns unpromoted candidate memos waiting for review.
func (s *SQLiteService) ListCandidates(ctx context.Context, workspaceRoot, space string, limit int) ([]MemoEvent, error) {
	return s.ListMemos(ctx, ListMemosInput{
		WorkspaceRoot: workspaceRoot,
		Space:         space,
		ClaimStatus:   ClaimCandidate,
		Limit:         limit,
	})
}

// PromoteCandidate explicitly promotes a candidate memo into a verified entry
// and records an auditable governance receipt.
func (s *SQLiteService) PromoteCandidate(ctx context.Context, workspaceRoot, candidateID, actor, note string) (MemoEvent, error) {
	cand, err := s.GetMemo(ctx, workspaceRoot, candidateID)
	if err != nil {
		return MemoEvent{}, err
	}
	if cand == nil {
		return MemoEvent{}, fmt.Errorf("candidate memo not found: %s", candidateID)
	}
	if cand.ClaimStatus != ClaimCandidate {
		return MemoEvent{}, fmt.Errorf("memo %s is not a candidate (status=%s)", candidateID, cand.ClaimStatus)
	}
	if strings.TrimSpace(actor) == "" {
		actor = "local_manual"
	}

	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return MemoEvent{}, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return MemoEvent{}, err
	}
	defer func() { _ = tx.Rollback() }()

	promoted, err := s.insertMemoTx(ctx, tx, WriteMemoInput{
		WorkspaceRoot: workspaceRoot,
		Text:          cand.Text,
		Space:         cand.Space,
		Scope:         ScopeProjectShared,
		Agent:         cand.Agent,
		WorkItemID:    cand.WorkItemID,
		Refs:          cand.Refs,
		Entities:      cand.Entities,
		EvidenceRef:   cand.EvidenceRef,
		Confidence:    cand.Confidence,
		ClaimStatus:   ClaimVerified,
		Supersedes:    []string{cand.ID},
		ValidAt:       cand.ValidAt,
		Provenance: Provenance{
			ProducerType: "candidate_promotion",
			PrincipalID:  actor,
			SourceRef:    cand.ID,
		},
	}, cand.ID)
	if err != nil {
		return MemoEvent{}, err
	}

	if err := s.recordGovernanceReceiptTx(ctx, tx, "promote_candidate", cand.ID, actor, note, map[string]string{
		"promotedMemoId": promoted.ID,
	}); err != nil {
		return MemoEvent{}, err
	}

	if err := tx.Commit(); err != nil {
		return MemoEvent{}, err
	}
	return promoted, nil
}

// RejectCandidate archives a candidate memo and records an audit receipt.
func (s *SQLiteService) RejectCandidate(ctx context.Context, workspaceRoot, candidateID, actor, note string) error {
	cand, err := s.GetMemo(ctx, workspaceRoot, candidateID)
	if err != nil {
		return err
	}
	if cand == nil {
		return fmt.Errorf("candidate memo not found: %s", candidateID)
	}
	if cand.ClaimStatus != ClaimCandidate {
		return fmt.Errorf("memo %s is not a candidate (status=%s)", candidateID, cand.ClaimStatus)
	}
	return s.ArchiveMemo(ctx, workspaceRoot, candidateID, actor, note, true)
}

// ArchiveMemo marks or unmarks a memo as archived with a governance audit row.
func (s *SQLiteService) ArchiveMemo(ctx context.Context, workspaceRoot, memoID, actor, note string, archived bool) error {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return err
	}
	if strings.TrimSpace(actor) == "" {
		actor = "local_manual"
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	flag := 0
	action := "unarchive_memo"
	if archived {
		flag = 1
		action = "archive_memo"
	}
	res, err := tx.ExecContext(ctx, `UPDATE memo_events SET archived = ? WHERE id = ?`, flag, memoID)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("memo not found: %s", memoID)
	}

	if err := s.recordGovernanceReceiptTx(ctx, tx, action, memoID, actor, note, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteService) recordGovernanceReceiptTx(ctx context.Context, tx *sql.Tx, action, targetID, actor, note string, detail any) error {
	detailJSON := "{}"
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			detailJSON = string(b)
		}
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO governance_receipts (id, action, target_id, actor, note, detail_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, newPrefixedID("gov"), action, targetID, actor, note, detailJSON, s.nowISO())
	return err
}
