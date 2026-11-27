package hook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/JacobZyy/jtao/internal/memory"
	"github.com/JacobZyy/jtao/internal/workflow"
)

type agyPayload struct {
	ConversationID  string   `json:"conversationId"`
	WorkspacePaths  []string `json:"workspacePaths"`
	LastUserInput   string   `json:"lastUserInput"`
	HookEventName   string   `json:"hookEventName"`
	Cwd             string   `json:"cwd"`
	InvocationNum   int      `json:"invocationNum"`
	InitialNumSteps int      `json:"initialNumSteps"`
}

func parseAGYPayload(raw []byte) (agyPayload, string) {
	var p agyPayload
	_ = json.Unmarshal(raw, &p)

	ws := strings.TrimSpace(p.Cwd)
	if !filepath.IsAbs(ws) || isAGYPluginDir(ws) {
		ws = ""
		for _, candidate := range p.WorkspacePaths {
			c := strings.TrimSpace(candidate)
			if filepath.IsAbs(c) && !isAGYPluginDir(c) {
				ws = c
				break
			}
		}
	}
	return p, ws
}

func isAGYPluginDir(dir string) bool {
	slash := filepath.ToSlash(filepath.Clean(dir))
	return strings.Contains(slash, "/.gemini/config/plugins/") || strings.Contains(slash, "/.gemini/antigravity/")
}

func marshalAGYInject(text string) ([]byte, error) {
	if strings.TrimSpace(text) == "" {
		return []byte("{}\n"), nil
	}
	out := map[string]any{
		"injectSteps": []map[string]any{
			{
				"systemMessage": map[string]any{
					"systemMessage": text,
				},
			},
		},
	}
	raw, err := json.Marshal(out)
	return append(raw, '\n'), err
}

// HandleAGYSessionStart implements Antigravity's SessionStart hook contract,
// emitting injectSteps.systemMessage when .jtao exists and silent `{}` otherwise.
func HandleAGYSessionStart(ctx context.Context, mem *memory.SQLiteService, wf *workflow.SQLiteService, stdin []byte) ([]byte, error) {
	p, ws := parseAGYPayload(stdin)
	additional := buildSessionStartContext(ctx, mem, wf, ws, strings.TrimSpace(p.ConversationID))
	return marshalAGYInject(additional)
}

// HandleAGYPreInvocation implements Antigravity's PreInvocation hook contract.
// Because PreInvocation fires on every model call inside a tool loop and injected
// systemMessages persist in the trajectory, it deduplicates per (conversationId, workspace, lastUserInput)
// so each user prompt injects context at most once.
func HandleAGYPreInvocation(ctx context.Context, mem *memory.SQLiteService, wf *workflow.SQLiteService, stdin []byte) ([]byte, error) {
	p, ws := parseAGYPayload(stdin)
	msg := strings.TrimSpace(p.LastUserInput)
	if ws == "" || msg == "" {
		return []byte("{}\n"), nil
	}

	// Fast zero-pollution check before touching dedup state.
	memDir, memErr := memory.ResolveMemoryDir(ws)
	wfDir, wfErr := workflow.ResolveWorkflowDir(ws)
	if !dbExists(memDir, memErr, "memory.db") && !dbExists(wfDir, wfErr, "workflow.db") {
		return []byte("{}\n"), nil
	}

	convID := strings.TrimSpace(p.ConversationID)
	if convID != "" {
		if alreadySeenAGYPrompt(convID, ws, msg) {
			return []byte("{}\n"), nil
		}
	} else if p.InvocationNum > 1 {
		return []byte("{}\n"), nil
	}

	additional := buildTurnContext(ctx, mem, wf, ws, msg, "antigravity", convID, "")
	return marshalAGYInject(additional)
}

func alreadySeenAGYPrompt(convID, ws, msg string) bool {
	sum := sha256.Sum256([]byte(ws + "\n" + msg))
	digest := hex.EncodeToString(sum[:])

	dedupDir := filepath.Join(os.TempDir(), "jtao-agy-dedup")
	safeName := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "..", "_").Replace(convID)
	statePath := filepath.Join(dedupDir, safeName+".sha256")

	if prev, err := os.ReadFile(statePath); err == nil && strings.TrimSpace(string(prev)) == digest {
		return true
	}
	if err := os.MkdirAll(dedupDir, 0o700); err == nil {
		_ = os.WriteFile(statePath, []byte(digest), 0o600)
	}
	return false
}
