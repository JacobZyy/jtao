package adapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JacobZyy/jtao/internal/memory"
	"github.com/JacobZyy/jtao/internal/workflow"
)

func TestInitIdempotencyBackupAndDoctor(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()

	// Dry-run must write zero files.
	dry, err := Init(ctx, InitInput{WorkspaceRoot: ws, Client: "claude", DryRun: true, BinaryPath: "/usr/local/bin/jtao"})
	if err != nil {
		t.Fatalf("Init dry-run failed: %v", err)
	}
	if len(dry.Actions) == 0 {
		t.Fatalf("expected planned actions")
	}
	if _, err := os.Stat(filepath.Join(ws, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not create .mcp.json")
	}

	// Real init creates skills, .mcp.json, and .claude/settings.local.json.
	first, err := Init(ctx, InitInput{WorkspaceRoot: ws, Client: "claude", BinaryPath: "/usr/local/bin/jtao"})
	if err != nil {
		t.Fatalf("Init first failed: %v", err)
	}
	if len(first.Actions) < 4 {
		t.Fatalf("expected >=4 init actions, got %+v", first.Actions)
	}

	mcpRaw, err := os.ReadFile(filepath.Join(ws, ".mcp.json"))
	if err != nil || !strings.Contains(string(mcpRaw), "/usr/local/bin/jtao") {
		t.Fatalf("unexpected .mcp.json: err=%v raw=%s", err, string(mcpRaw))
	}

	// Second identical init must be completely idempotent (all unchanged).
	second, err := Init(ctx, InitInput{WorkspaceRoot: ws, Client: "claude", BinaryPath: "/usr/local/bin/jtao"})
	if err != nil {
		t.Fatalf("Init second failed: %v", err)
	}
	for _, act := range second.Actions {
		if act.Action != "unchanged" {
			t.Fatalf("expected idempotent unchanged, got %+v", act)
		}
	}

	// Doctor verifies both SQLite stores cleanly.
	mem := memory.NewService()
	defer mem.Close()
	wf := workflow.NewService()
	defer wf.Close()
	doc, err := Doctor(ctx, mem, wf, ws)
	if err != nil || !doc.OK || !doc.Memory.FTS5Available || !doc.WorkflowOK {
		t.Fatalf("Doctor failed: err=%v rep=%+v", err, doc)
	}
}

func TestCodexInitHooksPreservesExistingAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	codexHome := t.TempDir()

	// Seed an unrelated existing hook that MUST be preserved intact.
	existingHooks := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"bash /custom/updater.sh"}]}]}}` + "\n"
	hooksPath := filepath.Join(codexHome, "hooks.json")
	if err := os.WriteFile(hooksPath, []byte(existingHooks), 0o644); err != nil {
		t.Fatal(err)
	}

	act1, err := upsertCodexHooks(hooksPath, "/usr/local/bin/jtao", false)
	if err != nil || act1.Action != "updated" {
		t.Fatalf("expected updated hooks.json, got err=%v act=%+v", err, act1)
	}
	raw, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "/custom/updater.sh") || !strings.Contains(text, "/usr/local/bin/jtao hook session-start") || !strings.Contains(text, "/usr/local/bin/jtao hook user-prompt-submit") {
		t.Fatalf("unexpected Codex hooks.json content: %s", text)
	}

	// Second call must be idempotent (unchanged).
	act2, err := upsertCodexHooks(hooksPath, "/usr/local/bin/jtao", false)
	if err != nil || act2.Action != "unchanged" {
		t.Fatalf("expected idempotent unchanged, got err=%v act=%+v", err, act2)
	}

	// Dry-run Codex init plans skills + hooks without writing.
	dry, err := Init(ctx, InitInput{WorkspaceRoot: ws, Client: "codex", DryRun: true, BinaryPath: "/usr/local/bin/jtao", CodexHome: t.TempDir()})
	if err != nil || len(dry.Actions) < 4 {
		t.Fatalf("unexpected codex dry-run: err=%v rep=%+v", err, dry)
	}
}
