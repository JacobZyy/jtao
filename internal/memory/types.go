package memory

import "context"

// Scope defines visibility boundaries for a memo entry.
type Scope string

const (
	ScopeProjectShared Scope = "project_shared"
	ScopeAgentPrivate  Scope = "agent_private"
	ScopeAgentEphemeral Scope = "agent_ephemeral"
)

// ClaimStatus distinguishes verified conclusions from unpromoted candidates.
type ClaimStatus string

const (
	ClaimVerified  ClaimStatus = "verified"
	ClaimCandidate ClaimStatus = "candidate"
	ClaimObserved  ClaimStatus = "observed"
)

// Provenance records where a memo entry originated.
type Provenance struct {
	ProducerType string `json:"producerType"`
	PrincipalID  string `json:"principalId,omitempty"`
	SessionID    string `json:"sessionId,omitempty"`
	RunID        string `json:"runId,omitempty"`
	SourceRef    string `json:"sourceRef,omitempty"`
	SourceHash   string `json:"sourceHash,omitempty"`
}

// MemoEvent is an immutable append-only project knowledge entry.
type MemoEvent struct {
	ID          string      `json:"id"`
	Seq         int64       `json:"seq"`
	Space       string      `json:"space"`
	Scope       Scope       `json:"scope"`
	Agent       string      `json:"agent,omitempty"`
	WorkItemID  string      `json:"workItemId,omitempty"`
	Text        string      `json:"text"`
	Refs        []string    `json:"refs,omitempty"`
	Entities    []string    `json:"entities,omitempty"`
	EvidenceRef string      `json:"evidenceRef,omitempty"`
	Confidence  string      `json:"confidence,omitempty"`
	ClaimStatus ClaimStatus `json:"claimStatus"`
	Archived    bool        `json:"archived"`
	PromotionOf string      `json:"promotionOf,omitempty"`
	Supersedes  []string    `json:"supersedes,omitempty"`
	Provenance  Provenance  `json:"provenance"`
	ValidAt     string      `json:"validAt"`
	CreatedAt   string      `json:"createdAt"`
	SourceRef   string      `json:"sourceRef,omitempty"`
}

// WriteMemoInput describes a new memo to persist.
type WriteMemoInput struct {
	WorkspaceRoot        string
	Text                 string
	Space                string
	Scope                Scope
	Agent                string
	WorkItemID           string
	Refs                 []string
	Entities             []string
	EvidenceRef          string
	Confidence           string
	ClaimStatus          ClaimStatus
	Supersedes           []string
	ValidAt              string
	SourceRef            string
	Provenance           Provenance
	ReturnSupersedeHints bool
	AllowSecretBypass    bool
}

// SupersedeHint points to an existing active memo that may be outdated by a new write.
type SupersedeHint struct {
	ID    string  `json:"id"`
	Text  string  `json:"text"`
	Score float64 `json:"score"`
}

// WriteMemoResult summarizes a memo write operation.
type WriteMemoResult struct {
	Event          MemoEvent       `json:"event"`
	Deduplicated   bool            `json:"deduplicated"`
	SupersedeHints []SupersedeHint `json:"supersedeHints,omitempty"`
	ScanFindings   []ScanFinding   `json:"scanFindings,omitempty"`
}

// ListMemosInput filters memo entries.
type ListMemosInput struct {
	WorkspaceRoot     string
	Space             string
	Scope             Scope
	Agent             string
	WorkItemID        string
	ClaimStatus       ClaimStatus
	IncludeSuperseded bool
	IncludeArchived   bool
	IncludeExpired    bool
	Limit             int
}

// SearchMemosInput queries project memos.
type SearchMemosInput struct {
	WorkspaceRoot string
	Query         string
	Space         string
	Agent         string
	WorkItemID    string
	Limit         int
	IncludeAll    bool
}

// MemoSearchHit pairs a memo with its composite relevance score.
type MemoSearchHit struct {
	Event       MemoEvent `json:"event"`
	Score       float64   `json:"score"`
	MatchReason string    `json:"matchReason"`
}

// FeedbackInput marks recalled memos as useful.
type FeedbackInput struct {
	WorkspaceRoot string
	MemoIDs       []string
	SessionID     string
	Agent         string
	TurnID        string
	AllowedIDs    []string
}

// FeedbackResult reports which memo IDs were accepted for usefulness tracking.
type FeedbackResult struct {
	AcceptedIDs []string `json:"acceptedIds"`
	IgnoredIDs  []string `json:"ignoredIds,omitempty"`
}

// PinEntry holds a space's pinned rules and milestones.
type PinEntry struct {
	Space       string `json:"space"`
	Content     string `json:"content"`
	ContentHash string `json:"contentHash"`
	UpdatedAt   string `json:"updatedAt"`
}

// PinRenderResult is the window-capped pinned context injected at session start.
type PinRenderResult struct {
	Space       string `json:"space"`
	Text        string `json:"text"`
	ContentHash string `json:"contentHash"`
	Truncated   bool   `json:"truncated"`
}

// Session represents a coding agent work session in a workspace.
type Session struct {
	ID         string `json:"id"`
	Agent      string `json:"agent"`
	Project    string `json:"project"`
	Goal       string `json:"goal"`
	WorkItemID string `json:"workItemId,omitempty"`
	Status     string `json:"status"`
	Tags       []string `json:"tags,omitempty"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

// SessionEvent is an append-only timeline entry within a session.
type SessionEvent struct {
	ID         string   `json:"id"`
	SessionID  string   `json:"sessionId"`
	Seq        int64    `json:"seq"`
	Role       string   `json:"role"`
	Kind       string   `json:"kind"`
	Text       string   `json:"text"`
	Refs       []string `json:"refs,omitempty"`
	WorkItemID string   `json:"workItemId,omitempty"`
	TurnID     string   `json:"turnId,omitempty"`
	Outcome    string   `json:"outcome,omitempty"`
	CreatedAt  string   `json:"createdAt"`
}

// Checkpoint records a structured milestone within a session.
type Checkpoint struct {
	ID           string   `json:"id"`
	SessionID    string   `json:"sessionId"`
	Seq          int64    `json:"seq"`
	WorkItemID   string   `json:"workItemId,omitempty"`
	Status       string   `json:"status"`
	Summary      string   `json:"summary"`
	NextActions  []string `json:"nextActions,omitempty"`
	Artifacts    []string `json:"artifacts,omitempty"`
	VerifyResult string   `json:"verifyResult,omitempty"`
	CreatedAt    string   `json:"createdAt"`
}

// Continuity stores compact session resume state.
type Continuity struct {
	SessionID    string   `json:"sessionId"`
	WorkItemID   string   `json:"workItemId,omitempty"`
	Intent       string   `json:"intent"`
	Summary      string   `json:"summary"`
	TouchedFiles []string `json:"touchedFiles,omitempty"`
	NextActions  []string `json:"nextActions,omitempty"`
	UpdatedAt    string   `json:"updatedAt"`
}

// HandoffPacket transfers structured context across agents or runs.
type HandoffPacket struct {
	SessionID    string   `json:"sessionId"`
	WorkItemID   string   `json:"workItemId,omitempty"`
	FromRole     string   `json:"fromRole"`
	ToRole       string   `json:"toRole,omitempty"`
	Status       string   `json:"status"`
	Summary      string   `json:"summary"`
	Findings     []string `json:"findings,omitempty"`
	Blockers     []string `json:"blockers,omitempty"`
	TouchedFiles []string `json:"touchedFiles,omitempty"`
	NextActions  []string `json:"nextActions,omitempty"`
	EvidenceRefs []string `json:"evidenceRefs,omitempty"`
	UpdatedAt    string   `json:"updatedAt"`
}

// SessionPack is a compact cross-session recovery payload.
type SessionPack struct {
	Session     *Session       `json:"session,omitempty"`
	Pinned      *PinRenderResult `json:"pinned,omitempty"`
	Continuity  *Continuity    `json:"continuity,omitempty"`
	Handoff     *HandoffPacket `json:"handoff,omitempty"`
	Checkpoints []Checkpoint   `json:"checkpoints,omitempty"`
	Memos       []MemoEvent    `json:"memos,omitempty"`
	Text        string         `json:"text"`
}

// RecallQueryInput queries unified memory sources.
type RecallQueryInput struct {
	WorkspaceRoot string
	Query         string
	Sources       []string // memo | checkpoint | event (default: memo, checkpoint)
	Space         string
	Agent         string
	WorkItemID    string
	Limit         int
}

// RecallHit is a normalized item returned by unified recall (always carries ID).
type RecallHit struct {
	ID         string   `json:"id"`
	Source     string   `json:"source"` // memo | checkpoint | event
	Text       string   `json:"text"`
	Score      float64  `json:"score"`
	Refs       []string `json:"refs,omitempty"`
	WorkItemID string   `json:"workItemId,omitempty"`
	CreatedAt  string   `json:"createdAt"`
}

// TurnRecallInput collects per-turn prompt injection context.
type TurnRecallInput struct {
	WorkspaceRoot string
	Message       string
	Space         string
	Agent         string
	SessionID     string
	WorkItemID    string
	Limit         int
	BudgetChars   int
}

// TurnRecallResult returns strictly hit-gated recall text (empty when Hits == 0).
type TurnRecallResult struct {
	Status     string      `json:"status"` // ok | no-hits | skipped
	Hits       int         `json:"hits"`
	Items      []RecallHit `json:"items,omitempty"`
	ExposedIDs []string    `json:"exposedIds,omitempty"`
	Text       string      `json:"text"`
}

// WorkItemRecallResult returns all memory linked to a workItemId.
type WorkItemRecallResult struct {
	WorkItemID  string           `json:"workItemId"`
	Pinned      *PinRenderResult `json:"pinned,omitempty"`
	Memos       []MemoEvent      `json:"memos,omitempty"`
	Checkpoints []Checkpoint     `json:"checkpoints,omitempty"`
	Continuity  *Continuity      `json:"continuity,omitempty"`
	Handoff     *HandoffPacket   `json:"handoff,omitempty"`
	Text        string           `json:"text"`
}

// ScanFinding reports a detected secret or unsafe content pattern.
type ScanFinding struct {
	RuleID   string `json:"ruleId"`
	Severity string `json:"severity"` // block | warn
	Detail   string `json:"detail"`
}

// ScanResult summarizes content safety validation.
type ScanResult struct {
	Blocked  bool          `json:"blocked"`
	Findings []ScanFinding `json:"findings,omitempty"`
}

// Artifact stores offloaded large tool or agent outputs outside SQLite.
type Artifact struct {
	ID         string `json:"id"` // artifact:<id>
	SessionID  string `json:"sessionId,omitempty"`
	WorkItemID string `json:"workItemId,omitempty"`
	Tool       string `json:"tool,omitempty"`
	SHA256     string `json:"sha256"`
	ByteSize   int64  `json:"byteSize"`
	RelPath    string `json:"relPath"`
	Preview    string `json:"preview"`
	CreatedAt  string `json:"createdAt"`
}

// ArtifactReadResult returns a slice of an offloaded artifact file.
type ArtifactReadResult struct {
	Artifact Artifact `json:"artifact"`
	Offset   int64    `json:"offset"`
	Limit    int64    `json:"limit"`
	Content  string   `json:"content"`
	HasMore  bool     `json:"hasMore"`
}

// ArtifactGrepMatch represents one matching line inside an offloaded artifact.
type ArtifactGrepMatch struct {
	LineNumber int    `json:"lineNumber"`
	Line       string `json:"line"`
}

// HygieneReport summarizes memory store hygiene metrics and optional cleanup actions.
type HygieneReport struct {
	ActiveMemos           int64 `json:"activeMemos"`
	SupersededMemos       int64 `json:"supersededMemos"`
	ArchivedMemos         int64 `json:"archivedMemos"`
	PendingCandidates     int64 `json:"pendingCandidates"`
	ExpiredPrivateMemos   int64 `json:"expiredPrivateMemos"`
	StaleActiveSessions   int64 `json:"staleActiveSessions"`
	OrphanArtifactFiles   int   `json:"orphanArtifactFiles"`
	CleanedOrphanFiles    int   `json:"cleanedOrphanFiles"`
	ArchivedStaleSessions int64 `json:"archivedStaleSessions"`
}

// DoctorReport describes memory store health.
type DoctorReport struct {
	OK              bool   `json:"ok"`
	DBPath          string `json:"dbPath"`
	JournalMode     string `json:"journalMode"`
	FTS5Available   bool   `json:"fts5Available"`
	MemoCount       int64  `json:"memoCount"`
	SessionCount    int64  `json:"sessionCount"`
	ArtifactCount   int64  `json:"artifactCount"`
	IntegrityResult string `json:"integrityResult"`
}

// Service is the fixed decoupled contract of internal/memory.
type Service interface {
	// Memo subdomain
	WriteMemo(ctx context.Context, in WriteMemoInput) (WriteMemoResult, error)
	GetMemo(ctx context.Context, workspaceRoot, memoID string) (*MemoEvent, error)
	ListMemos(ctx context.Context, in ListMemosInput) ([]MemoEvent, error)
	SearchMemos(ctx context.Context, in SearchMemosInput) ([]MemoSearchHit, error)
	MarkUseful(ctx context.Context, in FeedbackInput) (FeedbackResult, error)
	PinGet(ctx context.Context, workspaceRoot, space string) (*PinEntry, error)
	PinSet(ctx context.Context, workspaceRoot, space, content string) (PinEntry, error)
	PinAppend(ctx context.Context, workspaceRoot, space, line string) (PinEntry, error)
	PinRender(ctx context.Context, workspaceRoot, space string, maxChars int) (PinRenderResult, error)

	// Session subdomain
	CreateSession(ctx context.Context, workspaceRoot, agent, project, goal, workItemID string) (Session, error)
	EnsureSession(ctx context.Context, workspaceRoot, sessionID, agent, project, goal, workItemID string) (Session, bool, error)
	GetSession(ctx context.Context, workspaceRoot, sessionID string) (*Session, error)
	LatestSession(ctx context.Context, workspaceRoot, agent string) (*Session, error)
	ListSessions(ctx context.Context, workspaceRoot string, limit int) ([]Session, error)
	AppendEvent(ctx context.Context, workspaceRoot string, ev SessionEvent) (SessionEvent, error)
	AddCheckpoint(ctx context.Context, workspaceRoot string, cp Checkpoint) (Checkpoint, error)
	GetContinuity(ctx context.Context, workspaceRoot, sessionID string) (*Continuity, error)
	UpdateContinuity(ctx context.Context, workspaceRoot string, c Continuity) (Continuity, error)
	WriteHandoff(ctx context.Context, workspaceRoot string, h HandoffPacket) (HandoffPacket, error)
	ReadHandoff(ctx context.Context, workspaceRoot, sessionID string) (*HandoffPacket, error)
	PackSession(ctx context.Context, workspaceRoot, sessionID string, maxChars int) (SessionPack, error)
	CloseSession(ctx context.Context, workspaceRoot, sessionID string) error

	// Recall subdomain
	RecallQuery(ctx context.Context, in RecallQueryInput) ([]RecallHit, error)
	RecallForTurn(ctx context.Context, in TurnRecallInput) (TurnRecallResult, error)
	RecallForWorkItem(ctx context.Context, workspaceRoot, workItemID string, maxChars int) (WorkItemRecallResult, error)

	// Governance subdomain
	ScanContent(text string) ScanResult
	ListCandidates(ctx context.Context, workspaceRoot, space string, limit int) ([]MemoEvent, error)
	PromoteCandidate(ctx context.Context, workspaceRoot, candidateID, actor, note string) (MemoEvent, error)
	RejectCandidate(ctx context.Context, workspaceRoot, candidateID, actor, note string) error
	ArchiveMemo(ctx context.Context, workspaceRoot, memoID, actor, note string, archived bool) error

	// Artifact subdomain
	StoreArtifact(ctx context.Context, workspaceRoot, sessionID, workItemID, tool, content string) (Artifact, error)
	ReadArtifact(ctx context.Context, workspaceRoot, artifactRef string, offset, limit int64) (ArtifactReadResult, error)
	GrepArtifact(ctx context.Context, workspaceRoot, artifactRef, pattern string, maxMatches int) ([]ArtifactGrepMatch, error)
	ListArtifacts(ctx context.Context, workspaceRoot, sessionID, workItemID string, limit int) ([]Artifact, error)

	// Admin subdomain
	Doctor(ctx context.Context, workspaceRoot string) (DoctorReport, error)
	Hygiene(ctx context.Context, workspaceRoot string, staleDays int, apply bool) (HygieneReport, error)
	RebuildIndex(ctx context.Context, workspaceRoot string) error
	ExportJSONL(ctx context.Context, workspaceRoot string) (string, error)
}
