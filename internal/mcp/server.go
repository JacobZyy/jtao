package jtaomcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JacobZyy/jtao/internal/memory"
	"github.com/JacobZyy/jtao/internal/runner"
	"github.com/JacobZyy/jtao/internal/scheduler"
	"github.com/JacobZyy/jtao/internal/workflow"
)

// Config configures the jtao stdio MCP server.
type Config struct {
	DefaultWorkspace string
	Only             string // "" (all) | "memory" | "workflow" | "scheduler"
	BinaryPath       string // Optional jtao executable override for detached scheduler workers
	Memory           *memory.SQLiteService
	Workflow         *workflow.SQLiteService
}

// NewServer registers jtao's memory_*, workflow_*, and scheduler_* MCP tools on a fresh mcp.Server.
func NewServer(cfg Config) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "jtao",
		Version: "0.2.0",
	}, nil)

	only := strings.ToLower(strings.TrimSpace(cfg.Only))
	if only == "" || only == "memory" {
		registerMemoryTools(srv, cfg)
	}
	if only == "" || only == "workflow" {
		registerWorkflowTools(srv, cfg)
	}
	if only == "" || only == "workflow" || only == "scheduler" {
		registerSchedulerTools(srv, cfg)
	}
	return srv
}

// RunStdio runs the MCP server over stdin/stdout until ctx is cancelled or EOF.
func RunStdio(ctx context.Context, cfg Config) error {
	srv := NewServer(cfg)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

func resolveWS(provided, fallback string) (string, error) {
	if ws := strings.TrimSpace(provided); ws != "" {
		return ws, nil
	}
	ws := strings.TrimSpace(fallback)
	if ws == "" {
		return "", fmt.Errorf("workspace_root is required")
	}
	if isNonProjectFallbackDir(ws) {
		return "", fmt.Errorf("workspace_root is required (MCP process cwd %q is not a project workspace; pass the active project's absolute workspace_root)", ws)
	}
	return ws, nil
}

func isNonProjectFallbackDir(dir string) bool {
	cleaned := filepath.Clean(dir)
	if cleaned == "/" || cleaned == "." {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if cleaned == filepath.Clean(home) {
			return true
		}
	}
	slash := filepath.ToSlash(cleaned)
	return strings.Contains(slash, "/.codex/plugins/")
}

// --- Memory MCP Tools ---

type recallArgs struct {
	WorkspaceRoot string   `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	Query         string   `json:"query" jsonschema:"Search query in Chinese, English, or code identifiers"`
	Sources       []string `json:"sources,omitempty" jsonschema:"Sources to query: memo, checkpoint, event"`
	Space         string   `json:"space,omitempty" jsonschema:"Memo space (default: default)"`
	Agent         string   `json:"agent,omitempty" jsonschema:"Caller agent identity"`
	WorkItemID    string   `json:"work_item_id,omitempty" jsonschema:"Optional WorkItem ID filter"`
	Limit         int      `json:"limit,omitempty" jsonschema:"Maximum hits to return (default 8)"`
}

type recallOut struct {
	Hits []memory.RecallHit `json:"hits"`
}

type writeMemoArgs struct {
	WorkspaceRoot string   `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	Text          string   `json:"text" jsonschema:"Durable project fact or decision to record"`
	Space         string   `json:"space,omitempty" jsonschema:"Memo space (default: default)"`
	Scope         string   `json:"scope,omitempty" jsonschema:"project_shared | agent_private | agent_ephemeral"`
	Agent         string   `json:"agent,omitempty" jsonschema:"Caller agent identity"`
	WorkItemID    string   `json:"work_item_id,omitempty" jsonschema:"Associated WorkItem ID"`
	Refs          []string `json:"refs,omitempty" jsonschema:"Related file paths or references"`
	Entities      []string `json:"entities,omitempty" jsonschema:"Key technical entities for 1-hop recall"`
	EvidenceRef   string   `json:"evidence_ref,omitempty" jsonschema:"Single concrete evidence reference"`
	Confidence    string   `json:"confidence,omitempty" jsonschema:"high | medium"`
	Supersedes    []string `json:"supersedes,omitempty" jsonschema:"Older memo IDs superseded by this fact"`
	ValidAt       string   `json:"valid_at,omitempty" jsonschema:"ISO-8601 effective timestamp"`
}

type usefulArgs struct {
	WorkspaceRoot string   `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	MemoIDs       []string `json:"memo_ids" jsonschema:"Memo IDs returned by memory_recall that proved useful"`
	SessionID     string   `json:"session_id,omitempty" jsonschema:"Current session ID"`
	Agent         string   `json:"agent,omitempty" jsonschema:"Caller agent identity"`
}

type pinArgs struct {
	WorkspaceRoot string `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	Action        string `json:"action" jsonschema:"get | set | append | render"`
	Space         string `json:"space,omitempty" jsonschema:"Memo space (default: default)"`
	Content       string `json:"content,omitempty" jsonschema:"Content to set or line to append"`
	MaxChars      int    `json:"max_chars,omitempty" jsonschema:"Max characters when action=render"`
}

type checkpointArgs struct {
	WorkspaceRoot string   `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	SessionID     string   `json:"session_id,omitempty" jsonschema:"Session ID (auto-created if omitted)"`
	Agent         string   `json:"agent,omitempty" jsonschema:"Agent name (default: codex)"`
	WorkItemID    string   `json:"work_item_id,omitempty" jsonschema:"Associated WorkItem ID"`
	Status        string   `json:"status,omitempty" jsonschema:"running | blocked | done"`
	Summary       string   `json:"summary" jsonschema:"Concise checkpoint summary"`
	NextActions   []string `json:"next_actions,omitempty" jsonschema:"Concrete next actions"`
	Artifacts     []string `json:"artifacts,omitempty" jsonschema:"Related receipt or artifact refs"`
}

type sessionPackArgs struct {
	WorkspaceRoot string `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	SessionID     string `json:"session_id,omitempty" jsonschema:"Session ID to pack"`
	WorkItemID    string `json:"work_item_id,omitempty" jsonschema:"Or WorkItem ID to recall full context"`
	MaxChars      int    `json:"max_chars,omitempty" jsonschema:"Max output characters (default 4000)"`
}

type artifactReadArgs struct {
	WorkspaceRoot string `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	ArtifactRef   string `json:"artifact_ref" jsonschema:"Artifact ID (artifact:...)"`
	Offset        int64  `json:"offset,omitempty" jsonschema:"Byte offset"`
	Limit         int64  `json:"limit,omitempty" jsonschema:"Max bytes to read (default 4000)"`
}

func registerMemoryTools(srv *mcp.Server, cfg Config) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory_recall",
		Description: "Search project memos, checkpoints, and session events. Always returns record IDs for memory_useful or supersedes.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in recallArgs) (*mcp.CallToolResult, recallOut, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, recallOut{}, err
		}
		hits, err := cfg.Memory.RecallQuery(ctx, memory.RecallQueryInput{
			WorkspaceRoot: ws,
			Query:         in.Query,
			Sources:       in.Sources,
			Space:         in.Space,
			Agent:         in.Agent,
			WorkItemID:    in.WorkItemID,
			Limit:         in.Limit,
		})
		if err != nil {
			return nil, recallOut{}, err
		}
		return nil, recallOut{Hits: hits}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory_write",
		Description: "Write a durable project memo with optional 5-element metadata and supersedes links.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in writeMemoArgs) (*mcp.CallToolResult, memory.WriteMemoResult, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, memory.WriteMemoResult{}, err
		}
		res, err := cfg.Memory.WriteMemo(ctx, memory.WriteMemoInput{
			WorkspaceRoot:        ws,
			Text:                 in.Text,
			Space:                in.Space,
			Scope:                memory.Scope(in.Scope),
			Agent:                in.Agent,
			WorkItemID:           in.WorkItemID,
			Refs:                 in.Refs,
			Entities:             in.Entities,
			EvidenceRef:          in.EvidenceRef,
			Confidence:           in.Confidence,
			Supersedes:           in.Supersedes,
			ValidAt:              in.ValidAt,
			ReturnSupersedeHints: true,
			Provenance: memory.Provenance{
				ProducerType: "mcp",
				PrincipalID:  in.Agent,
			},
		})
		return nil, res, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory_useful",
		Description: "Record adoption feedback for recalled memo IDs to boost future ranking.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in usefulArgs) (*mcp.CallToolResult, memory.FeedbackResult, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, memory.FeedbackResult{}, err
		}
		res, err := cfg.Memory.MarkUseful(ctx, memory.FeedbackInput{
			WorkspaceRoot: ws,
			MemoIDs:       in.MemoIDs,
			SessionID:     in.SessionID,
			Agent:         in.Agent,
		})
		return nil, res, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "memory_pin",
		Description: "Manage pinned project rules and milestones (action: get | set | append | render).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in pinArgs) (*mcp.CallToolResult, any, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, nil, err
		}
		switch strings.ToLower(strings.TrimSpace(in.Action)) {
		case "", "get":
			entry, err := cfg.Memory.PinGet(ctx, ws, in.Space)
			return nil, entry, err
		case "set":
			entry, err := cfg.Memory.PinSet(ctx, ws, in.Space, in.Content)
			return nil, entry, err
		case "append":
			entry, err := cfg.Memory.PinAppend(ctx, ws, in.Space, in.Content)
			return nil, entry, err
		case "render":
			res, err := cfg.Memory.PinRender(ctx, ws, in.Space, in.MaxChars)
			return nil, res, err
		default:
			return nil, nil, fmt.Errorf("unsupported pin action: %q (use get|set|append|render)", in.Action)
		}
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "session_checkpoint",
		Description: "Append a structured progress checkpoint to a session (auto-ensuring the session if needed).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in checkpointArgs) (*mcp.CallToolResult, memory.Checkpoint, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, memory.Checkpoint{}, err
		}
		sess, _, err := cfg.Memory.EnsureSession(ctx, ws, in.SessionID, in.Agent, "", in.Summary, in.WorkItemID)
		if err != nil {
			return nil, memory.Checkpoint{}, err
		}
		cp, err := cfg.Memory.AddCheckpoint(ctx, ws, memory.Checkpoint{
			SessionID:   sess.ID,
			WorkItemID:  in.WorkItemID,
			Status:      in.Status,
			Summary:     in.Summary,
			NextActions: in.NextActions,
			Artifacts:   in.Artifacts,
		})
		return nil, cp, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "session_pack",
		Description: "Fetch a compact cross-session recovery pack by session_id or work_item_id.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sessionPackArgs) (*mcp.CallToolResult, any, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(in.WorkItemID) != "" {
			res, err := cfg.Memory.RecallForWorkItem(ctx, ws, in.WorkItemID, in.MaxChars)
			return nil, res, err
		}
		if strings.TrimSpace(in.SessionID) == "" {
			latest, err := cfg.Memory.LatestSession(ctx, ws, "")
			if err != nil || latest == nil {
				return nil, nil, fmt.Errorf("no session found; provide session_id or work_item_id")
			}
			in.SessionID = latest.ID
		}
		pack, err := cfg.Memory.PackSession(ctx, ws, in.SessionID, in.MaxChars)
		return nil, pack, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "artifact_read",
		Description: "Read a paginated slice of an offloaded large tool/agent output artifact.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in artifactReadArgs) (*mcp.CallToolResult, memory.ArtifactReadResult, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, memory.ArtifactReadResult{}, err
		}
		res, err := cfg.Memory.ReadArtifact(ctx, ws, in.ArtifactRef, in.Offset, in.Limit)
		return nil, res, err
	})
}

// --- Workflow MCP Tools ---

type autoGateArgs struct {
	WorkspaceRoot  string `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	Message        string `json:"message" jsonschema:"User message or slash command"`
	ExplicitIntent string `json:"explicit_intent,omitempty" jsonschema:"Explicit intent: direct|plan|team|harness|debug|review"`
	Client         string `json:"client,omitempty" jsonschema:"Caller client name (codex|claude)"`
	SessionID      string `json:"session_id,omitempty" jsonschema:"Current session ID"`
	WorkItemID     string `json:"work_item_id,omitempty" jsonschema:"Optional declared WorkItem ID"`
	DryRun         bool   `json:"dry_run,omitempty" jsonschema:"Classify only without persisting"`
}

type autoGateOut struct {
	Decision workflow.PolicyDecision  `json:"decision"`
	WorkItem *workflow.WorkItemDetail `json:"workItem,omitempty"`
}

type createWorkNodeArg struct {
	ID                string                      `json:"id" jsonschema:"Unique node ID within the WorkItem"`
	Title             string                      `json:"title" jsonschema:"Node title"`
	Objective         string                      `json:"objective,omitempty" jsonschema:"Detailed node objective"`
	Role              string                      `json:"role,omitempty" jsonschema:"Role (default: implementer)"`
	NodeKind          string                      `json:"node_kind,omitempty" jsonschema:"task | plan | verify | review | merge"`
	CanEditFiles      *bool                       `json:"can_edit_files,omitempty" jsonschema:"Whether this node may edit repository files (default true)"`
	OwnedPathPrefixes []string                    `json:"owned_path_prefixes,omitempty" jsonschema:"Required for parallel editable nodes in Team mode (e.g. ['internal/auth/'])"`
	Acceptance        string                      `json:"acceptance,omitempty" jsonschema:"Acceptance criteria"`
	VerifySpec        *workflow.VerifyCommandSpec `json:"verify_spec,omitempty" jsonschema:"Optional real verification command spec"`
	DependsOn         []string                    `json:"depends_on,omitempty" jsonschema:"Upstream node IDs"`
}

type workCreateArgs struct {
	WorkspaceRoot  string              `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	ID             string              `json:"id,omitempty" jsonschema:"Canonical slug ID"`
	Title          string              `json:"title" jsonschema:"WorkItem title"`
	Objective      string              `json:"objective,omitempty" jsonschema:"WorkItem objective"`
	Blueprint      string              `json:"blueprint,omitempty" jsonschema:"single | feature | bugfix | refactor | security"`
	Nodes          []createWorkNodeArg `json:"nodes,omitempty" jsonschema:"Custom DAG nodes (overrides blueprint)"`
	SetActive      bool                `json:"set_active,omitempty" jsonschema:"Mark as workspace active WorkItem"`
	ExportMarkdown bool                `json:"export_markdown,omitempty" jsonschema:"Also project docs/plans/<date>-<id>.md"`
}

type workStatusArgs struct {
	WorkspaceRoot string `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	WorkItemID    string `json:"work_item_id,omitempty" jsonschema:"WorkItem ID (defaults to active WorkItem)"`
}

type workStatusOut struct {
	WorkItem   *workflow.WorkItemDetail `json:"workItem,omitempty"`
	ReadyNodes []workflow.TaskNode      `json:"readyNodes,omitempty"`
	DoneGate   *workflow.DoneGateResult `json:"doneGate,omitempty"`
}

type nodeClaimArgs struct {
	WorkspaceRoot string `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	WorkItemID    string `json:"work_item_id,omitempty" jsonschema:"WorkItem ID (defaults to active)"`
	NodeID        string `json:"node_id,omitempty" jsonschema:"Optional specific ready node ID"`
}

type nodeSubmitArgs struct {
	WorkspaceRoot  string   `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	WorkItemID     string   `json:"work_item_id" jsonschema:"WorkItem ID"`
	NodeID         string   `json:"node_id" jsonschema:"Node ID"`
	ExecutionToken string   `json:"execution_token" jsonschema:"One-time execution token from workflow_node_claim"`
	Status         string   `json:"status,omitempty" jsonschema:"pass | blocked | needs-input | fail"`
	Summary        string   `json:"summary" jsonschema:"Outcome summary"`
	ReceiptID      string   `json:"receipt_id,omitempty" jsonschema:"Real receipt:<id> from workflow_receipt_run"`
	EvidenceRefs   []string `json:"evidence_refs,omitempty" jsonschema:"Concrete scheme:value evidence references"`
}

type receiptRunArgs struct {
	WorkspaceRoot string   `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	WorkItemID    string   `json:"work_item_id,omitempty" jsonschema:"Associated WorkItem ID"`
	NodeID        string   `json:"node_id,omitempty" jsonschema:"Associated Node ID"`
	Command       string   `json:"command" jsonschema:"Executable to run (e.g. go, npm, pytest)"`
	Args          []string `json:"args,omitempty" jsonschema:"Command arguments"`
	CwdRel        string   `json:"cwd_rel,omitempty" jsonschema:"Optional relative subdirectory inside workspace"`
}

func registerWorkflowTools(srv *mcp.Server, cfg Config) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "workflow_auto_gate",
		Description: "Run deterministic workflow policy classification and optionally open or create the active WorkItem.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in autoGateArgs) (*mcp.CallToolResult, autoGateOut, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, autoGateOut{}, err
		}
		activeDetail, err := cfg.Workflow.GetActiveWorkItem(ctx, ws)
		if err != nil {
			return nil, autoGateOut{}, err
		}
		var active *workflow.WorkItem
		if activeDetail != nil {
			active = &activeDetail.WorkItem
		}
		decision := cfg.Workflow.ClassifyPolicy(workflow.PolicyClassifyInput{
			Message:        in.Message,
			ExplicitIntent: in.ExplicitIntent,
			Client:         in.Client,
			SessionID:      in.SessionID,
			WorkItemID:     in.WorkItemID,
			ActiveItem:     active,
		})
		out := autoGateOut{Decision: decision}
		if in.DryRun {
			return nil, out, nil
		}
		if decision.Persistence == workflow.PersistReuse && activeDetail != nil {
			out.WorkItem = activeDetail
		} else if decision.Persistence == workflow.PersistCreate {
			created, err := cfg.Workflow.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
				WorkspaceRoot:   ws,
				ID:              in.WorkItemID,
				Title:           decision.CleanedPrompt,
				Objective:       decision.CleanedPrompt,
				Route:           decision.RouteHint,
				OriginClient:    in.Client,
				OriginSessionID: in.SessionID,
				SetActive:       true,
				ExportMarkdown:  true,
			})
			if err != nil {
				return nil, out, err
			}
			out.WorkItem = &created
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "workflow_work_create",
		Description: "Create a WorkItem with either a built-in blueprint or custom dependency-validated Task DAG.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in workCreateArgs) (*mcp.CallToolResult, workflow.WorkItemDetail, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, workflow.WorkItemDetail{}, err
		}
		var nodes []workflow.TaskNode
		var edges []workflow.TaskEdge
		for idx, n := range in.Nodes {
			canEdit := true
			if n.CanEditFiles != nil {
				canEdit = *n.CanEditFiles
			}
			nodes = append(nodes, workflow.TaskNode{
				ID:                n.ID,
				NodeKind:          workflow.NodeKind(n.NodeKind),
				Title:             n.Title,
				Objective:         n.Objective,
				Role:              n.Role,
				OrderIndex:        idx + 1,
				CanEditFiles:      canEdit,
				OwnedPathPrefixes: n.OwnedPathPrefixes,
				Acceptance:        n.Acceptance,
				VerifySpec:        n.VerifySpec,
				MaxRetries:        2,
			})
			for _, dep := range n.DependsOn {
				edges = append(edges, workflow.TaskEdge{FromNodeID: dep, ToNodeID: n.ID})
			}
		}
		detail, err := cfg.Workflow.CreateWorkItem(ctx, workflow.CreateWorkItemInput{
			WorkspaceRoot:  ws,
			ID:             in.ID,
			Title:          in.Title,
			Objective:      in.Objective,
			Blueprint:      in.Blueprint,
			Nodes:          nodes,
			Edges:          edges,
			SetActive:      in.SetActive,
			ExportMarkdown: in.ExportMarkdown,
		})
		return nil, detail, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "workflow_status",
		Description: "Inspect a WorkItem's task DAG, ready nodes, and DoneGate status.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in workStatusArgs) (*mcp.CallToolResult, workStatusOut, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, workStatusOut{}, err
		}
		var detail *workflow.WorkItemDetail
		if strings.TrimSpace(in.WorkItemID) != "" {
			detail, err = cfg.Workflow.GetWorkItem(ctx, ws, in.WorkItemID)
		} else {
			detail, err = cfg.Workflow.GetActiveWorkItem(ctx, ws)
		}
		if err != nil || detail == nil {
			return nil, workStatusOut{}, err
		}
		ready, _ := cfg.Workflow.ListReadyNodes(ctx, ws, detail.ID)
		gate, _ := cfg.Workflow.EvaluateDoneGate(ctx, ws, detail.ID)
		return nil, workStatusOut{WorkItem: detail, ReadyNodes: ready, DoneGate: &gate}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "workflow_node_claim",
		Description: "Claim the next ready node in a WorkItem and issue its one-time execution_token.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nodeClaimArgs) (*mcp.CallToolResult, *workflow.ClaimedCommand, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, nil, err
		}
		wiID := strings.TrimSpace(in.WorkItemID)
		if wiID == "" {
			active, err := cfg.Workflow.GetActiveWorkItem(ctx, ws)
			if err != nil || active == nil {
				return nil, nil, fmt.Errorf("no active work item found")
			}
			wiID = active.ID
		}
		claimed, err := cfg.Workflow.ClaimNode(ctx, ws, wiID, in.NodeID)
		return nil, claimed, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "workflow_node_submit",
		Description: "Submit node completion/failure with execution_token and real receipt/evidence references to advance the DAG.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nodeSubmitArgs) (*mcp.CallToolResult, workflow.AdvanceNodeResult, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, workflow.AdvanceNodeResult{}, err
		}
		res, err := cfg.Workflow.SubmitNodeResult(ctx, workflow.SubmitNodeResultInput{
			WorkspaceRoot:  ws,
			WorkItemID:     in.WorkItemID,
			NodeID:         in.NodeID,
			ExecutionToken: in.ExecutionToken,
			Status:         in.Status,
			Summary:        in.Summary,
			ReceiptID:      in.ReceiptID,
			EvidenceRefs:   in.EvidenceRefs,
			Handoff: &workflow.NodeHandoff{
				Status:       in.Status,
				Summary:      in.Summary,
				EvidenceRefs: in.EvidenceRefs,
			},
		})
		return nil, res, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "workflow_receipt_run",
		Description: "Execute a real verification command in the workspace and persist a tamper-evident receipt:<id>.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in receiptRunArgs) (*mcp.CallToolResult, runner.ReceiptExecOutput, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, runner.ReceiptExecOutput{}, err
		}
		cwd := ws
		if strings.TrimSpace(in.CwdRel) != "" {
			cwd = ws + "/" + strings.TrimSpace(in.CwdRel)
		}
		exec := runner.ReceiptExecutor{
			Sink: func(rctx context.Context, root string, rec runner.ReceiptRecord) (string, error) {
				stored, err := cfg.Workflow.RecordReceipt(rctx, root, workflow.ExecutionReceipt{
					WorkItemID:    rec.WorkItemID,
					NodeID:        rec.NodeID,
					RunID:         rec.RunID,
					Command:       rec.Command,
					Args:          rec.Args,
					Cwd:           rec.Cwd,
					ExitCode:      rec.ExitCode,
					TimedOut:      rec.TimedOut,
					DurationMs:    rec.DurationMs,
					StdoutSHA256:  rec.StdoutSHA256,
					StderrSHA256:  rec.StderrSHA256,
					OutputExcerpt: rec.OutputExcerpt,
					ArtifactRef:   rec.ArtifactRef,
				})
				if err != nil {
					return "", err
				}
				return stored.ID, nil
			},
		}
		if cfg.Memory != nil {
			exec.Offload = func(octx context.Context, root, sessID, wiID, tool, content string) (string, error) {
				art, err := cfg.Memory.StoreArtifact(octx, root, sessID, wiID, tool, content)
				return art.ID, err
			}
		}
		out, err := exec.RunAndRecord(ctx, runner.ReceiptExecInput{
			WorkspaceRoot: ws,
			ExecutionCwd:  cwd,
			WorkItemID:    in.WorkItemID,
			NodeID:        in.NodeID,
			Command:       in.Command,
			Args:          in.Args,
		})
		return nil, out, err
	})
}

// --- Scheduler MCP Tools (Non-Blocking Detached Background Orchestration) ---

type soloStartArgs struct {
	WorkspaceRoot  string `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	WorkItemID     string `json:"work_item_id,omitempty" jsonschema:"WorkItem ID (defaults to active WorkItem)"`
	Provider       string `json:"provider,omitempty" jsonschema:"Agent CLI provider: codex (default) | claude"`
	UseWorktree    bool   `json:"use_worktree,omitempty" jsonschema:"Run inside an isolated git worktree"`
	BaseRef        string `json:"base_ref,omitempty" jsonschema:"Base git ref when use_worktree is true (default: HEAD)"`
	MaxIterations  int    `json:"max_iterations,omitempty" jsonschema:"Maximum Solo iterations (default: 20)"`
	TimeoutSec     int    `json:"timeout_sec,omitempty" jsonschema:"Per-turn timeout in seconds (default: 1800)"`
	Unattended     bool   `json:"unattended,omitempty" jsonschema:"Enable unattended pacing gate (quota/cadence/quiet/safe-bypass)"`
	Quota          int    `json:"quota,omitempty" jsonschema:"Max material turns per rolling quota window (0 = unlimited)"`
	QuotaWindowMin int    `json:"quota_window_min,omitempty" jsonschema:"Rolling quota window in minutes (default: 60)"`
	CadenceBaseSec int    `json:"cadence_base_sec,omitempty" jsonschema:"Base backoff seconds for monitor_wait cadence"`
	CadenceMaxSec  int    `json:"cadence_max_sec,omitempty" jsonschema:"Max backoff seconds for monitor_wait cadence"`
	QuietThreshold int    `json:"quiet_threshold,omitempty" jsonschema:"Consecutive noop turns before quiet stop"`
	SafeBypass     *bool  `json:"safe_bypass,omitempty" jsonschema:"Grant one read-only probe turn before human-gate stop when unattended"`
}

type teamStartArgs struct {
	WorkspaceRoot string `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	WorkItemID    string `json:"work_item_id,omitempty" jsonschema:"WorkItem ID (defaults to active WorkItem)"`
	Provider      string `json:"provider,omitempty" jsonschema:"Agent CLI provider: codex (default) | claude"`
	Concurrency   int    `json:"concurrency,omitempty" jsonschema:"Max concurrent agent nodes (default: 3)"`
	UseWorktree   bool   `json:"use_worktree,omitempty" jsonschema:"Isolate editable nodes in dedicated git worktrees"`
	BaseRef       string `json:"base_ref,omitempty" jsonschema:"Base git ref when use_worktree is true (default: HEAD)"`
	Snapshot      bool   `json:"snapshot,omitempty" jsonschema:"Capture pre-mutation git snapshot before editable nodes"`
	TimeoutSec    int    `json:"timeout_sec,omitempty" jsonschema:"Per-node timeout in seconds (default: 900)"`
}

type schedulerStatusArgs struct {
	WorkspaceRoot string `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	RunID         string `json:"run_id,omitempty" jsonschema:"Specific Run ID (defaults to latest run)"`
	WorkItemID    string `json:"work_item_id,omitempty" jsonschema:"Filter latest run by WorkItem ID"`
}

type schedulerStatusOut struct {
	Found    bool                   `json:"found"`
	Snapshot *scheduler.RunSnapshot `json:"snapshot,omitempty"`
}

type schedulerStopArgs struct {
	WorkspaceRoot string `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	RunID         string `json:"run_id,omitempty" jsonschema:"Run ID to stop (defaults to latest run)"`
	WorkItemID    string `json:"work_item_id,omitempty" jsonschema:"WorkItem ID to resolve latest run"`
	Reason        string `json:"reason,omitempty" jsonschema:"Human-readable stop reason"`
	Immediate     bool   `json:"immediate,omitempty" jsonschema:"Send SIGTERM immediately instead of waiting for next turn boundary"`
}

type schedulerResumeArgs struct {
	WorkspaceRoot string `json:"workspace_root,omitempty" jsonschema:"Absolute path to project workspace"`
	RunID         string `json:"run_id,omitempty" jsonschema:"Run ID to resume or retry-blocked (defaults to latest run)"`
	WorkItemID    string `json:"work_item_id,omitempty" jsonschema:"WorkItem ID to resolve latest run"`
	Provider      string `json:"provider,omitempty" jsonschema:"Optional provider override (codex | claude)"`
	MaxIterations int    `json:"max_iterations,omitempty" jsonschema:"Solo only: optional updated max iterations"`
	Unattended    bool   `json:"unattended,omitempty" jsonschema:"Solo only: enable unattended pacing gate"`
	Concurrency   int    `json:"concurrency,omitempty" jsonschema:"Team only: max concurrent agent nodes"`
	UseWorktree   bool   `json:"use_worktree,omitempty" jsonschema:"Team only: isolate editable nodes in worktrees"`
	Force         bool   `json:"force,omitempty" jsonschema:"Team only: override repeated-block instability guard"`
}

func registerSchedulerTools(srv *mcp.Server, cfg Config) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "scheduler_solo_start",
		Description: "Launch a detached background Solo loop on a WorkItem and return immediately with runId, pid, and logPath.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in soloStartArgs) (*mcp.CallToolResult, scheduler.BackgroundStartResult, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, scheduler.BackgroundStartResult{}, err
		}
		res, err := scheduler.StartBackgroundSolo(ctx, cfg.Workflow, scheduler.BackgroundSoloInput{
			WorkspaceRoot:  ws,
			WorkItemID:     in.WorkItemID,
			Provider:       in.Provider,
			UseWorktree:    in.UseWorktree,
			BaseRef:        in.BaseRef,
			MaxIterations:  in.MaxIterations,
			TimeoutSec:     in.TimeoutSec,
			Unattended:     in.Unattended,
			Quota:          in.Quota,
			QuotaWindowMin: in.QuotaWindowMin,
			CadenceBaseSec: in.CadenceBaseSec,
			CadenceMaxSec:  in.CadenceMaxSec,
			QuietThreshold: in.QuietThreshold,
			SafeBypass:     in.SafeBypass,
			BinaryPath:     cfg.BinaryPath,
		})
		return nil, res, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "scheduler_team_start",
		Description: "Launch a detached background Team DAG run with ownership checks and Merge Gate, returning immediately with runId, pid, and logPath.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in teamStartArgs) (*mcp.CallToolResult, scheduler.BackgroundStartResult, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, scheduler.BackgroundStartResult{}, err
		}
		res, err := scheduler.StartBackgroundTeam(ctx, cfg.Workflow, scheduler.BackgroundTeamInput{
			WorkspaceRoot: ws,
			WorkItemID:    in.WorkItemID,
			Provider:      in.Provider,
			Concurrency:   in.Concurrency,
			UseWorktree:   in.UseWorktree,
			BaseRef:       in.BaseRef,
			Snapshot:      in.Snapshot,
			TimeoutSec:    in.TimeoutSec,
			BinaryPath:    cfg.BinaryPath,
		})
		return nil, res, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "scheduler_status",
		Description: "Inspect a background Solo or Team run's progress, live worker process liveness, DAG nodes, DoneGate, and recent log output.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in schedulerStatusArgs) (*mcp.CallToolResult, schedulerStatusOut, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, schedulerStatusOut{}, err
		}
		snap, err := scheduler.InspectRun(ctx, cfg.Workflow, ws, in.RunID, in.WorkItemID)
		if err != nil {
			return nil, schedulerStatusOut{}, err
		}
		if snap == nil {
			return nil, schedulerStatusOut{Found: false}, nil
		}
		return nil, schedulerStatusOut{Found: true, Snapshot: snap}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "scheduler_stop",
		Description: "Stop a running background Solo or Team run cleanly (graceful at next boundary, or immediate SIGTERM).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in schedulerStopArgs) (*mcp.CallToolResult, *scheduler.StopRunResult, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, nil, err
		}
		res, err := scheduler.RequestStop(ctx, cfg.Workflow, ws, in.RunID, in.WorkItemID, in.Reason, in.Immediate)
		return nil, res, err
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "scheduler_resume",
		Description: "Resume a stopped/blocked/crashed Solo run or retry blocked nodes of a Team run in the background.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in schedulerResumeArgs) (*mcp.CallToolResult, scheduler.BackgroundStartResult, error) {
		ws, err := resolveWS(in.WorkspaceRoot, cfg.DefaultWorkspace)
		if err != nil {
			return nil, scheduler.BackgroundStartResult{}, err
		}
		res, err := scheduler.ResumeBackgroundRun(ctx, cfg.Workflow, scheduler.BackgroundResumeInput{
			WorkspaceRoot: ws,
			RunID:         in.RunID,
			WorkItemID:    in.WorkItemID,
			Provider:      in.Provider,
			MaxIterations: in.MaxIterations,
			Unattended:    in.Unattended,
			Concurrency:   in.Concurrency,
			UseWorktree:   in.UseWorktree,
			Force:         in.Force,
			BinaryPath:    cfg.BinaryPath,
		})
		return nil, res, err
	})
}
