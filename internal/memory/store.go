package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	defaultSpace     = "default"
	schemaDDLVersion = 1
)

// SQLiteService implements Service backed by per-workspace SQLite WAL + FTS5
// plus external artifact files under <stateRoot>/memory/artifacts/.
type SQLiteService struct {
	mu  sync.Mutex
	dbs map[string]*sql.DB
	now func() time.Time
}

// NewService constructs a decoupled memory service.
func NewService() *SQLiteService {
	return &SQLiteService{
		dbs: make(map[string]*sql.DB),
		now: func() time.Time { return time.Now().UTC() },
	}
}

// Close closes all cached SQLite connections held by this service.
func (s *SQLiteService) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var firstErr error
	for key, db := range s.dbs {
		if err := db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(s.dbs, key)
	}
	return firstErr
}

// ResolveMemoryDir returns <workspace>/.jtao/memory or the JTAO_STATE_DIR override.
func ResolveMemoryDir(workspaceRoot string) (string, error) {
	ws := strings.TrimSpace(workspaceRoot)
	if ws == "" {
		return "", fmt.Errorf("workspaceRoot is required")
	}
	absWS, err := filepath.Abs(ws)
	if err != nil {
		return "", fmt.Errorf("resolve workspaceRoot: %w", err)
	}
	if override := strings.TrimSpace(os.Getenv("JTAO_STATE_DIR")); override != "" {
		absOverride, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("resolve JTAO_STATE_DIR: %w", err)
		}
		return filepath.Join(absOverride, "memory"), nil
	}
	return filepath.Join(absWS, ".jtao", "memory"), nil
}

func (s *SQLiteService) openDB(ctx context.Context, workspaceRoot string) (*sql.DB, string, error) {
	memDir, err := ResolveMemoryDir(workspaceRoot)
	if err != nil {
		return nil, "", err
	}
	dbPath := filepath.Join(memDir, "memory.db")

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.dbs[dbPath]; ok {
		return existing, memDir, nil
	}

	if err := os.MkdirAll(filepath.Join(memDir, "artifacts"), 0o755); err != nil {
		return nil, "", fmt.Errorf("create memory directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, "", fmt.Errorf("open memory sqlite: %w", err)
	}
	// SQLite handles concurrent readers well in WAL mode, while serializing
	// Go-side writes on a single connection avoids SQLITE_BUSY flakiness.
	db.SetMaxOpenConns(1)

	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA foreign_keys = ON;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA synchronous = NORMAL;",
	}
	for _, stmt := range pragmas {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			return nil, "", fmt.Errorf("apply pragma %q: %w", stmt, err)
		}
	}

	if _, err := db.ExecContext(ctx, memorySchemaSQL); err != nil {
		_ = db.Close()
		return nil, "", fmt.Errorf("init memory schema: %w", err)
	}

	s.dbs[dbPath] = db
	return db, memDir, nil
}

func (s *SQLiteService) nowISO() string {
	return s.now().UTC().Format(time.RFC3339Nano)
}

func newPrefixedID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	stamp := time.Now().UTC().UnixMilli()
	return fmt.Sprintf("%s:%d-%s", prefix, stamp, hex.EncodeToString(raw[:]))
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func normalizeSpace(space string) string {
	trimmed := strings.TrimSpace(space)
	if trimmed == "" {
		return defaultSpace
	}
	return trimmed
}

func marshalStringSlice(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	b, err := json.Marshal(items)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalStringSlice(raw string) []string {
	if strings.TrimSpace(raw) == "" || raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

const memorySchemaSQL = `
CREATE TABLE IF NOT EXISTS memo_events (
  id              TEXT PRIMARY KEY,
  seq             INTEGER NOT NULL,
  space           TEXT NOT NULL DEFAULT 'default',
  scope           TEXT NOT NULL DEFAULT 'project_shared',
  agent           TEXT NOT NULL DEFAULT '',
  work_item_id    TEXT NOT NULL DEFAULT '',
  text            TEXT NOT NULL,
  search_tokens   TEXT NOT NULL,
  refs_json       TEXT NOT NULL DEFAULT '[]',
  entities_json   TEXT NOT NULL DEFAULT '[]',
  evidence_ref    TEXT NOT NULL DEFAULT '',
  confidence      TEXT NOT NULL DEFAULT '',
  claim_status    TEXT NOT NULL DEFAULT 'verified',
  archived        INTEGER NOT NULL DEFAULT 0,
  promotion_of    TEXT NOT NULL DEFAULT '',
  provenance_json TEXT NOT NULL,
  valid_at        TEXT NOT NULL,
  created_at      TEXT NOT NULL,
  source_ref      TEXT NOT NULL DEFAULT '',
  UNIQUE(space, seq)
);

CREATE TABLE IF NOT EXISTS memo_supersedes (
  source_id TEXT NOT NULL REFERENCES memo_events(id) ON DELETE CASCADE,
  target_id TEXT NOT NULL REFERENCES memo_events(id) ON DELETE CASCADE,
  PRIMARY KEY (source_id, target_id)
);

CREATE TABLE IF NOT EXISTS memo_feedback (
  memo_id    TEXT NOT NULL REFERENCES memo_events(id) ON DELETE CASCADE,
  session_id TEXT NOT NULL DEFAULT '',
  agent      TEXT NOT NULL DEFAULT '',
  turn_id    TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS memo_exposures (
  session_id TEXT NOT NULL,
  memo_id    TEXT NOT NULL REFERENCES memo_events(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS pinned_entries (
  space        TEXT PRIMARY KEY,
  content      TEXT NOT NULL DEFAULT '',
  content_hash TEXT NOT NULL DEFAULT '',
  updated_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
  id           TEXT PRIMARY KEY,
  agent        TEXT NOT NULL,
  project      TEXT NOT NULL,
  goal         TEXT NOT NULL DEFAULT '',
  work_item_id TEXT NOT NULL DEFAULT '',
  status       TEXT NOT NULL DEFAULT 'active',
  tags_json    TEXT NOT NULL DEFAULT '[]',
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS session_events (
  id            TEXT PRIMARY KEY,
  session_id    TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  seq           INTEGER NOT NULL,
  role          TEXT NOT NULL,
  kind          TEXT NOT NULL,
  text          TEXT NOT NULL,
  search_tokens TEXT NOT NULL,
  refs_json     TEXT NOT NULL DEFAULT '[]',
  work_item_id  TEXT NOT NULL DEFAULT '',
  turn_id       TEXT NOT NULL DEFAULT '',
  outcome       TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL,
  UNIQUE(session_id, seq)
);

CREATE TABLE IF NOT EXISTS session_checkpoints (
  id             TEXT PRIMARY KEY,
  session_id     TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  seq            INTEGER NOT NULL,
  work_item_id   TEXT NOT NULL DEFAULT '',
  status         TEXT NOT NULL,
  summary        TEXT NOT NULL,
  search_tokens  TEXT NOT NULL,
  next_json      TEXT NOT NULL DEFAULT '[]',
  artifacts_json TEXT NOT NULL DEFAULT '[]',
  verify_result  TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  UNIQUE(session_id, seq)
);

CREATE TABLE IF NOT EXISTS session_continuity (
  session_id    TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
  work_item_id  TEXT NOT NULL DEFAULT '',
  intent        TEXT NOT NULL DEFAULT '',
  summary       TEXT NOT NULL DEFAULT '',
  touched_json  TEXT NOT NULL DEFAULT '[]',
  next_json     TEXT NOT NULL DEFAULT '[]',
  updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS session_handoffs (
  session_id   TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
  work_item_id TEXT NOT NULL DEFAULT '',
  packet_json  TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS governance_receipts (
  id          TEXT PRIMARY KEY,
  action      TEXT NOT NULL,
  target_id   TEXT NOT NULL,
  actor       TEXT NOT NULL,
  note        TEXT NOT NULL DEFAULT '',
  detail_json TEXT NOT NULL DEFAULT '{}',
  created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS artifacts (
  id           TEXT PRIMARY KEY,
  session_id   TEXT NOT NULL DEFAULT '',
  work_item_id TEXT NOT NULL DEFAULT '',
  tool         TEXT NOT NULL DEFAULT '',
  sha256       TEXT NOT NULL,
  byte_size    INTEGER NOT NULL,
  rel_path     TEXT NOT NULL,
  preview      TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL
);

CREATE VIRTUAL TABLE IF NOT EXISTS memo_fts USING fts5(memo_id UNINDEXED, search_tokens, tokenize = 'unicode61');
CREATE VIRTUAL TABLE IF NOT EXISTS session_events_fts USING fts5(event_id UNINDEXED, search_tokens, tokenize = 'unicode61');
CREATE VIRTUAL TABLE IF NOT EXISTS session_checkpoints_fts USING fts5(checkpoint_id UNINDEXED, search_tokens, tokenize = 'unicode61');

CREATE INDEX IF NOT EXISTS idx_memo_space_active ON memo_events(space, archived, claim_status, created_at);
CREATE INDEX IF NOT EXISTS idx_memo_work_item ON memo_events(work_item_id) WHERE work_item_id != '';
CREATE INDEX IF NOT EXISTS idx_memo_source_ref ON memo_events(space, source_ref) WHERE source_ref != '';
CREATE INDEX IF NOT EXISTS idx_sessions_agent_updated ON sessions(agent, updated_at);
CREATE INDEX IF NOT EXISTS idx_checkpoints_work_item ON session_checkpoints(work_item_id) WHERE work_item_id != '';
CREATE INDEX IF NOT EXISTS idx_exposures_session ON memo_exposures(session_id, created_at);
`
