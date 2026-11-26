package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JacobZyy/jtao/internal/memory"
	"github.com/JacobZyy/jtao/internal/scheduler"
	"github.com/JacobZyy/jtao/internal/workflow"
)

const (
	doneSurfaceWindowSessionStart = 24 * time.Hour
	doneSurfaceWindowUserPrompt   = 2 * time.Hour
)

func dbExists(dir string, err error, name string) bool {
	if err != nil || dir == "" {
		return false
	}
	info, statErr := os.Stat(filepath.Join(dir, name))
	return statErr == nil && !info.IsDir()
}

const offloadMinBytes = 2048

type payload struct {
	Prompt         string `json:"prompt"`
	Message        string `json:"message"`
	Cwd            string `json:"cwd"`
	SessionID      string `json:"session_id"`
	SessionIDCamel string `json:"sessionId"`
	Client         string `json:"client"`
	ExplicitIntent string `json:"explicit_intent"`
	ToolName       string `json:"tool_name"`
	ToolOutput     string `json:"tool_output"`
	Response       string `json:"response"`
}

func parsePayload(raw []byte, defaultWorkspace string) (payload, string, string) {
	var p payload
	_ = json.Unmarshal(raw, &p)

	ws := defaultWorkspace
	if filepath.IsAbs(strings.TrimSpace(p.Cwd)) {
		ws = strings.TrimSpace(p.Cwd)
	}
	sessID := strings.TrimSpace(p.SessionID)
	if sessID == "" {
		sessID = strings.TrimSpace(p.SessionIDCamel)
	}
	return p, ws, sessID
}

// HandleUserPromptSubmit runs fast hit-gated turn recall + active workflow summary.
// Returns `{}` (zero noise) when there are 0 memory hits and no active planned work.
// Never injects unconsumed `<!--memory: ...-->` declaration tags for native chats.
func HandleUserPromptSubmit(ctx context.Context, mem *memory.SQLiteService, wf *workflow.SQLiteService, defaultWorkspace string, stdin []byte) ([]byte, error) {
	p, ws, sessID := parsePayload(stdin, defaultWorkspace)
	msg := strings.TrimSpace(p.Prompt)
	if msg == "" {
		msg = strings.TrimSpace(p.Message)
	}
	if msg == "" || ws == "" {
		return []byte("{}\n"), nil
	}

	memDir, memErr := memory.ResolveMemoryDir(ws)
	wfDir, wfErr := workflow.ResolveWorkflowDir(ws)
	hasMem := dbExists(memDir, memErr, "memory.db")
	hasWF := dbExists(wfDir, wfErr, "workflow.db")
	if !hasMem && !hasWF {
		return []byte("{}\n"), nil
	}

	var sections []string

	// 1. Check active WorkItem / policy summary (read-only; never auto-creates workflow.db in untouched dirs).
	var activeDetail *workflow.WorkItemDetail
	if hasWF {
		activeDetail, _ = wf.GetActiveWorkItem(ctx, ws)
	}
	var active *workflow.WorkItem
	if activeDetail != nil {
		active = &activeDetail.WorkItem
	}
	decision := wf.ClassifyPolicy(workflow.PolicyClassifyInput{
		Message:        msg,
		ExplicitIntent: p.ExplicitIntent,
		Client:         p.Client,
		SessionID:      sessID,
		ActiveItem:     active,
	})

	if activeDetail != nil && activeDetail.Status != workflow.WorkDone && activeDetail.Status != workflow.WorkCancelled {
		ready, _ := wf.ListReadyNodes(ctx, ws, activeDetail.ID)
		nextNode := "(none ready)"
		if len(ready) > 0 {
			nextNode = fmt.Sprintf("%s (%s)", ready[0].ID, ready[0].Title)
		}
		sections = append(sections, fmt.Sprintf(
			"## Active jtao WorkItem\n- id: `%s` status=`%s` route=`%s`\n- next: %s",
			activeDetail.ID, activeDetail.Status, activeDetail.Route, nextNode,
		))
	}
	if hasWF {
		if runNote := formatBackgroundRunSection(ctx, wf, ws, false); runNote != "" {
			sections = append(sections, runNote)
		}
	}

	// 2. Hit-gated memory recall across the whole workspace (do not filter out general project memos when a WorkItem is active).
	if hasMem && decision.Disposition != workflow.DispositionNoop {
		recall, err := mem.RecallForTurn(ctx, memory.TurnRecallInput{
			WorkspaceRoot: ws,
			Message:       msg,
			Agent:         p.Client,
			SessionID:     sessID,
			Limit:         5,
			BudgetChars:   1400,
		})
		if err == nil && recall.Hits > 0 && recall.Text != "" {
			sections = append(sections, recall.Text)
		}
	}

	if len(sections) == 0 {
		return []byte("{}\n"), nil
	}

	additional := strings.Join(sections, "\n\n")
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": additional,
		},
		"additionalContext": additional,
	}
	raw, err := json.Marshal(out)
	return append(raw, '\n'), err
}

// HandleSessionStart injects window-capped pinned project rules and latest continuity.
func HandleSessionStart(ctx context.Context, mem *memory.SQLiteService, wf *workflow.SQLiteService, defaultWorkspace string, stdin []byte) ([]byte, error) {
	_, ws, sessID := parsePayload(stdin, defaultWorkspace)
	if ws == "" {
		return []byte("{}\n"), nil
	}

	memDir, memErr := memory.ResolveMemoryDir(ws)
	wfDir, wfErr := workflow.ResolveWorkflowDir(ws)
	hasMem := dbExists(memDir, memErr, "memory.db")
	hasWF := dbExists(wfDir, wfErr, "workflow.db")
	if !hasMem && !hasWF {
		return []byte("{}\n"), nil
	}

	var sections []string
	if hasMem {
		if pin, err := mem.PinRender(ctx, ws, "default", 1500); err == nil && pin.Text != "" {
			sections = append(sections, pin.Text)
		}
		if sessID != "" {
			if cont, err := mem.GetContinuity(ctx, ws, sessID); err == nil && cont != nil && cont.Summary != "" {
				sections = append(sections, "## Previous Session Continuity\n"+cont.Summary)
			}
		}
	}
	if hasWF {
		if active, err := wf.GetActiveWorkItem(ctx, ws); err == nil && active != nil && active.Status != workflow.WorkDone && active.Status != workflow.WorkCancelled {
			sections = append(sections, fmt.Sprintf("## Active jtao WorkItem\n- id: `%s` status=`%s` title=%s", active.ID, active.Status, active.Title))
		}
		if runNote := formatBackgroundRunSection(ctx, wf, ws, true); runNote != "" {
			sections = append(sections, runNote)
		}
	}

	if len(sections) == 0 {
		return []byte("{}\n"), nil
	}
	additional := strings.Join(sections, "\n\n")
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": additional,
		},
		"additionalContext": additional,
	}
	raw, err := json.Marshal(out)
	return append(raw, '\n'), err
}

// HandleStop performs save-guard checkpoint persistence when a host session stops
// inside a workspace that has already initialized jtao memory.
func HandleStop(ctx context.Context, mem *memory.SQLiteService, defaultWorkspace string, stdin []byte) ([]byte, error) {
	p, ws, sessID := parsePayload(stdin, defaultWorkspace)
	if ws == "" {
		return []byte("{}\n"), nil
	}
	memDir, memErr := memory.ResolveMemoryDir(ws)
	if !dbExists(memDir, memErr, "memory.db") {
		return []byte("{}\n"), nil
	}
	client := strings.TrimSpace(p.Client)
	if client == "" {
		client = "claude"
	}
	summary := strings.TrimSpace(p.Response)
	if summary == "" {
		summary = "Session stopped (save-guard checkpoint)"
	}
	if len([]rune(summary)) > 300 {
		summary = string([]rune(summary)[:300]) + "..."
	}

	sess, _, err := mem.EnsureSession(ctx, ws, sessID, client, filepath.Base(ws), "Interactive session", "")
	if err != nil {
		return nil, err
	}
	cp, err := mem.AddCheckpoint(ctx, ws, memory.Checkpoint{
		SessionID: sess.ID,
		Status:    "running",
		Summary:   summary,
	})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(map[string]any{"checkpointId": cp.ID, "sessionId": sess.ID})
	return append(raw, '\n'), err
}

// HandlePostToolUse offloads oversized tool outputs (>= 2048 bytes) into artifact storage.
func HandlePostToolUse(ctx context.Context, mem *memory.SQLiteService, defaultWorkspace string, stdin []byte) ([]byte, error) {
	p, ws, sessID := parsePayload(stdin, defaultWorkspace)
	if ws == "" || len(p.ToolOutput) < offloadMinBytes {
		return []byte("{}\n"), nil
	}
	art, err := mem.StoreArtifact(ctx, ws, sessID, "", p.ToolName, p.ToolOutput)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"offloaded":   true,
		"artifactRef": art.ID,
		"byteSize":    art.ByteSize,
	}
	raw, err := json.Marshal(out)
	return append(raw, '\n'), err
}

func formatBackgroundRunSection(ctx context.Context, wf *workflow.SQLiteService, ws string, isSessionStart bool) string {
	snap, err := scheduler.InspectRun(ctx, wf, ws, "", "")
	if err != nil || snap == nil {
		return ""
	}
	run := snap.Run

	switch run.Status {
	case "running":
		if snap.LiveProcess {
			pid := 0
			if snap.Lease != nil {
				pid = snap.Lease.OwnerPID
			}
			return fmt.Sprintf(
				"## Background jtao Run (running)\n- run: `%s` mode=`%s` work_item=`%s` progress=%d/%d (worker pid=%d alive)\n- inspect/control: call `scheduler_status` or `scheduler_stop`",
				run.ID, run.Mode, run.WorkItemID, run.CurrentIteration, run.MaxIterations, pid,
			)
		}
		return fmt.Sprintf(
			"## Background jtao Run (orphaned)\n- run: `%s` mode=`%s` work_item=`%s` progress=%d/%d (worker process no longer alive)\n- recover: call `scheduler_resume` or invoke `$jtao-%s`",
			run.ID, run.Mode, run.WorkItemID, run.CurrentIteration, run.MaxIterations, run.Mode,
		)
	case "blocked", "human-gate", "failed", "stopped":
		if snap.WorkItem != nil && (snap.WorkItem.Status == workflow.WorkDone || snap.WorkItem.Status == workflow.WorkCancelled) {
			return ""
		}
		summary := strings.TrimSpace(run.SummaryText)
		if summary == "" {
			summary = run.StopReason
		}
		return fmt.Sprintf(
			"## Background jtao Run (%s)\n- run: `%s` mode=`%s` work_item=`%s` summary=%s\n- next: inspect via `scheduler_status` or continue via `scheduler_resume`",
			run.Status, run.ID, run.Mode, run.WorkItemID, summary,
		)
	case "done":
		updatedAt, parseErr := time.Parse(time.RFC3339Nano, run.UpdatedAt)
		if parseErr != nil {
			return ""
		}
		window := doneSurfaceWindowUserPrompt
		if isSessionStart {
			window = doneSurfaceWindowSessionStart
		}
		if time.Since(updatedAt) > window {
			return ""
		}
		return fmt.Sprintf(
			"## Background jtao Run (done)\n- run: `%s` mode=`%s` work_item=`%s` summary=%s",
			run.ID, run.Mode, run.WorkItemID, strings.TrimSpace(run.SummaryText),
		)
	default:
		return ""
	}
}
