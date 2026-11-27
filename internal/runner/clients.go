package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ReadinessCheck reports whether a client CLI is installed and runnable.
type ReadinessCheck struct {
	OK          bool     `json:"ok"`
	Provider    string   `json:"provider"`
	BinaryPath  string   `json:"binaryPath,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	NextActions []string `json:"nextActions,omitempty"`
}

// RunTurnInput specifies a single agent turn execution.
type RunTurnInput struct {
	WorkspaceRoot string
	ExecutionCwd  string
	SessionID     string
	WorkItemID    string
	SystemPrompt  string
	UserPrompt    string
	OutputSchema  []byte // Optional JSON Schema passed to Codex --output-schema
	Timeout       time.Duration
	Env           []string
	ExtraArgs     []string
	Offload       OffloadHook
}

// RunTurnResult returns normalized execution output, parsed JSON, and safety flags.
type RunTurnResult struct {
	ExitCode          int            `json:"exitCode"`
	TimedOut          bool           `json:"timedOut"`
	Aborted           bool           `json:"aborted"`
	TreeAlive         bool           `json:"treeAlive"`
	KillEscalated     bool           `json:"killEscalated"`
	ChildPID          int            `json:"childPid,omitempty"`
	RawOutput         string         `json:"rawOutput"`
	ParsedJSON        map[string]any `json:"parsedJson,omitempty"`
	OutputArtifactRef string         `json:"outputArtifactRef,omitempty"`
	ElapsedMs         int64          `json:"elapsedMs"`
	Command           string         `json:"command"`
	Args              []string       `json:"args"`
	Error             string         `json:"error,omitempty"`
}

// AgentRunner is the single unified interface shared by Solo and Team schedulers.
type AgentRunner interface {
	ID() string
	CheckReadiness() ReadinessCheck
	RunTurn(ctx context.Context, in RunTurnInput) (RunTurnResult, error)
}

// ResolveRunner returns the built-in AgentRunner for codex or claude.
func ResolveRunner(provider string) (AgentRunner, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "codex":
		return CodexRunner{}, nil
	case "claude":
		return ClaudeRunner{}, nil
	default:
		return nil, fmt.Errorf("unsupported agent provider: %q (supported: codex, claude)", provider)
	}
}

// fallbackBinDirs lists standard user/package-manager binary directories that GUI apps
// (such as macOS Codex Desktop) often omit from their minimal launchd PATH.
func fallbackBinDirs() []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs,
			filepath.Join(home, ".vite-plus", "bin"),
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "go", "bin"),
			filepath.Join(home, ".volta", "bin"),
			filepath.Join(home, ".npm-global", "bin"),
		)
	}
	dirs = append(dirs, "/opt/homebrew/bin", "/usr/local/bin", "/usr/local/go/bin")
	return dirs
}

// ResolveBinary locates an executable via PATH first, then falls back to common user/Homebrew
// bin directories so background runners launched from GUI apps still find codex/claude.
func ResolveBinary(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, dir := range fallbackBinDirs() {
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found on PATH or standard user bin directories", name)
}

// BuildAugmentedEnv returns an environment slice whose PATH includes extraBinDir and standard
// user/Homebrew bin directories (preserving existing PATH entries and order).
func BuildAugmentedEnv(baseEnv []string, extraBinDir string) []string {
	env := baseEnv
	if len(env) == 0 {
		env = os.Environ()
	}
	var extras []string
	if strings.TrimSpace(extraBinDir) != "" {
		extras = append(extras, strings.TrimSpace(extraBinDir))
	}
	extras = append(extras, fallbackBinDirs()...)

	out := make([]string, 0, len(env)+1)
	pathFound := false
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PATH=") {
			out = append(out, kv)
			continue
		}
		pathFound = true
		existing := strings.TrimPrefix(kv, "PATH=")
		out = append(out, "PATH="+mergePATH(existing, extras))
	}
	if !pathFound {
		out = append(out, "PATH="+mergePATH("/usr/bin:/bin:/usr/sbin:/sbin", extras))
	}
	return out
}

func mergePATH(existing string, extras []string) string {
	seen := make(map[string]bool)
	var parts []string
	for _, entry := range filepath.SplitList(existing) {
		if entry != "" && !seen[entry] {
			seen[entry] = true
			parts = append(parts, entry)
		}
	}
	for _, dir := range extras {
		if dir == "" || seen[dir] {
			continue
		}
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			seen[dir] = true
			parts = append(parts, dir)
		}
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

// CodexRunner invokes the native `codex exec` CLI with optional --output-schema.
type CodexRunner struct{}

func (CodexRunner) ID() string { return "codex" }

func (CodexRunner) CheckReadiness() ReadinessCheck {
	path, err := ResolveBinary("codex")
	if err != nil {
		return ReadinessCheck{
			OK:          false,
			Provider:    "codex",
			Reason:      "codex CLI not found on PATH or fallback user bin dirs",
			NextActions: []string{"Install Codex CLI or pass --provider claude."},
		}
	}
	return ReadinessCheck{OK: true, Provider: "codex", BinaryPath: path}
}

func (CodexRunner) RunTurn(ctx context.Context, in RunTurnInput) (RunTurnResult, error) {
	cwd := strings.TrimSpace(in.ExecutionCwd)
	if cwd == "" {
		cwd = in.WorkspaceRoot
	}
	prompt := composePrompt(in.SystemPrompt, in.UserPrompt)

	var tempFiles []string
	defer func() {
		for _, f := range tempFiles {
			_ = os.Remove(f)
		}
	}()

	args := []string{"exec", "--skip-git-repo-check", "--sandbox", "workspace-write"}
	lastMsgPath := ""

	if len(in.OutputSchema) > 0 {
		schemaFile, err := os.CreateTemp("", "jtao-codex-schema-*.json")
		if err != nil {
			return RunTurnResult{}, fmt.Errorf("create output schema temp file: %w", err)
		}
		_, writeErr := schemaFile.Write(in.OutputSchema)
		closeErr := schemaFile.Close()
		tempFiles = append(tempFiles, schemaFile.Name())
		if writeErr != nil || closeErr != nil {
			return RunTurnResult{}, fmt.Errorf("write output schema: %v %v", writeErr, closeErr)
		}
		args = append(args, "--output-schema", schemaFile.Name())
	}

	if outFile, err := os.CreateTemp("", "jtao-codex-last-*.txt"); err == nil {
		lastMsgPath = outFile.Name()
		_ = outFile.Close()
		tempFiles = append(tempFiles, lastMsgPath)
		args = append(args, "--output-last-message", lastMsgPath)
	}

	args = append(args, in.ExtraArgs...)
	args = append(args, prompt)

	binPath, err := ResolveBinary("codex")
	if err != nil {
		return RunTurnResult{
			ExitCode: -1,
			Command:  "codex",
			Args:     args,
			Error:    err.Error(),
		}, nil
	}
	env := BuildAugmentedEnv(in.Env, filepath.Dir(binPath))

	procRes := ExecProcessGroup(ctx, binPath, args, cwd, env, NormalizeTimeout(in.Timeout))
	raw := strings.TrimSpace(procRes.Stdout + "\n" + procRes.Stderr)
	if lastMsgPath != "" {
		if msgBytes, err := os.ReadFile(lastMsgPath); err == nil && len(strings.TrimSpace(string(msgBytes))) > 0 {
			raw = strings.TrimSpace(string(msgBytes))
		}
	}

	return finalizeTurnResult(ctx, "codex", args, in, procRes, raw), nil
}

// ClaudeRunner invokes `claude -p --output-format json`.
type ClaudeRunner struct{}

func (ClaudeRunner) ID() string { return "claude" }

func (ClaudeRunner) CheckReadiness() ReadinessCheck {
	path, err := ResolveBinary("claude")
	if err != nil {
		return ReadinessCheck{
			OK:          false,
			Provider:    "claude",
			Reason:      "claude CLI not found on PATH or fallback user bin dirs",
			NextActions: []string{"Install Claude Code CLI or pass --provider codex."},
		}
	}
	return ReadinessCheck{OK: true, Provider: "claude", BinaryPath: path}
}

func (ClaudeRunner) RunTurn(ctx context.Context, in RunTurnInput) (RunTurnResult, error) {
	cwd := strings.TrimSpace(in.ExecutionCwd)
	if cwd == "" {
		cwd = in.WorkspaceRoot
	}
	args := []string{"-p", "--output-format", "json"}
	if strings.TrimSpace(in.SystemPrompt) != "" {
		args = append(args, "--append-system-prompt", strings.TrimSpace(in.SystemPrompt))
	}
	args = append(args, in.ExtraArgs...)
	args = append(args, in.UserPrompt)

	binPath, err := ResolveBinary("claude")
	if err != nil {
		return RunTurnResult{
			ExitCode: -1,
			Command:  "claude",
			Args:     args,
			Error:    err.Error(),
		}, nil
	}
	env := BuildAugmentedEnv(in.Env, filepath.Dir(binPath))

	procRes := ExecProcessGroup(ctx, binPath, args, cwd, env, NormalizeTimeout(in.Timeout))
	raw := strings.TrimSpace(procRes.Stdout)
	if raw == "" {
		raw = strings.TrimSpace(procRes.Stderr)
	}
	return finalizeTurnResult(ctx, "claude", args, in, procRes, raw), nil
}

func finalizeTurnResult(ctx context.Context, cmd string, args []string, in RunTurnInput, procRes ProcessResult, raw string) RunTurnResult {
	var parsed map[string]any
	if procRes.ExitCode == 0 {
		parsed = ExtractJSONObject(raw)
		// Claude --output-format json wraps the assistant text in {"result": "..."}.
		if parsed != nil && cmd == "claude" {
			if inner, ok := parsed["result"].(string); ok {
				if innerObj := ExtractJSONObject(inner); innerObj != nil {
					parsed = innerObj
				}
			}
		}
	}

	artifactRef := ""
	displayRaw := raw
	if in.Offload != nil && len(raw) >= defaultOffloadThreshold {
		if ref, err := in.Offload(ctx, in.WorkspaceRoot, in.SessionID, in.WorkItemID, "agent:"+cmd, raw); err == nil && ref != "" {
			artifactRef = ref
			displayRaw = fmt.Sprintf("[offloaded to %s (%d bytes)]\n%s", ref, len(raw), clipText(raw, 400))
		}
	}

	errMsg := ""
	if procRes.Err != nil {
		errMsg = procRes.Err.Error()
	}

	return RunTurnResult{
		ExitCode:          procRes.ExitCode,
		TimedOut:          procRes.TimedOut,
		Aborted:           procRes.Aborted,
		TreeAlive:         procRes.TreeAlive,
		KillEscalated:     procRes.KillEscalated,
		ChildPID:          procRes.ChildPID,
		RawOutput:         displayRaw,
		ParsedJSON:        parsed,
		OutputArtifactRef: artifactRef,
		ElapsedMs:         procRes.ElapsedMs,
		Command:           cmd,
		Args:              args,
		Error:             errMsg,
	}
}

// ExtractJSONObject parses a raw JSON object or extracts the last fenced/bare JSON object from agent output.
func ExtractJSONObject(raw string) map[string]any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(trimmed), &obj); err == nil && obj != nil {
		return obj
	}

	// Scan for fenced ```json ... ``` block.
	if start := strings.LastIndex(trimmed, "```json"); start >= 0 {
		sub := trimmed[start+len("```json"):]
		if end := strings.Index(sub, "```"); end >= 0 {
			candidate := strings.TrimSpace(sub[:end])
			if err := json.Unmarshal([]byte(candidate), &obj); err == nil && obj != nil {
				return obj
			}
		}
	}

	// Fallback: outermost balanced braces from last '}' backwards.
	end := strings.LastIndexByte(trimmed, '}')
	if end < 0 {
		return nil
	}
	for i := strings.IndexByte(trimmed, '{'); i >= 0 && i < end; {
		candidate := trimmed[i : end+1]
		if err := json.Unmarshal([]byte(candidate), &obj); err == nil && obj != nil {
			return obj
		}
		next := strings.IndexByte(trimmed[i+1:], '{')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return nil
}

func composePrompt(systemPrompt, userPrompt string) string {
	sys := strings.TrimSpace(systemPrompt)
	usr := strings.TrimSpace(userPrompt)
	if sys == "" {
		return usr
	}
	if usr == "" {
		return sys
	}
	return sys + "\n\n" + usr
}

// ResolveExecutionCwd resolves a worktree path if active, otherwise absolute workspaceRoot.
func ResolveExecutionCwd(workspaceRoot string, wt *WorktreeHandle) string {
	if wt != nil && wt.Enabled && strings.TrimSpace(wt.Path) != "" {
		return wt.Path
	}
	abs, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return workspaceRoot
	}
	return abs
}
