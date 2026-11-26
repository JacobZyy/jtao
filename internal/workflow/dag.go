package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	unsafeWorkItemChars = regexp.MustCompile(`[/\\:*?"<>|\x00-\x1f]`)
	slugNonAlnum        = regexp.MustCompile(`[^a-z0-9\p{Han}]+`)
	placeholderRefRegex = regexp.MustCompile(`(?i)artifact-or-command-ref|placeholder|真实存在|\btodo\b|\btbd\b`)
	validRefSchemeRegex = regexp.MustCompile(`^[a-z][a-z0-9+.-]*:.+$`)
)

// CanonicalWorkItemID validates and normalizes a work item ID (fail-closed against path traversal).
func CanonicalWorkItemID(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("work item id must not be empty")
	}
	if strings.Contains(trimmed, "..") || unsafeWorkItemChars.MatchString(trimmed) {
		return "", fmt.Errorf("unsafe work item id: %q", raw)
	}
	slug := slugify(trimmed)
	if slug == "" {
		return "", fmt.Errorf("work item id has no usable characters: %q", raw)
	}
	if len([]rune(slug)) > 80 {
		return "", fmt.Errorf("work item id too long (max 80 chars): %q", raw)
	}
	return slug, nil
}

// CanonicalizeWorkItemID is the non-throwing normalizer used for policy comparisons.
func CanonicalizeWorkItemID(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	return slugify(trimmed)
}

func slugify(text string) string {
	lower := strings.ToLower(strings.TrimSpace(text))
	replaced := slugNonAlnum.ReplaceAllString(lower, "-")
	trimmed := strings.Trim(replaced, "-")
	runes := []rune(trimmed)
	if len(runes) > 64 {
		trimmed = strings.Trim(string(runes[:64]), "-")
	}
	return trimmed
}

// ValidateGraph verifies unique node IDs, valid edge endpoints, no self-loops,
// and an acyclic DAG via Kahn's algorithm.
func ValidateGraph(nodes []TaskNode, edges []TaskEdge) error {
	if len(nodes) == 0 {
		return fmt.Errorf("task graph requires at least one node")
	}
	byID := make(map[string]struct{}, len(nodes))
	indegree := make(map[string]int, len(nodes))
	dependents := make(map[string][]string, len(nodes))

	for _, n := range nodes {
		id := strings.TrimSpace(n.ID)
		if id == "" {
			return fmt.Errorf("task node id must not be empty")
		}
		if _, dup := byID[id]; dup {
			return fmt.Errorf("duplicate task node id: %s", id)
		}
		byID[id] = struct{}{}
		indegree[id] = 0
	}

	edgeSeen := make(map[string]struct{}, len(edges))
	for _, e := range edges {
		from := strings.TrimSpace(e.FromNodeID)
		to := strings.TrimSpace(e.ToNodeID)
		if from == to {
			return fmt.Errorf("self-dependency cycle on node: %s", from)
		}
		if _, ok := byID[from]; !ok {
			return fmt.Errorf("edge references unknown fromNodeId: %s", from)
		}
		if _, ok := byID[to]; !ok {
			return fmt.Errorf("edge references unknown toNodeId: %s", to)
		}
		key := from + "->" + to
		if _, dup := edgeSeen[key]; dup {
			return fmt.Errorf("duplicate dependency edge: %s", key)
		}
		edgeSeen[key] = struct{}{}
		dependents[from] = append(dependents[from], to)
		indegree[to]++
	}

	queue := make([]string, 0, len(nodes))
	for id, deg := range indegree {
		if deg == 0 {
			queue = append(queue, id)
		}
	}
	visited := 0
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		visited++
		for _, next := range dependents[curr] {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if visited != len(nodes) {
		return fmt.Errorf("task graph contains a dependency cycle")
	}
	return nil
}

// CreateWorkItem persists a new WorkItem and its validated Task DAG.
func (s *SQLiteService) CreateWorkItem(ctx context.Context, in CreateWorkItemInput) (WorkItemDetail, error) {
	title := strings.TrimSpace(in.Title)
	objective := strings.TrimSpace(in.Objective)
	if title == "" {
		title = objective
	}
	if title == "" {
		return WorkItemDetail{}, fmt.Errorf("work item title or objective is required")
	}
	if objective == "" {
		objective = title
	}

	rawID := strings.TrimSpace(in.ID)
	if rawID == "" {
		rawID = title
	}
	id, err := CanonicalWorkItemID(rawID)
	if err != nil {
		return WorkItemDetail{}, err
	}

	// Verify baseline receipt when configured (Rex long-running delivery invariant).
	if strings.TrimSpace(in.BaselineReceiptID) != "" && in.BaselineSpec != nil {
		ver, err := s.VerifyReceipt(ctx, in.WorkspaceRoot, in.BaselineReceiptID, *in.BaselineSpec)
		if err != nil {
			return WorkItemDetail{}, err
		}
		if !ver.OK {
			return WorkItemDetail{}, fmt.Errorf("baseline receipt verification failed: %s", ver.Reason)
		}
	}

	nodes := in.Nodes
	edges := in.Edges
	blueprint := strings.TrimSpace(in.Blueprint)
	if len(nodes) == 0 {
		if blueprint == "" {
			blueprint = "single"
		}
		nodes, edges, err = s.ExpandBlueprint(blueprint, objective)
		if err != nil {
			return WorkItemDetail{}, err
		}
	} else if blueprint == "" {
		blueprint = "custom"
	}

	if err := ValidateGraph(nodes, edges); err != nil {
		return WorkItemDetail{}, err
	}

	route := strings.TrimSpace(in.Route)
	if route == "" {
		route = "implement"
	}
	client := strings.TrimSpace(in.OriginClient)
	if client == "" {
		client = "cli"
	}

	db, _, err := s.openDB(ctx, in.WorkspaceRoot)
	if err != nil {
		return WorkItemDetail{}, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return WorkItemDetail{}, err
	}
	defer func() { _ = tx.Rollback() }()

	now := s.nowISO()
	activeInt := 0
	if in.SetActive {
		activeInt = 1
		if _, err := tx.ExecContext(ctx, `UPDATE work_items SET is_active = 0 WHERE is_active = 1`); err != nil {
			return WorkItemDetail{}, err
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO work_items (
			id, title, objective, route, blueprint, status, is_active,
			baseline_receipt_id, origin_client, origin_session_id, plan_md_path,
			metadata_json, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '{}', ?, ?)
	`, id, title, objective, route, blueprint, string(WorkActive), activeInt,
		strings.TrimSpace(in.BaselineReceiptID), client, strings.TrimSpace(in.OriginSessionID), now, now)
	if err != nil {
		return WorkItemDetail{}, fmt.Errorf("insert work_items: %w", err)
	}

	if err := insertGraphTx(ctx, tx, id, nodes, edges, now); err != nil {
		return WorkItemDetail{}, err
	}

	if err := tx.Commit(); err != nil {
		return WorkItemDetail{}, err
	}

	if in.ExportMarkdown {
		_, _, _ = s.ExportMarkdown(ctx, in.WorkspaceRoot, id)
	}

	detail, err := s.GetWorkItem(ctx, in.WorkspaceRoot, id)
	if err != nil {
		return WorkItemDetail{}, err
	}
	return *detail, nil
}

// OpenOrCreateWorkItem idempotently activates an existing WorkItem by canonical ID or creates a new one.
func (s *SQLiteService) OpenOrCreateWorkItem(ctx context.Context, in CreateWorkItemInput) (WorkItemDetail, bool, error) {
	if strings.TrimSpace(in.ID) != "" {
		id, err := CanonicalWorkItemID(in.ID)
		if err != nil {
			return WorkItemDetail{}, false, err
		}
		existing, err := s.GetWorkItem(ctx, in.WorkspaceRoot, id)
		if err != nil {
			return WorkItemDetail{}, false, err
		}
		if existing != nil {
			if in.SetActive && !existing.IsActive {
				db, _, err := s.openDB(ctx, in.WorkspaceRoot)
				if err == nil {
					_, _ = db.ExecContext(ctx, `UPDATE work_items SET is_active = 0 WHERE is_active = 1`)
					_, _ = db.ExecContext(ctx, `UPDATE work_items SET is_active = 1, updated_at = ? WHERE id = ?`, s.nowISO(), id)
					existing.IsActive = true
				}
			}
			return *existing, false, nil
		}
	}
	created, err := s.CreateWorkItem(ctx, in)
	return created, err == nil, err
}

// GetWorkItem loads a WorkItem together with all its TaskNodes and TaskEdges.
func (s *SQLiteService) GetWorkItem(ctx context.Context, workspaceRoot, workItemID string) (*WorkItemDetail, error) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	var wi WorkItem
	var statusStr string
	var isActiveInt int
	err = db.QueryRowContext(ctx, `
		SELECT id, title, objective, route, blueprint, status, is_active,
		       baseline_receipt_id, origin_client, origin_session_id, plan_md_path,
		       created_at, updated_at
		FROM work_items WHERE id = ?
	`, strings.TrimSpace(workItemID)).Scan(
		&wi.ID, &wi.Title, &wi.Objective, &wi.Route, &wi.Blueprint, &statusStr, &isActiveInt,
		&wi.BaselineReceiptID, &wi.OriginClient, &wi.OriginSessionID, &wi.PlanMDPath,
		&wi.CreatedAt, &wi.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	wi.Status = WorkItemStatus(statusStr)
	wi.IsActive = isActiveInt != 0

	nodes, err := loadNodes(ctx, db, wi.ID)
	if err != nil {
		return nil, err
	}
	edges, err := loadEdges(ctx, db, wi.ID)
	if err != nil {
		return nil, err
	}
	return &WorkItemDetail{WorkItem: wi, Nodes: nodes, Edges: edges}, nil
}

// GetActiveWorkItem returns the currently active WorkItem in the workspace, if any.
func (s *SQLiteService) GetActiveWorkItem(ctx context.Context, workspaceRoot string) (*WorkItemDetail, error) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	var id string
	err = db.QueryRowContext(ctx, `SELECT id FROM work_items WHERE is_active = 1 ORDER BY updated_at DESC LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetWorkItem(ctx, workspaceRoot, id)
}

// ListWorkItems returns WorkItems ordered by most recently updated.
func (s *SQLiteService) ListWorkItems(ctx context.Context, workspaceRoot string, statuses []WorkItemStatus, limit int) ([]WorkItem, error) {
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 30
	}
	query := `
		SELECT id, title, objective, route, blueprint, status, is_active,
		       baseline_receipt_id, origin_client, origin_session_id, plan_md_path,
		       created_at, updated_at
		FROM work_items
	`
	var args []any
	if len(statuses) > 0 {
		holders := make([]string, len(statuses))
		for i, st := range statuses {
			holders[i] = "?"
			args = append(args, string(st))
		}
		query += " WHERE status IN (" + strings.Join(holders, ",") + ")"
	}
	query += " ORDER BY is_active DESC, updated_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WorkItem
	for rows.Next() {
		var wi WorkItem
		var statusStr string
		var isActiveInt int
		if err := rows.Scan(
			&wi.ID, &wi.Title, &wi.Objective, &wi.Route, &wi.Blueprint, &statusStr, &isActiveInt,
			&wi.BaselineReceiptID, &wi.OriginClient, &wi.OriginSessionID, &wi.PlanMDPath,
			&wi.CreatedAt, &wi.UpdatedAt,
		); err != nil {
			return nil, err
		}
		wi.Status = WorkItemStatus(statusStr)
		wi.IsActive = isActiveInt != 0
		out = append(out, wi)
	}
	return out, rows.Err()
}

// SetWorkItemStatus transitions a WorkItem's status, enforcing EvaluateDoneGate before done.
func (s *SQLiteService) SetWorkItemStatus(ctx context.Context, workspaceRoot, workItemID string, status WorkItemStatus, force bool) (WorkItemDetail, error) {
	if status == WorkDone && !force {
		gate, err := s.EvaluateDoneGate(ctx, workspaceRoot, workItemID)
		if err != nil {
			return WorkItemDetail{}, err
		}
		if !gate.OK {
			return WorkItemDetail{}, fmt.Errorf("cannot mark work item %s done: %s", workItemID, strings.Join(gate.Reasons, "; "))
		}
	}
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return WorkItemDetail{}, err
	}
	res, err := db.ExecContext(ctx, `UPDATE work_items SET status = ?, updated_at = ? WHERE id = ?`, string(status), s.nowISO(), workItemID)
	if err != nil {
		return WorkItemDetail{}, err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return WorkItemDetail{}, fmt.Errorf("work item not found: %s", workItemID)
	}
	detail, err := s.GetWorkItem(ctx, workspaceRoot, workItemID)
	if err != nil {
		return WorkItemDetail{}, err
	}
	return *detail, nil
}

// ReplaceGraph atomically replaces the DAG nodes and edges for an existing WorkItem.
func (s *SQLiteService) ReplaceGraph(ctx context.Context, workspaceRoot, workItemID string, nodes []TaskNode, edges []TaskEdge) (WorkItemDetail, error) {
	if err := ValidateGraph(nodes, edges); err != nil {
		return WorkItemDetail{}, err
	}
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return WorkItemDetail{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return WorkItemDetail{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM task_edges WHERE work_item_id = ?`, workItemID); err != nil {
		return WorkItemDetail{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_nodes WHERE work_item_id = ?`, workItemID); err != nil {
		return WorkItemDetail{}, err
	}
	now := s.nowISO()
	if err := insertGraphTx(ctx, tx, workItemID, nodes, edges, now); err != nil {
		return WorkItemDetail{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE work_items SET updated_at = ? WHERE id = ?`, now, workItemID); err != nil {
		return WorkItemDetail{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkItemDetail{}, err
	}
	detail, err := s.GetWorkItem(ctx, workspaceRoot, workItemID)
	if err != nil {
		return WorkItemDetail{}, err
	}
	return *detail, nil
}

// ListReadyNodes returns all pending/active nodes whose upstream dependencies are all done or skipped.
func (s *SQLiteService) ListReadyNodes(ctx context.Context, workspaceRoot, workItemID string) ([]TaskNode, error) {
	detail, err := s.GetWorkItem(ctx, workspaceRoot, workItemID)
	if err != nil {
		return nil, err
	}
	if detail == nil {
		return nil, fmt.Errorf("work item not found: %s", workItemID)
	}
	return readyNodesFromDetail(*detail), nil
}

// ClaimNode seals a fresh one-time executionToken onto a ready (or already active) node.
func (s *SQLiteService) ClaimNode(ctx context.Context, workspaceRoot, workItemID, nodeID string) (*ClaimedCommand, error) {
	detail, err := s.GetWorkItem(ctx, workspaceRoot, workItemID)
	if err != nil {
		return nil, err
	}
	if detail == nil {
		return nil, fmt.Errorf("work item not found: %s", workItemID)
	}

	ready := readyNodesFromDetail(*detail)
	if len(ready) == 0 {
		return nil, nil
	}

	var target *TaskNode
	if strings.TrimSpace(nodeID) != "" {
		for i := range ready {
			if ready[i].ID == strings.TrimSpace(nodeID) {
				target = &ready[i]
				break
			}
		}
		if target == nil {
			return nil, fmt.Errorf("node %q is not ready or does not exist in work item %q", nodeID, workItemID)
		}
	} else {
		// Prefer an already active node so retries stay focused on the current node.
		for i := range ready {
			if ready[i].Status == NodeActive {
				target = &ready[i]
				break
			}
		}
		if target == nil {
			target = &ready[0]
		}
	}

	token := newToken("exec")
	now := s.nowISO()
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return nil, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		UPDATE task_nodes SET status = ?, execution_token = ?, updated_at = ?
		WHERE work_item_id = ? AND id = ?
	`, string(NodeActive), token, now, workItemID, target.ID); err != nil {
		return nil, err
	}
	if detail.Status == WorkActive {
		_, _ = tx.ExecContext(ctx, `UPDATE work_items SET status = ?, updated_at = ? WHERE id = ?`, string(WorkExecuting), now, workItemID)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	target.Status = NodeActive
	target.ExecutionToken = token
	target.UpdatedAt = now
	return &ClaimedCommand{
		WorkItemID:     workItemID,
		Node:           *target,
		ExecutionToken: token,
	}, nil
}

// SubmitNodeResult validates the one-time execution token, evidence refs, and optional
// real command receipt, then transitions the DAG cleanly.
func (s *SQLiteService) SubmitNodeResult(ctx context.Context, in SubmitNodeResultInput) (AdvanceNodeResult, error) {
	detail, err := s.GetWorkItem(ctx, in.WorkspaceRoot, in.WorkItemID)
	if err != nil {
		return AdvanceNodeResult{}, err
	}
	if detail == nil {
		return AdvanceNodeResult{}, fmt.Errorf("work item not found: %s", in.WorkItemID)
	}

	var node *TaskNode
	for i := range detail.Nodes {
		if detail.Nodes[i].ID == strings.TrimSpace(in.NodeID) {
			node = &detail.Nodes[i]
			break
		}
	}
	if node == nil {
		return AdvanceNodeResult{}, fmt.Errorf("node not found: %s", in.NodeID)
	}
	if node.ExecutionToken == "" || strings.TrimSpace(in.ExecutionToken) != node.ExecutionToken {
		return AdvanceNodeResult{}, fmt.Errorf("executionToken mismatch for node %s: claim the node first", node.ID)
	}

	evidence := collectUniqueRefs(in.ReceiptID, in.EvidenceRefs, in.Handoff)
	for _, ref := range evidence {
		if err := validateEvidenceRef(ref); err != nil {
			return AdvanceNodeResult{}, err
		}
	}

	status := strings.ToLower(strings.TrimSpace(in.Status))
	if status == "" {
		status = "pass"
	}

	// Non-pass status -> human-gate or blocked immediately.
	if status == "needs-input" {
		return s.commitNodeTransition(ctx, in.WorkspaceRoot, detail, node, NodeBlocked, node.RetryCount, evidence, in.Handoff, DecisionHumanGate, "node requires operator input")
	}
	if status == "blocked" || status == "fail" {
		nextRetry := node.RetryCount + 1
		if nextRetry <= node.MaxRetries {
			return s.commitNodeTransition(ctx, in.WorkspaceRoot, detail, node, NodeActive, nextRetry, evidence, in.Handoff, DecisionRetry, "node reported failure within retry budget")
		}
		return s.commitNodeTransition(ctx, in.WorkspaceRoot, detail, node, NodeBlocked, nextRetry, evidence, in.Handoff, DecisionHumanGate, "node exhausted retry budget")
	}

	// If VerifySpec is configured, real execution receipt verification is mandatory.
	if node.VerifySpec != nil {
		receiptID := strings.TrimSpace(in.ReceiptID)
		if receiptID == "" {
			for _, ref := range evidence {
				if strings.HasPrefix(ref, "receipt:") {
					receiptID = ref
					break
				}
			}
		}
		if receiptID == "" {
			return s.commitNodeTransition(ctx, in.WorkspaceRoot, detail, node, NodeBlocked, node.RetryCount, evidence, in.Handoff, DecisionBlocked, "missing required execution receipt for verification command")
		}
		ver, err := s.VerifyReceipt(ctx, in.WorkspaceRoot, receiptID, *node.VerifySpec)
		if err != nil {
			return AdvanceNodeResult{}, err
		}
		if !ver.OK {
			// Command ran validly but failed exit code -> retry within budget, else human-gate.
			if ver.Receipt != nil && ver.Receipt.Command == node.VerifySpec.Command && ver.Receipt.ExitCode != node.VerifySpec.ExpectedExitCode {
				nextRetry := node.RetryCount + 1
				if nextRetry <= node.MaxRetries {
					return s.commitNodeTransition(ctx, in.WorkspaceRoot, detail, node, NodeActive, nextRetry, evidence, in.Handoff, DecisionRetry, ver.Reason)
				}
				return s.commitNodeTransition(ctx, in.WorkspaceRoot, detail, node, NodeBlocked, nextRetry, evidence, in.Handoff, DecisionHumanGate, "verification failed and retries exhausted: "+ver.Reason)
			}
			// Wrong command, missing receipt, or tampered ref -> fail-closed blocked.
			return s.commitNodeTransition(ctx, in.WorkspaceRoot, detail, node, NodeBlocked, node.RetryCount, evidence, in.Handoff, DecisionBlocked, ver.Reason)
		}
	}

	return s.commitNodeTransition(ctx, in.WorkspaceRoot, detail, node, NodeDone, node.RetryCount, evidence, in.Handoff, DecisionContinue, "node verified and accepted")
}

func (s *SQLiteService) commitNodeTransition(
	ctx context.Context,
	workspaceRoot string,
	detail *WorkItemDetail,
	node *TaskNode,
	nextStatus NodeStatus,
	retryCount int,
	newEvidence []string,
	handoff *NodeHandoff,
	decision AdvanceDecision,
	reason string,
) (AdvanceNodeResult, error) {
	mergedEvidence := mergeUniqueStrings(node.EvidenceRefs, newEvidence)
	now := s.nowISO()

	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return AdvanceNodeResult{}, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return AdvanceNodeResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Seal token consumed on terminal or retry transition so next turn must re-claim.
	_, err = tx.ExecContext(ctx, `
		UPDATE task_nodes
		SET status = ?, retry_count = ?, execution_token = '',
		    handoff_json = ?, evidence_refs_json = ?, updated_at = ?
		WHERE work_item_id = ? AND id = ?
	`, string(nextStatus), retryCount, marshalJSON(handoff, "null"), marshalJSON(mergedEvidence, "[]"), now, detail.ID, node.ID)
	if err != nil {
		return AdvanceNodeResult{}, err
	}

	node.Status = nextStatus
	node.RetryCount = retryCount
	node.ExecutionToken = ""
	node.Handoff = handoff
	node.EvidenceRefs = mergedEvidence
	node.UpdatedAt = now

	// Check if entire graph is now complete.
	allDone := true
	for _, n := range detail.Nodes {
		if n.Status != NodeDone && n.Status != NodeSkipped {
			allDone = false
			break
		}
	}

	var nextIDs []string
	if allDone {
		decision = DecisionCompleted
		reason = "all task nodes verified and completed"
		_, _ = tx.ExecContext(ctx, `UPDATE work_items SET status = ?, updated_at = ? WHERE id = ?`, string(WorkDone), now, detail.ID)
	} else {
		if decision == DecisionBlocked || decision == DecisionHumanGate {
			_, _ = tx.ExecContext(ctx, `UPDATE work_items SET status = ?, updated_at = ? WHERE id = ?`, string(WorkBlocked), now, detail.ID)
		} else {
			_, _ = tx.ExecContext(ctx, `UPDATE work_items SET status = ?, updated_at = ? WHERE id = ?`, string(WorkExecuting), now, detail.ID)
		}
		for _, rn := range readyNodesFromDetail(*detail) {
			nextIDs = append(nextIDs, rn.ID)
		}
	}

	if err := tx.Commit(); err != nil {
		return AdvanceNodeResult{}, err
	}

	if detail.PlanMDPath != "" {
		_, _, _ = s.ExportMarkdown(ctx, workspaceRoot, detail.ID)
	}

	return AdvanceNodeResult{
		Decision:    decision,
		WorkItemID:  detail.ID,
		Node:        *node,
		NextNodeIDs: nextIDs,
		Reason:      reason,
	}, nil
}

// AddEvidence attaches a verified reference to a specific task node.
func (s *SQLiteService) AddEvidence(ctx context.Context, workspaceRoot, workItemID, nodeID, ref string) error {
	if err := validateEvidenceRef(ref); err != nil {
		return err
	}
	detail, err := s.GetWorkItem(ctx, workspaceRoot, workItemID)
	if err != nil {
		return err
	}
	if detail == nil {
		return fmt.Errorf("work item not found: %s", workItemID)
	}
	var existing []string
	found := false
	for _, n := range detail.Nodes {
		if n.ID == nodeID {
			existing = n.EvidenceRefs
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("node not found: %s", nodeID)
	}
	merged := mergeUniqueStrings(existing, []string{strings.TrimSpace(ref)})
	db, _, err := s.openDB(ctx, workspaceRoot)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `
		UPDATE task_nodes SET evidence_refs_json = ?, updated_at = ?
		WHERE work_item_id = ? AND id = ?
	`, marshalJSON(merged, "[]"), s.nowISO(), workItemID, nodeID)
	return err
}

// EvaluateDoneGate verifies all nodes are done/skipped and at least one evidence ref exists.
func (s *SQLiteService) EvaluateDoneGate(ctx context.Context, workspaceRoot, workItemID string) (DoneGateResult, error) {
	detail, err := s.GetWorkItem(ctx, workspaceRoot, workItemID)
	if err != nil {
		return DoneGateResult{}, err
	}
	if detail == nil {
		return DoneGateResult{}, fmt.Errorf("work item not found: %s", workItemID)
	}

	res := DoneGateResult{NodesTotal: len(detail.Nodes)}
	var incomplete []string
	for _, n := range detail.Nodes {
		res.EvidenceRefs += len(n.EvidenceRefs)
		if n.Status == NodeDone || n.Status == NodeSkipped {
			res.NodesDone++
		} else {
			incomplete = append(incomplete, fmt.Sprintf("%s(%s)", n.ID, n.Status))
		}
	}
	if len(incomplete) > 0 {
		res.Reasons = append(res.Reasons, "incomplete nodes: "+strings.Join(incomplete, ", "))
	}
	if res.EvidenceRefs == 0 {
		res.Reasons = append(res.Reasons, "missing verification evidence")
	}
	res.OK = len(res.Reasons) == 0
	return res, nil
}

func validateEvidenceRef(ref string) error {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return fmt.Errorf("evidence ref cannot be empty")
	}
	if placeholderRefRegex.MatchString(trimmed) || !validRefSchemeRegex.MatchString(trimmed) {
		return fmt.Errorf("invalid or placeholder evidence ref %q: must use scheme:value format", ref)
	}
	return nil
}

func readyNodesFromDetail(d WorkItemDetail) []TaskNode {
	statusByID := make(map[string]NodeStatus, len(d.Nodes))
	for _, n := range d.Nodes {
		statusByID[n.ID] = n.Status
	}
	depsByNode := make(map[string][]string)
	for _, e := range d.Edges {
		depsByNode[e.ToNodeID] = append(depsByNode[e.ToNodeID], e.FromNodeID)
	}

	var ready []TaskNode
	for _, n := range d.Nodes {
		if n.Status != NodePending && n.Status != NodeActive {
			continue
		}
		depsOK := true
		for _, depID := range depsByNode[n.ID] {
			st := statusByID[depID]
			if st != NodeDone && st != NodeSkipped {
				depsOK = false
				break
			}
		}
		if depsOK {
			ready = append(ready, n)
		}
	}
	return ready
}

func insertGraphTx(ctx context.Context, tx *sql.Tx, workItemID string, nodes []TaskNode, edges []TaskEdge, now string) error {
	for i, n := range nodes {
		kind := n.NodeKind
		if kind == "" {
			kind = NodeKindTask
		}
		role := strings.TrimSpace(n.Role)
		if role == "" {
			role = "implementer"
		}
		st := n.Status
		if st == "" {
			st = NodePending
		}
		orderIdx := n.OrderIndex
		if orderIdx == 0 {
			orderIdx = i + 1
		}
		maxRetries := n.MaxRetries
		if maxRetries < 0 {
			maxRetries = 0
		}
		canEditInt := 0
		if n.CanEditFiles {
			canEditInt = 1
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO task_nodes (
				id, work_item_id, node_kind, title, objective, role, status, order_index,
				parallel_group, can_edit_files, owned_path_prefixes_json, targets_json,
				acceptance, verify_spec_json, template_stage, retry_count, max_retries,
				execution_token, handoff_json, evidence_refs_json, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?)
		`,
			strings.TrimSpace(n.ID), workItemID, string(kind), strings.TrimSpace(n.Title), strings.TrimSpace(n.Objective),
			role, string(st), orderIdx, strings.TrimSpace(n.ParallelGroup), canEditInt,
			marshalJSON(n.OwnedPathPrefixes, "[]"), marshalJSON(n.Targets, "[]"),
			strings.TrimSpace(n.Acceptance), marshalJSON(n.VerifySpec, "null"), strings.TrimSpace(n.TemplateStage),
			n.RetryCount, maxRetries, marshalJSON(n.Handoff, "null"), marshalJSON(n.EvidenceRefs, "[]"), now, now,
		)
		if err != nil {
			return fmt.Errorf("insert task_node %s: %w", n.ID, err)
		}
	}
	for _, e := range edges {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO task_edges (work_item_id, from_node_id, to_node_id) VALUES (?, ?, ?)
		`, workItemID, strings.TrimSpace(e.FromNodeID), strings.TrimSpace(e.ToNodeID))
		if err != nil {
			return fmt.Errorf("insert task_edge %s->%s: %w", e.FromNodeID, e.ToNodeID, err)
		}
	}
	return nil
}

func loadNodes(ctx context.Context, db *sql.DB, workItemID string) ([]TaskNode, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, work_item_id, node_kind, title, objective, role, status, order_index,
		       parallel_group, can_edit_files, owned_path_prefixes_json, targets_json,
		       acceptance, verify_spec_json, template_stage, retry_count, max_retries,
		       execution_token, handoff_json, evidence_refs_json, created_at, updated_at
		FROM task_nodes WHERE work_item_id = ? ORDER BY order_index ASC, id ASC
	`, workItemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TaskNode
	for rows.Next() {
		var n TaskNode
		var kindStr, statusStr, ownedJSON, targetsJSON, verifyJSON, handoffJSON, evJSON string
		var canEditInt int
		if err := rows.Scan(
			&n.ID, &n.WorkItemID, &kindStr, &n.Title, &n.Objective, &n.Role, &statusStr, &n.OrderIndex,
			&n.ParallelGroup, &canEditInt, &ownedJSON, &targetsJSON,
			&n.Acceptance, &verifyJSON, &n.TemplateStage, &n.RetryCount, &n.MaxRetries,
			&n.ExecutionToken, &handoffJSON, &evJSON, &n.CreatedAt, &n.UpdatedAt,
		); err != nil {
			return nil, err
		}
		n.NodeKind = NodeKind(kindStr)
		n.Status = NodeStatus(statusStr)
		n.CanEditFiles = canEditInt != 0
		n.OwnedPathPrefixes = unmarshalStrings(ownedJSON)
		n.Targets = unmarshalStrings(targetsJSON)
		n.EvidenceRefs = unmarshalStrings(evJSON)
		if verifyJSON != "" && verifyJSON != "null" {
			var spec VerifyCommandSpec
			if err := json.Unmarshal([]byte(verifyJSON), &spec); err == nil {
				n.VerifySpec = &spec
			}
		}
		if handoffJSON != "" && handoffJSON != "null" {
			var h NodeHandoff
			if err := json.Unmarshal([]byte(handoffJSON), &h); err == nil {
				n.Handoff = &h
			}
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func loadEdges(ctx context.Context, db *sql.DB, workItemID string) ([]TaskEdge, error) {
	rows, err := db.QueryContext(ctx, `SELECT from_node_id, to_node_id FROM task_edges WHERE work_item_id = ?`, workItemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskEdge
	for rows.Next() {
		var e TaskEdge
		if err := rows.Scan(&e.FromNodeID, &e.ToNodeID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func collectUniqueRefs(receiptID string, refs []string, handoff *NodeHandoff) []string {
	var all []string
	if strings.TrimSpace(receiptID) != "" {
		all = append(all, strings.TrimSpace(receiptID))
	}
	all = append(all, refs...)
	if handoff != nil {
		all = append(all, handoff.EvidenceRefs...)
	}
	return mergeUniqueStrings(nil, all)
}

func mergeUniqueStrings(base, extra []string) []string {
	seen := make(map[string]struct{}, len(base)+len(extra))
	var out []string
	for _, list := range [][]string{base, extra} {
		for _, item := range list {
			trimmed := strings.TrimSpace(item)
			if trimmed == "" {
				continue
			}
			if _, ok := seen[trimmed]; ok {
				continue
			}
			seen[trimmed] = struct{}{}
			out = append(out, trimmed)
		}
	}
	return out
}
