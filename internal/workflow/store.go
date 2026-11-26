package workflow

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
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteService implements Service backed by <stateRoot>/workflow/workflow.db.
type SQLiteService struct {
	mu  sync.Mutex
	dbs map[string]*sql.DB
	now func() time.Time
}

// NewService constructs a decoupled workflow service.
func NewService() *SQLiteService {
	return &SQLiteService{
		dbs: make(map[string]*sql.DB),
		now: func() time.Time { return time.Now().UTC() },
	}
}

// Close closes all open SQLite handles managed by this service.
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

// ResolveWorkflowDir returns <workspace>/.jtao/workflow or the JTAO_STATE_DIR override.
func ResolveWorkflowDir(workspaceRoot string) (string, error) {
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
		return filepath.Join(absOverride, "workflow"), nil
	}
	return filepath.Join(absWS, ".jtao", "workflow"), nil
}

// DB exposes the shared single-writer WAL SQLite connection for workflow.db
// so scheduler can persist runs, run_turns, and run_leases atomically in the
// same database without SQLITE_BUSY lock contention.
func (s *SQLiteService) DB(ctx context.Context, workspaceRoot string) (*sql.DB, error) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	return db, err
}

func (s *SQLiteService) openDB(ctx context.Context, workspaceRoot string) (*sql.DB, string, error) {
	wfDir, err := ResolveWorkflowDir(workspaceRoot)
	if err != nil {
		return nil, "", err
	}
	dbPath := filepath.Join(wfDir, "workflow.db")

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.dbs[dbPath]; ok {
		return existing, wfDir, nil
	}

	if err := os.MkdirAll(filepath.Join(wfDir, "receipts"), 0o755); err != nil {
		return nil, "", fmt.Errorf("create workflow directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, "", fmt.Errorf("open workflow sqlite: %w", err)
	}
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

	if _, err := db.ExecContext(ctx, workflowSchemaSQL); err != nil {
		_ = db.Close()
		return nil, "", fmt.Errorf("init workflow schema: %w", err)
	}

	s.dbs[dbPath] = db
	return db, wfDir, nil
}

func (s *SQLiteService) nowISO() string {
	return s.now().UTC().Format(time.RFC3339Nano)
}

func newToken(prefix string) string {
	var raw [12]byte
	_, _ = rand.Read(raw[:])
	return fmt.Sprintf("%s:%d-%s", prefix, time.Now().UTC().UnixMilli(), hex.EncodeToString(raw[:]))
}

func marshalJSON(v any, fallback string) string {
	if v == nil {
		return fallback
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fallback
	}
	return string(b)
}

func unmarshalStrings(raw string) []string {
	if strings.TrimSpace(raw) == "" || raw == "[]" || raw == "null" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

const workflowSchemaSQL = `
CREATE TABLE IF NOT EXISTS work_items (
  id                  TEXT PRIMARY KEY,
  title               TEXT NOT NULL,
  objective           TEXT NOT NULL,
  route               TEXT NOT NULL DEFAULT 'implement',
  blueprint           TEXT NOT NULL DEFAULT 'single',
  status              TEXT NOT NULL DEFAULT 'active',
  is_active           INTEGER NOT NULL DEFAULT 0,
  baseline_receipt_id TEXT NOT NULL DEFAULT '',
  origin_client       TEXT NOT NULL DEFAULT 'unknown',
  origin_session_id   TEXT NOT NULL DEFAULT '',
  plan_md_path        TEXT NOT NULL DEFAULT '',
  metadata_json       TEXT NOT NULL DEFAULT '{}',
  created_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS task_nodes (
  id                       TEXT NOT NULL,
  work_item_id             TEXT NOT NULL REFERENCES work_items(id) ON DELETE CASCADE,
  node_kind                TEXT NOT NULL DEFAULT 'task',
  title                    TEXT NOT NULL,
  objective                TEXT NOT NULL DEFAULT '',
  role                     TEXT NOT NULL DEFAULT 'implementer',
  status                   TEXT NOT NULL DEFAULT 'pending',
  order_index              INTEGER NOT NULL DEFAULT 0,
  parallel_group           TEXT NOT NULL DEFAULT '',
  can_edit_files           INTEGER NOT NULL DEFAULT 1,
  owned_path_prefixes_json TEXT NOT NULL DEFAULT '[]',
  targets_json             TEXT NOT NULL DEFAULT '[]',
  acceptance               TEXT NOT NULL DEFAULT '',
  verify_spec_json         TEXT NOT NULL DEFAULT 'null',
  template_stage           TEXT NOT NULL DEFAULT '',
  retry_count              INTEGER NOT NULL DEFAULT 0,
  max_retries              INTEGER NOT NULL DEFAULT 2,
  execution_token          TEXT NOT NULL DEFAULT '',
  handoff_json             TEXT NOT NULL DEFAULT 'null',
  evidence_refs_json       TEXT NOT NULL DEFAULT '[]',
  created_at               TEXT NOT NULL,
  updated_at               TEXT NOT NULL,
  PRIMARY KEY (work_item_id, id)
);

CREATE TABLE IF NOT EXISTS task_edges (
  work_item_id TEXT NOT NULL,
  from_node_id TEXT NOT NULL,
  to_node_id   TEXT NOT NULL,
  PRIMARY KEY (work_item_id, from_node_id, to_node_id),
  FOREIGN KEY (work_item_id, from_node_id) REFERENCES task_nodes(work_item_id, id) ON DELETE CASCADE,
  FOREIGN KEY (work_item_id, to_node_id)   REFERENCES task_nodes(work_item_id, id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS execution_receipts (
  id             TEXT PRIMARY KEY,
  work_item_id   TEXT NOT NULL DEFAULT '',
  node_id        TEXT NOT NULL DEFAULT '',
  run_id         TEXT NOT NULL DEFAULT '',
  command        TEXT NOT NULL,
  args_json      TEXT NOT NULL,
  cwd            TEXT NOT NULL,
  exit_code      INTEGER NOT NULL,
  timed_out      INTEGER NOT NULL DEFAULT 0,
  duration_ms    INTEGER NOT NULL DEFAULT 0,
  stdout_sha256  TEXT NOT NULL DEFAULT '',
  stderr_sha256  TEXT NOT NULL DEFAULT '',
  output_excerpt TEXT NOT NULL DEFAULT '',
  artifact_ref   TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
  id                TEXT PRIMARY KEY,
  work_item_id      TEXT NOT NULL REFERENCES work_items(id),
  mode              TEXT NOT NULL,
  provider          TEXT NOT NULL DEFAULT 'codex',
  status            TEXT NOT NULL DEFAULT 'running',
  current_iteration INTEGER NOT NULL DEFAULT 0,
  max_iterations    INTEGER NOT NULL DEFAULT 20,
  worktree_json     TEXT NOT NULL DEFAULT 'null',
  pacing_json       TEXT NOT NULL DEFAULT 'null',
  backoff_json      TEXT NOT NULL DEFAULT 'null',
  stop_requested    INTEGER NOT NULL DEFAULT 0,
  stop_reason       TEXT NOT NULL DEFAULT '',
  summary_text      TEXT NOT NULL DEFAULT '',
  created_at        TEXT NOT NULL,
  updated_at        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS run_turns (
  id                 TEXT PRIMARY KEY,
  run_id             TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  work_item_id       TEXT NOT NULL,
  node_id            TEXT NOT NULL,
  iteration          INTEGER NOT NULL,
  phase_state        TEXT NOT NULL,
  outcome            TEXT NOT NULL DEFAULT '',
  failure_class      TEXT NOT NULL DEFAULT 'none',
  material           INTEGER NOT NULL DEFAULT 0,
  is_safe_bypass     INTEGER NOT NULL DEFAULT 0,
  prompt_ref         TEXT NOT NULL DEFAULT '',
  output_ref         TEXT NOT NULL DEFAULT '',
  receipt_ids_json   TEXT NOT NULL DEFAULT '[]',
  files_touched_json TEXT NOT NULL DEFAULT '[]',
  handoff_json       TEXT NOT NULL DEFAULT 'null',
  elapsed_ms         INTEGER NOT NULL DEFAULT 0,
  started_at         TEXT NOT NULL,
  finished_at        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS run_leases (
  lease_key    TEXT PRIMARY KEY,
  run_id       TEXT NOT NULL,
  owner_pid    INTEGER NOT NULL,
  hostname     TEXT NOT NULL,
  acquired_at  TEXT NOT NULL,
  heartbeat_at TEXT NOT NULL,
  expires_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_nodes_work_status ON task_nodes(work_item_id, status, order_index);
CREATE INDEX IF NOT EXISTS idx_receipts_work_node ON execution_receipts(work_item_id, node_id, created_at);
CREATE INDEX IF NOT EXISTS idx_turns_run_iter ON run_turns(run_id, iteration);
`
