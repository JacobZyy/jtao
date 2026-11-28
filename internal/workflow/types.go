package workflow

import "context"

// PolicyMode controls how aggressively plans are persisted.
type PolicyMode string

const (
	PolicyAdaptive PolicyMode = "adaptive"
	PolicyStrict   PolicyMode = "strict"
)

// Disposition is the deterministic workflow classification of a turn.
type Disposition string

const (
	DispositionNoop    Disposition = "noop"
	DispositionDirect  Disposition = "direct"
	DispositionGuarded Disposition = "guarded"
	DispositionPlanned Disposition = "planned"
	DispositionBlocked Disposition = "blocked"
)

// ExecutionHost identifies which execution layer should own the turn.
type ExecutionHost string

const (
	HostSingle  ExecutionHost = "single"
	HostTeam    ExecutionHost = "team"
	HostHarness ExecutionHost = "harness"
)

// PersistenceAction decides whether a WorkItem should be created or reused.
type PersistenceAction string

const (
	PersistNone   PersistenceAction = "none"
	PersistReuse  PersistenceAction = "reuse"
	PersistCreate PersistenceAction = "create"
)

// PolicyClassifyInput feeds pure turn classification without side effects.
type PolicyClassifyInput struct {
	Message        string
	ExplicitIntent string
	PolicyMode     PolicyMode
	Client         string
	SessionID      string
	WorkItemID     string
	NewWorkItem    bool
	ActiveItem     *WorkItem
}

// PolicyDecision is the deterministic output of Policy.Classify.
type PolicyDecision struct {
	Disposition   Disposition       `json:"disposition"`
	Continuation  string            `json:"continuation"` // none | same-session-ack | explicit-resume | missing
	Persistence   PersistenceAction `json:"persistence"`
	RouteHint     string            `json:"routeHint"`
	ExecutionHost ExecutionHost     `json:"executionHost"`
	Reason        string            `json:"reason"`
	CleanedPrompt string            `json:"cleanedPrompt"`
	ActiveItemID  string            `json:"activeItemId,omitempty"`
}

// WorkItemStatus represents the lifecycle state of a WorkItem.
type WorkItemStatus string

const (
	WorkActive    WorkItemStatus = "active"
	WorkApproved  WorkItemStatus = "approved"
	WorkExecuting WorkItemStatus = "executing"
	WorkDone      WorkItemStatus = "done"
	WorkBlocked   WorkItemStatus = "blocked"
	WorkCancelled WorkItemStatus = "cancelled"
)

// NodeStatus represents the lifecycle state of a single DAG node.
type NodeStatus string

const (
	NodePending NodeStatus = "pending"
	NodeActive  NodeStatus = "active"
	NodeDone    NodeStatus = "done"
	NodeBlocked NodeStatus = "blocked"
	NodeSkipped NodeStatus = "skipped"
	NodeFailed  NodeStatus = "failed"
)

// NodeKind classifies nodes in the unified Task DAG.
type NodeKind string

const (
	NodeKindTask      NodeKind = "task"
	NodeKindPhase     NodeKind = "phase"
	NodeKindMergeGate NodeKind = "merge_gate"
)

// VerifyCommandSpec defines a concrete command that must pass to verify a node.
type VerifyCommandSpec struct {
	Command          string   `json:"command"`
	Args             []string `json:"args,omitempty"`
	CwdRel           string   `json:"cwdRel,omitempty"`
	ExpectedExitCode int      `json:"expectedExitCode"`
}

// NodeHandoff captures structured output from an agent finishing a node.
type NodeHandoff struct {
	Status       string   `json:"status"` // pass | blocked | needs-input | fail
	Summary      string   `json:"summary"`
	Findings     []string `json:"findings,omitempty"`
	Blockers     []string `json:"blockers,omitempty"`
	FilesTouched []string `json:"filesTouched,omitempty"`
	NextSteps    []string `json:"nextSteps,omitempty"`
	EvidenceRefs []string `json:"evidenceRefs,omitempty"`
}

// TaskNode is a single unit of work inside a WorkItem DAG.
type TaskNode struct {
	ID                string             `json:"id"`
	WorkItemID        string             `json:"workItemId"`
	NodeKind          NodeKind           `json:"nodeKind"`
	Title             string             `json:"title"`
	Objective         string             `json:"objective,omitempty"`
	Role              string             `json:"role"`
	Status            NodeStatus         `json:"status"`
	OrderIndex        int                `json:"orderIndex"`
	ParallelGroup     string             `json:"parallelGroup,omitempty"`
	CanEditFiles      bool               `json:"canEditFiles"`
	OwnedPathPrefixes []string           `json:"ownedPathPrefixes,omitempty"`
	Targets           []string           `json:"targets,omitempty"`
	Acceptance        string             `json:"acceptance,omitempty"`
	VerifySpec        *VerifyCommandSpec `json:"verifySpec,omitempty"`
	TemplateStage     string             `json:"templateStage,omitempty"`
	RetryCount        int                `json:"retryCount"`
	MaxRetries        int                `json:"maxRetries"`
	ExecutionToken    string             `json:"executionToken,omitempty"`
	Handoff           *NodeHandoff       `json:"handoff,omitempty"`
	EvidenceRefs      []string           `json:"evidenceRefs,omitempty"`
	CreatedAt         string             `json:"createdAt"`
	UpdatedAt         string             `json:"updatedAt"`
}

// TaskEdge encodes a directed dependency: FromNodeID must complete before ToNodeID.
type TaskEdge struct {
	FromNodeID string `json:"fromNodeId"`
	ToNodeID   string `json:"toNodeId"`
}

// WorkItem is the unified top-level plan and delivery ledger.
type WorkItem struct {
	ID                string         `json:"id"`
	Title             string         `json:"title"`
	Objective         string         `json:"objective"`
	Route             string         `json:"route"`
	Blueprint         string         `json:"blueprint"`
	Status            WorkItemStatus `json:"status"`
	IsActive          bool           `json:"isActive"`
	BaselineReceiptID string         `json:"baselineReceiptId,omitempty"`
	OriginClient      string         `json:"originClient"`
	OriginSessionID   string         `json:"originSessionId,omitempty"`
	PlanMDPath        string         `json:"planMdPath,omitempty"`
	CreatedAt         string         `json:"createdAt"`
	UpdatedAt         string         `json:"updatedAt"`
}

// WorkItemDetail bundles a WorkItem with its full DAG.
type WorkItemDetail struct {
	WorkItem
	Nodes []TaskNode `json:"nodes"`
	Edges []TaskEdge `json:"edges"`
}

// CreateWorkItemInput defines a new WorkItem and optional initial graph.
type CreateWorkItemInput struct {
	WorkspaceRoot     string
	ID                string
	Title             string
	Objective         string
	Route             string
	Blueprint         string
	BaselineReceiptID string
	BaselineSpec      *VerifyCommandSpec
	OriginClient      string
	OriginSessionID   string
	Nodes             []TaskNode
	Edges             []TaskEdge
	SetActive         bool
	ExportMarkdown    bool
}

// ClaimedCommand is the host-sealed execution contract handed to an agent or scheduler.
type ClaimedCommand struct {
	WorkItemID     string   `json:"workItemId"`
	Node           TaskNode `json:"node"`
	ExecutionToken string   `json:"executionToken"`
}

// SubmitNodeResultInput submits evidence and outcome for a claimed node.
type SubmitNodeResultInput struct {
	WorkspaceRoot  string
	WorkItemID     string
	NodeID         string
	ExecutionToken string
	Status         string // pass | blocked | needs-input | fail
	Summary        string
	ReceiptID      string
	EvidenceRefs   []string
	Handoff        *NodeHandoff
}

// AdvanceDecision classifies what the caller should do next after submitting a node result.
type AdvanceDecision string

const (
	DecisionContinue  AdvanceDecision = "continue"
	DecisionRetry     AdvanceDecision = "retry"
	DecisionBlocked   AdvanceDecision = "blocked"
	DecisionHumanGate AdvanceDecision = "human-gate"
	DecisionCompleted AdvanceDecision = "completed"
)

// AdvanceNodeResult reports the post-verification state transition.
type AdvanceNodeResult struct {
	Decision    AdvanceDecision `json:"decision"`
	WorkItemID  string          `json:"workItemId"`
	Node        TaskNode        `json:"node"`
	NextNodeIDs []string        `json:"nextNodeIds,omitempty"`
	Reason      string          `json:"reason"`
}

// DoneGateResult reports whether a WorkItem satisfies completion requirements.
type DoneGateResult struct {
	OK           bool     `json:"ok"`
	Reasons      []string `json:"reasons,omitempty"`
	NodesTotal   int      `json:"nodesTotal"`
	NodesDone    int      `json:"nodesDone"`
	EvidenceRefs int      `json:"evidenceRefs"`
}

// ExecutionReceipt is an immutable record of an actually executed local command.
type ExecutionReceipt struct {
	ID            string   `json:"id"` // receipt:<id>
	WorkItemID    string   `json:"workItemId,omitempty"`
	NodeID        string   `json:"nodeId,omitempty"`
	RunID         string   `json:"runId,omitempty"`
	Command       string   `json:"command"`
	Args          []string `json:"args,omitempty"`
	Cwd           string   `json:"cwd"`
	ExitCode      int      `json:"exitCode"`
	TimedOut      bool     `json:"timedOut"`
	DurationMs    int64    `json:"durationMs"`
	StdoutSHA256  string   `json:"stdoutSha256"`
	StderrSHA256  string   `json:"stderrSha256"`
	OutputExcerpt string   `json:"outputExcerpt,omitempty"`
	ArtifactRef   string   `json:"artifactRef,omitempty"`
	ExecError     string   `json:"execError,omitempty"`
	CreatedAt     string   `json:"createdAt"`
}

// ReceiptVerifyResult reports whether a receipt satisfies a VerifyCommandSpec.
type ReceiptVerifyResult struct {
	OK      bool              `json:"ok"`
	Reason  string            `json:"reason,omitempty"`
	Receipt *ExecutionReceipt `json:"receipt,omitempty"`
}

// BlueprintSummary describes a built-in workflow blueprint.
type BlueprintSummary struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	PhaseCount  int    `json:"phaseCount"`
}

// Service is the fixed decoupled contract of internal/workflow.
type Service interface {
	ClassifyPolicy(in PolicyClassifyInput) PolicyDecision

	CreateWorkItem(ctx context.Context, in CreateWorkItemInput) (WorkItemDetail, error)
	OpenOrCreateWorkItem(ctx context.Context, in CreateWorkItemInput) (WorkItemDetail, bool, error)
	GetWorkItem(ctx context.Context, workspaceRoot, workItemID string) (*WorkItemDetail, error)
	GetActiveWorkItem(ctx context.Context, workspaceRoot string) (*WorkItemDetail, error)
	ListWorkItems(ctx context.Context, workspaceRoot string, statuses []WorkItemStatus, limit int) ([]WorkItem, error)
	SetWorkItemStatus(ctx context.Context, workspaceRoot, workItemID string, status WorkItemStatus, force bool) (WorkItemDetail, error)
	ReplaceGraph(ctx context.Context, workspaceRoot, workItemID string, nodes []TaskNode, edges []TaskEdge) (WorkItemDetail, error)
	ExportMarkdown(ctx context.Context, workspaceRoot, workItemID string) (string, string, error)

	ListReadyNodes(ctx context.Context, workspaceRoot, workItemID string) ([]TaskNode, error)
	ClaimNode(ctx context.Context, workspaceRoot, workItemID, nodeID string) (*ClaimedCommand, error)
	SubmitNodeResult(ctx context.Context, in SubmitNodeResultInput) (AdvanceNodeResult, error)
	AddEvidence(ctx context.Context, workspaceRoot, workItemID, nodeID, ref string) error
	EvaluateDoneGate(ctx context.Context, workspaceRoot, workItemID string) (DoneGateResult, error)

	RecordReceipt(ctx context.Context, workspaceRoot string, r ExecutionReceipt) (ExecutionReceipt, error)
	GetReceipt(ctx context.Context, workspaceRoot, receiptID string) (*ExecutionReceipt, error)
	VerifyReceipt(ctx context.Context, workspaceRoot, receiptID string, spec VerifyCommandSpec) (ReceiptVerifyResult, error)

	ListBlueprints() ([]BlueprintSummary, error)
	ExpandBlueprint(name, objective string) ([]TaskNode, []TaskEdge, error)
}
