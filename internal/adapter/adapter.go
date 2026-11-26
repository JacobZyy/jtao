package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/JacobZyy/jtao/assets"
	"github.com/JacobZyy/jtao/internal/memory"
	"github.com/JacobZyy/jtao/internal/runner"
	"github.com/JacobZyy/jtao/internal/workflow"
)

// InitInput configures idempotent jtao integration into Codex / Claude Code.
type InitInput struct {
	WorkspaceRoot string
	Client        string // codex | claude | all
	DryRun        bool
	BinaryPath    string // Defaults to resolved os.Executable()
	CodexHome     string // Optional override for ~/.codex (defaults to $CODEX_HOME or ~/.codex)
}

// InitAction records a single file or command operation performed (or planned) by Init.
type InitAction struct {
	Target string `json:"target"`
	Action string `json:"action"` // created | updated | unchanged | planned | skipped
	Detail string `json:"detail"`
}

// InitReport summarizes the outcome of jtao init.
type InitReport struct {
	BinaryPath string       `json:"binaryPath"`
	Workspace  string       `json:"workspace"`
	DryRun     bool         `json:"dryRun"`
	Actions    []InitAction `json:"actions"`
}

// DoctorReport aggregates environment, SQLite, and client readiness checks.
type DoctorReport struct {
	OK           bool                   `json:"ok"`
	BinaryPath   string                 `json:"binaryPath"`
	Workspace    string                 `json:"workspace"`
	Memory       memory.DoctorReport    `json:"memory"`
	WorkflowDB   string                 `json:"workflowDb"`
	WorkflowOK   bool                   `json:"workflowOk"`
	Codex        runner.ReadinessCheck  `json:"codex"`
	Claude       runner.ReadinessCheck  `json:"claude"`
	Notes        []string               `json:"notes,omitempty"`
}

// Init idempotently installs jtao MCP, Hooks (with absolute binary path), and embedded Skills.
func Init(ctx context.Context, in InitInput) (InitReport, error) {
	absWS, err := filepath.Abs(in.WorkspaceRoot)
	if err != nil {
		return InitReport{}, fmt.Errorf("resolve workspace: %w", err)
	}
	binPath := strings.TrimSpace(in.BinaryPath)
	if binPath == "" {
		binPath, err = os.Executable()
		if err != nil {
			return InitReport{}, fmt.Errorf("resolve jtao binary path: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(binPath); err == nil {
			binPath = resolved
		}
	}

	client := strings.ToLower(strings.TrimSpace(in.Client))
	if client == "" {
		client = "all"
	}
	if client != "codex" && client != "claude" && client != "all" {
		return InitReport{}, fmt.Errorf("unsupported client %q (use codex|claude|all)", in.Client)
	}

	report := InitReport{
		BinaryPath: binPath,
		Workspace:  absWS,
		DryRun:     in.DryRun,
	}

	// 1. Claude Code workspace .mcp.json + .claude/settings.local.json hooks + workspace skills.
	if client == "claude" || client == "all" {
		skillActions, err := syncEmbeddedSkills(filepath.Join(absWS, ".jtao", "skills"), in.DryRun)
		if err != nil {
			return report, err
		}
		report.Actions = append(report.Actions, skillActions...)

		mcpAct, err := upsertProjectMCPJSON(filepath.Join(absWS, ".mcp.json"), binPath, absWS, in.DryRun)
		if err != nil {
			return report, err
		}
		report.Actions = append(report.Actions, mcpAct)

		hookAct, err := upsertClaudeHooks(filepath.Join(absWS, ".claude", "settings.local.json"), binPath, absWS, in.DryRun)
		if err != nil {
			return report, err
		}
		report.Actions = append(report.Actions, hookAct)
	}

	// 2. Codex CLI global integration: MCP (`codex mcp add`), ~/.codex/hooks.json, and ~/.codex/skills/.
	// Codex configs are user-global, so omit --workspace to let jtao bind dynamically to each repo's cwd.
	if client == "codex" || client == "all" {
		codexHome, err := resolveCodexHome(in.CodexHome)
		if err != nil {
			return report, err
		}

		codexSkillActions, err := syncEmbeddedSkills(filepath.Join(codexHome, "skills"), in.DryRun)
		if err != nil {
			return report, err
		}
		report.Actions = append(report.Actions, codexSkillActions...)

		codexHookAct, err := upsertCodexHooks(filepath.Join(codexHome, "hooks.json"), binPath, in.DryRun)
		if err != nil {
			return report, err
		}
		report.Actions = append(report.Actions, codexHookAct)

		codexAct := registerCodexMCP(ctx, binPath, in.DryRun)
		report.Actions = append(report.Actions, codexAct)
	}

	return report, nil
}

// Doctor verifies binary path, SQLite WAL/FTS5 health, and Codex/Claude CLI readiness.
func Doctor(ctx context.Context, mem *memory.SQLiteService, wf *workflow.SQLiteService, workspaceRoot string) (DoctorReport, error) {
	absWS, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return DoctorReport{}, err
	}
	binPath, _ := os.Executable()

	memDoc, err := mem.Doctor(ctx, absWS)
	if err != nil {
		return DoctorReport{}, err
	}

	wfDir, err := workflow.ResolveWorkflowDir(absWS)
	if err != nil {
		return DoctorReport{}, err
	}
	wfOK := false
	if db, err := wf.DB(ctx, absWS); err == nil {
		var integrity string
		if err := db.QueryRowContext(ctx, `PRAGMA integrity_check;`).Scan(&integrity); err == nil && strings.EqualFold(integrity, "ok") {
			wfOK = true
		}
	}

	codexCheck := (runner.CodexRunner{}).CheckReadiness()
	claudeCheck := (runner.ClaudeRunner{}).CheckReadiness()

	var notes []string
	if !codexCheck.OK && !claudeCheck.OK {
		notes = append(notes, "Neither codex nor claude CLI found on PATH; solo/team agent runs require at least one.")
	}

	return DoctorReport{
		OK:         memDoc.OK && wfOK,
		BinaryPath: binPath,
		Workspace:  absWS,
		Memory:     memDoc,
		WorkflowDB: filepath.Join(wfDir, "workflow.db"),
		WorkflowOK: wfOK,
		Codex:      codexCheck,
		Claude:     claudeCheck,
		Notes:      notes,
	}, nil
}

func syncEmbeddedSkills(destDir string, dryRun bool) ([]InitAction, error) {
	var actions []InitAction
	err := fs.WalkDir(assets.FS, "skills", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		content, err := assets.FS.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, "skills/")
		target := filepath.Join(destDir, filepath.FromSlash(rel))

		existing, readErr := os.ReadFile(target)
		if readErr == nil && string(existing) == string(content) {
			actions = append(actions, InitAction{Target: target, Action: "unchanged", Detail: "skill already up to date"})
			return nil
		}
		if dryRun {
			actions = append(actions, InitAction{Target: target, Action: "planned", Detail: "would write embedded skill"})
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, content, 0o644); err != nil {
			return err
		}
		act := "created"
		if readErr == nil {
			act = "updated"
		}
		actions = append(actions, InitAction{Target: target, Action: act, Detail: "embedded skill written"})
		return nil
	})
	return actions, err
}

func upsertProjectMCPJSON(filePath, binPath, workspaceRoot string, dryRun bool) (InitAction, error) {
	doc, raw, exists, err := readJSONObjectFile(filePath)
	if err != nil {
		return InitAction{}, err
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = make(map[string]any)
	}
	servers["jtao"] = map[string]any{
		"command": binPath,
		"args":    []string{"mcp", "--workspace", workspaceRoot},
	}
	doc["mcpServers"] = servers

	return writeJSONWithBackupAndVerify(filePath, doc, raw, exists, dryRun, "registered jtao stdio MCP server")
}

func upsertClaudeHooks(filePath, binPath, workspaceRoot string, dryRun bool) (InitAction, error) {
	doc, raw, exists, err := readJSONObjectFile(filePath)
	if err != nil {
		return InitAction{}, err
	}
	hooksMap, _ := doc["hooks"].(map[string]any)
	if hooksMap == nil {
		hooksMap = make(map[string]any)
	}

	hookSpecs := []struct {
		event string
		sub   string
	}{
		{"SessionStart", "session-start"},
		{"UserPromptSubmit", "user-prompt-submit"},
		{"Stop", "stop"},
		{"PostToolUse", "post-tool-use"},
	}

	for _, spec := range hookSpecs {
		cmd := fmt.Sprintf("%s hook %s --workspace %s", shellQuote(binPath), spec.sub, shellQuote(workspaceRoot))
		hooksMap[spec.event] = upsertClaudeHookEvent(hooksMap[spec.event], spec.sub, cmd)
	}
	doc["hooks"] = hooksMap

	return writeJSONWithBackupAndVerify(filePath, doc, raw, exists, dryRun, "registered Claude hooks with absolute jtao binary path")
}

func upsertClaudeHookEvent(rawList any, subcmd, cmd string) []any {
	list, _ := rawList.([]any)
	marker := " hook " + subcmd + " "
	desired := map[string]any{
		"matcher": "",
		"hooks": []any{
			map[string]any{"type": "command", "command": cmd},
		},
	}

	for i, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := entry["hooks"].([]any)
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			existingCmd, _ := hm["command"].(string)
			if strings.Contains(existingCmd, marker) {
				list[i] = desired
				return list
			}
		}
	}
	return append(list, desired)
}

func resolveCodexHome(explicit string) (string, error) {
	if trimmed := strings.TrimSpace(explicit); trimmed != "" {
		return filepath.Abs(trimmed)
	}
	if env := strings.TrimSpace(os.Getenv("CODEX_HOME")); env != "" {
		return filepath.Abs(env)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home for ~/.codex: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

func upsertCodexHooks(filePath, binPath string, dryRun bool) (InitAction, error) {
	doc, raw, exists, err := readJSONObjectFile(filePath)
	if err != nil {
		return InitAction{}, err
	}
	hooksMap, _ := doc["hooks"].(map[string]any)
	if hooksMap == nil {
		hooksMap = make(map[string]any)
	}

	hookSpecs := []struct {
		event         string
		sub           string
		statusMessage string
	}{
		{"SessionStart", "session-start", "jtao · 置顶与工单恢复"},
		{"UserPromptSubmit", "user-prompt-submit", "jtao · 记忆与工单召回"},
	}

	for _, spec := range hookSpecs {
		cmd := fmt.Sprintf("%s hook %s", shellQuote(binPath), spec.sub)
		hooksMap[spec.event] = upsertCodexHookEvent(hooksMap[spec.event], spec.sub, cmd, spec.statusMessage)
	}
	doc["hooks"] = hooksMap

	return writeJSONWithBackupAndVerify(filePath, doc, raw, exists, dryRun, "registered global Codex hooks (SessionStart + UserPromptSubmit) without workspace lock")
}

func upsertCodexHookEvent(rawList any, subcmd, cmd, statusMessage string) []any {
	list, _ := rawList.([]any)
	marker := "jtao hook " + subcmd
	desired := map[string]any{
		"hooks": []any{
			map[string]any{
				"type":          "command",
				"command":       cmd,
				"timeout":       10,
				"statusMessage": statusMessage,
			},
		},
	}

	for i, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		inner, _ := entry["hooks"].([]any)
		for _, h := range inner {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			existingCmd, _ := hm["command"].(string)
			if strings.Contains(existingCmd, marker) {
				list[i] = desired
				return list
			}
		}
	}
	return append(list, desired)
}

func registerCodexMCP(ctx context.Context, binPath string, dryRun bool) InitAction {
	codexPath, err := runner.ResolveBinary("codex")
	if err != nil {
		return InitAction{
			Target: "codex-mcp",
			Action: "skipped",
			Detail: "codex CLI not found on PATH; run `codex mcp add jtao -- " + binPath + " mcp` once installed",
		}
	}
	args := []string{"mcp", "add", "jtao", "--", binPath, "mcp"}
	if dryRun {
		return InitAction{
			Target: "codex-mcp",
			Action: "planned",
			Detail: fmt.Sprintf("would run: %s %s", codexPath, strings.Join(args, " ")),
		}
	}
	cmd := exec.CommandContext(ctx, codexPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return InitAction{
			Target: "codex-mcp",
			Action: "skipped",
			Detail: fmt.Sprintf("codex mcp add returned error (%v): %s", err, strings.TrimSpace(string(out))),
		}
	}
	return InitAction{
		Target: "codex-mcp",
		Action: "updated",
		Detail: "registered global jtao MCP server in Codex via `codex mcp add`",
	}
}

func readJSONObjectFile(filePath string) (map[string]any, string, bool, error) {
	rawBytes, err := os.ReadFile(filePath)
	if os.IsNotExist(err) {
		return make(map[string]any), "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	raw := string(rawBytes)
	doc := make(map[string]any)
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal(rawBytes, &doc); err != nil {
			return nil, raw, true, fmt.Errorf("parse %s: %w", filePath, err)
		}
	}
	return doc, raw, true, nil
}

func writeJSONWithBackupAndVerify(filePath string, doc map[string]any, prevRaw string, exists, dryRun bool, detail string) (InitAction, error) {
	nextBytes, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return InitAction{}, err
	}
	nextRaw := string(nextBytes) + "\n"
	if exists && prevRaw == nextRaw {
		return InitAction{Target: filePath, Action: "unchanged", Detail: detail}, nil
	}
	if dryRun {
		return InitAction{Target: filePath, Action: "planned", Detail: "would update: " + detail}, nil
	}

	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return InitAction{}, err
	}
	if exists {
		backup := fmt.Sprintf("%s.bak-%s", filePath, time.Now().UTC().Format("20060102-150405"))
		if err := os.WriteFile(backup, []byte(prevRaw), 0o644); err != nil {
			return InitAction{}, fmt.Errorf("write backup %s: %w", backup, err)
		}
	}
	if err := os.WriteFile(filePath, []byte(nextRaw), 0o644); err != nil {
		return InitAction{}, err
	}
	// Readback verification to guarantee valid persisted JSON.
	readBack, err := os.ReadFile(filePath)
	if err != nil || string(readBack) != nextRaw {
		return InitAction{}, fmt.Errorf("readback verification failed for %s", filePath)
	}

	act := "created"
	if exists {
		act = "updated"
	}
	return InitAction{Target: filePath, Action: act, Detail: detail}, nil
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '/' || r == '.' || r == '_' || r == '-' || r == ':') {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
