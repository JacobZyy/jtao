package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryWriteSearchSupersedeAndWorkspaceIsolation(t *testing.T) {
	ctx := context.Background()
	svc := NewService()
	defer svc.Close()

	wsA := t.TempDir()
	wsB := t.TempDir()

	// 1. Write Chinese + English technical memo with entities & evidenceRef.
	first, err := svc.WriteMemo(ctx, WriteMemoInput{
		WorkspaceRoot: wsA,
		Text:          "结账接口 checkout.json 在金额小于等于 0 时必须返回 400 InvalidAmount",
		WorkItemID:    "feat-checkout",
		Entities:      []string{"checkout.json", "InvalidAmount"},
		EvidenceRef:   "file:internal/checkout/handler.go",
		Confidence:    "high",
		SourceRef:     "seed:1",
	})
	if err != nil {
		t.Fatalf("WriteMemo first failed: %v", err)
	}
	if first.Deduplicated {
		t.Fatalf("expected fresh write, got deduplicated")
	}

	// Idempotent duplicate by source_ref.
	dup, err := svc.WriteMemo(ctx, WriteMemoInput{
		WorkspaceRoot: wsA,
		Text:          "duplicate text ignored",
		SourceRef:     "seed:1",
	})
	if err != nil {
		t.Fatalf("WriteMemo duplicate failed: %v", err)
	}
	if !dup.Deduplicated || dup.Event.ID != first.Event.ID {
		t.Fatalf("expected deduplicated hit %s, got %+v", first.Event.ID, dup)
	}

	// 2. Search in Chinese and English returns the memo with its real ID.
	hits, err := svc.SearchMemos(ctx, SearchMemosInput{
		WorkspaceRoot: wsA,
		Query:         "结账金额 checkout.json",
		Limit:         5,
	})
	if err != nil {
		t.Fatalf("SearchMemos failed: %v", err)
	}
	if len(hits) != 1 || hits[0].Event.ID != first.Event.ID {
		t.Fatalf("expected 1 hit %s, got %+v", first.Event.ID, hits)
	}

	// 3. Cross-workspace isolation: wsB must NOT see wsA's memo.
	hitsB, err := svc.SearchMemos(ctx, SearchMemosInput{
		WorkspaceRoot: wsB,
		Query:         "结账金额 checkout.json",
		Limit:         5,
	})
	if err != nil {
		t.Fatalf("SearchMemos wsB failed: %v", err)
	}
	if len(hitsB) != 0 {
		t.Fatalf("expected 0 hits in isolated workspace B, got %d", len(hitsB))
	}

	// 4. Supersede folds the older memo out of active queries.
	second, err := svc.WriteMemo(ctx, WriteMemoInput{
		WorkspaceRoot: wsA,
		Text:          "结账接口 checkout.json 在金额小于等于 0 时改为返回 422 UnprocessableAmount",
		WorkItemID:    "feat-checkout",
		Entities:      []string{"checkout.json", "UnprocessableAmount"},
		Supersedes:    []string{first.Event.ID},
	})
	if err != nil {
		t.Fatalf("WriteMemo supersede failed: %v", err)
	}

	activeHits, err := svc.SearchMemos(ctx, SearchMemosInput{
		WorkspaceRoot: wsA,
		Query:         "checkout.json 结账",
		Limit:         5,
	})
	if err != nil {
		t.Fatalf("SearchMemos after supersede failed: %v", err)
	}
	if len(activeHits) != 1 || activeHits[0].Event.ID != second.Event.ID {
		t.Fatalf("expected only superseding memo %s, got %+v", second.Event.ID, activeHits)
	}

	// 5. Useful feedback with allowedIds intersection.
	fb, err := svc.MarkUseful(ctx, FeedbackInput{
		WorkspaceRoot: wsA,
		MemoIDs:       []string{second.Event.ID, "memo:default:unknown"},
		AllowedIDs:    []string{second.Event.ID},
		SessionID:     "sess-1",
	})
	if err != nil {
		t.Fatalf("MarkUseful failed: %v", err)
	}
	if len(fb.AcceptedIDs) != 1 || fb.AcceptedIDs[0] != second.Event.ID {
		t.Fatalf("unexpected feedback result: %+v", fb)
	}
	if len(fb.IgnoredIDs) != 1 {
		t.Fatalf("expected 1 ignored id, got %+v", fb.IgnoredIDs)
	}
}

func TestSecretScannerAndCandidatePromotion(t *testing.T) {
	ctx := context.Background()
	svc := NewService()
	defer svc.Close()
	ws := t.TempDir()

	// Secret must be rejected.
	_, err := svc.WriteMemo(ctx, WriteMemoInput{
		WorkspaceRoot: ws,
		Text:          "use key sk-proj-1234567890abcdefghijklmnop for auth",
	})
	if err == nil || !strings.Contains(err.Error(), "secret scanner") {
		t.Fatalf("expected secret scanner block, got %v", err)
	}

	// Candidate write -> not returned in verified search -> promote -> searchable.
	cand, err := svc.WriteMemo(ctx, WriteMemoInput{
		WorkspaceRoot: ws,
		Text:          "缓存失效策略优先按租户前缀批量清理",
		ClaimStatus:   ClaimCandidate,
	})
	if err != nil {
		t.Fatalf("write candidate failed: %v", err)
	}
	hits, err := svc.SearchMemos(ctx, SearchMemosInput{WorkspaceRoot: ws, Query: "缓存失效"})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("unpromoted candidate should not appear in verified search")
	}

	promoted, err := svc.PromoteCandidate(ctx, ws, cand.Event.ID, "jacob", "confirmed in review")
	if err != nil {
		t.Fatalf("PromoteCandidate failed: %v", err)
	}
	if promoted.ClaimStatus != ClaimVerified || promoted.PromotionOf != cand.Event.ID {
		t.Fatalf("unexpected promoted memo: %+v", promoted)
	}

	hitsAfter, err := svc.SearchMemos(ctx, SearchMemosInput{WorkspaceRoot: ws, Query: "缓存失效"})
	if err != nil || len(hitsAfter) != 1 || hitsAfter[0].Event.ID != promoted.ID {
		t.Fatalf("expected promoted memo in search, got err=%v hits=%+v", err, hitsAfter)
	}
}

func TestTurnRecallZeroHitSilenceAndWorkItemRecall(t *testing.T) {
	ctx := context.Background()
	svc := NewService()
	defer svc.Close()
	ws := t.TempDir()

	// 0 hits must return completely empty Text (fixes AIOS UserPromptSubmit empty-block bug).
	zero, err := svc.RecallForTurn(ctx, TurnRecallInput{
		WorkspaceRoot: ws,
		Message:       "完全不存在的关键词 xyz999",
	})
	if err != nil {
		t.Fatalf("RecallForTurn failed: %v", err)
	}
	if zero.Hits != 0 || zero.Text != "" || zero.Status != "no-hits" {
		t.Fatalf("expected silent no-hits recall, got %+v", zero)
	}

	// Pin + Session + Checkpoint + WorkItem recall + Artifact offload.
	if _, err := svc.PinAppend(ctx, ws, defaultSpace, "- Always run go test before claiming done."); err != nil {
		t.Fatalf("PinAppend failed: %v", err)
	}
	sess, err := svc.CreateSession(ctx, ws, "codex", "jtao", "build memory core", "wi-memory")
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	if _, err := svc.AddCheckpoint(ctx, ws, Checkpoint{
		SessionID:  sess.ID,
		WorkItemID: "wi-memory",
		Status:     "running",
		Summary:    "完成 FTS5 中英文分词与双时态折叠",
	}); err != nil {
		t.Fatalf("AddCheckpoint failed: %v", err)
	}
	if _, err := svc.WriteMemo(ctx, WriteMemoInput{
		WorkspaceRoot: ws,
		Text:          "Memory SQLite 使用 WAL 模式与 modernc.org/sqlite 零 CGO 驱动",
		WorkItemID:    "wi-memory",
	}); err != nil {
		t.Fatalf("WriteMemo failed: %v", err)
	}

	wiRecall, err := svc.RecallForWorkItem(ctx, ws, "wi-memory", 4000)
	if err != nil {
		t.Fatalf("RecallForWorkItem failed: %v", err)
	}
	if len(wiRecall.Memos) != 1 || len(wiRecall.Checkpoints) != 1 || wiRecall.Pinned == nil {
		t.Fatalf("incomplete workItem recall: %+v", wiRecall)
	}

	// Turn recall now hits and records exposures.
	hitTurn, err := svc.RecallForTurn(ctx, TurnRecallInput{
		WorkspaceRoot: ws,
		Message:       "SQLite WAL 驱动",
		SessionID:     sess.ID,
	})
	if err != nil {
		t.Fatalf("RecallForTurn hit failed: %v", err)
	}
	if hitTurn.Hits == 0 || hitTurn.Text == "" || len(hitTurn.ExposedIDs) == 0 {
		t.Fatalf("expected non-empty turn recall, got %+v", hitTurn)
	}

	// Artifact store + paginated read.
	large := strings.Repeat("log line with detail\n", 200)
	art, err := svc.StoreArtifact(ctx, ws, sess.ID, "wi-memory", "go-test", large)
	if err != nil {
		t.Fatalf("StoreArtifact failed: %v", err)
	}
	page1, err := svc.ReadArtifact(ctx, ws, art.ID, 0, 100)
	if err != nil || len(page1.Content) != 100 || !page1.HasMore {
		t.Fatalf("unexpected artifact page1: err=%v res=%+v", err, page1)
	}

	// Doctor + RebuildIndex.
	doc, err := svc.Doctor(ctx, ws)
	if err != nil || !doc.OK || !doc.FTS5Available {
		t.Fatalf("Doctor failed: err=%v report=%+v", err, doc)
	}
	if err := svc.RebuildIndex(ctx, ws); err != nil {
		t.Fatalf("RebuildIndex failed: %v", err)
	}
}

func TestAgentPrivateTTLAndEntitySpread(t *testing.T) {
	ctx := context.Background()
	svc := NewService()
	defer svc.Close()
	ws := t.TempDir()

	// Write an expired private memo 31 days ago.
	svc.now = func() time.Time { return time.Now().UTC().Add(-31 * 24 * time.Hour) }
	if _, err := svc.WriteMemo(ctx, WriteMemoInput{
		WorkspaceRoot: ws,
		Text:          "临时私有笔记 alpha-token",
		Scope:         ScopeAgentPrivate,
		Agent:         "codex",
	}); err != nil {
		t.Fatalf("write private memo failed: %v", err)
	}

	// Return clock to now: private memo must be filtered by 30-day read TTL.
	svc.now = func() time.Time { return time.Now().UTC() }
	expiredHits, err := svc.SearchMemos(ctx, SearchMemosInput{
		WorkspaceRoot: ws,
		Query:         "alpha-token",
		Agent:         "codex",
	})
	if err != nil {
		t.Fatalf("search expired failed: %v", err)
	}
	if len(expiredHits) != 0 {
		t.Fatalf("expected 30-day expired private memo to be filtered, got %+v", expiredHits)
	}

	// 1-hop entity spread: memo2 shares entity "order-service" with direct hit memo1.
	m1, err := svc.WriteMemo(ctx, WriteMemoInput{
		WorkspaceRoot: ws,
		Text:          "支付回调入口位于 payment-webhook",
		Entities:      []string{"order-service"},
	})
	if err != nil {
		t.Fatalf("write m1 failed: %v", err)
	}
	m2, err := svc.WriteMemo(ctx, WriteMemoInput{
		WorkspaceRoot: ws,
		Text:          "库存扣减必须保持幂等锁",
		Entities:      []string{"order-service"},
	})
	if err != nil {
		t.Fatalf("write m2 failed: %v", err)
	}

	spreadHits, err := svc.SearchMemos(ctx, SearchMemosInput{
		WorkspaceRoot: ws,
		Query:         "payment-webhook",
		Limit:         5,
	})
	if err != nil {
		t.Fatalf("spread search failed: %v", err)
	}
	if len(spreadHits) != 2 || spreadHits[0].Event.ID != m1.Event.ID || spreadHits[1].Event.ID != m2.Event.ID {
		t.Fatalf("expected direct hit %s + spread hit %s, got %+v", m1.Event.ID, m2.Event.ID, spreadHits)
	}
}

func TestArtifactGrepAndMemoryHygiene(t *testing.T) {
	ctx := context.Background()
	svc := NewService()
	defer svc.Close()
	ws := t.TempDir()

	art, err := svc.StoreArtifact(ctx, ws, "sess-1", "wi-1", "test", "line 1 ok\nERROR: database timeout\nline 3 ok\nERROR: retry failed\n")
	if err != nil {
		t.Fatal(err)
	}
	matches, err := svc.GrepArtifact(ctx, ws, art.ID, "ERROR:", 10)
	if err != nil || len(matches) != 2 || matches[0].LineNumber != 2 || matches[1].LineNumber != 4 {
		t.Fatalf("unexpected grep matches: err=%v matches=%+v", err, matches)
	}

	// Create a stale session 20 days ago and an orphan artifact file on disk.
	svc.now = func() time.Time { return time.Now().UTC().Add(-20 * 24 * time.Hour) }
	if _, err := svc.CreateSession(ctx, ws, "codex", "jtao", "old work", ""); err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return time.Now().UTC() }

	memDir, _ := ResolveMemoryDir(ws)
	orphanPath := filepath.Join(memDir, "artifacts", "orphan.txt")
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}

	repDry, err := svc.Hygiene(ctx, ws, 14, false)
	if err != nil || repDry.StaleActiveSessions != 1 || repDry.OrphanArtifactFiles != 1 || repDry.CleanedOrphanFiles != 0 {
		t.Fatalf("unexpected dry hygiene report: err=%v rep=%+v", err, repDry)
	}

	repApply, err := svc.Hygiene(ctx, ws, 14, true)
	if err != nil || repApply.ArchivedStaleSessions != 1 || repApply.CleanedOrphanFiles != 1 {
		t.Fatalf("unexpected apply hygiene report: err=%v rep=%+v", err, repApply)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("expected orphan artifact deleted")
	}
}
