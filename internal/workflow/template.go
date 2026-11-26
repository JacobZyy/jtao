package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/JacobZyy/jtao/assets"
)

type blueprintSpecFile struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Roles         map[string]roleCardSpec   `json:"roles"`
	Blueprints    map[string]blueprintEntry `json:"blueprints"`
}

type roleCardSpec struct {
	Responsibility    string   `json:"responsibility"`
	Ownership         string   `json:"ownership"`
	CanEditFiles      bool     `json:"canEditFiles"`
	OwnedPathPrefixes []string `json:"ownedPathPrefixes"`
}

type blueprintEntry struct {
	Description string           `json:"description"`
	Phases      []blueprintPhase `json:"phases"`
}

type blueprintPhase struct {
	ID    string `json:"id"`
	Role  string `json:"role"`
	Mode  string `json:"mode"` // sequential | parallel
	Group string `json:"group,omitempty"`
}

func loadBlueprintSpec() (blueprintSpecFile, error) {
	raw, err := assets.FS.ReadFile("blueprints/blueprints.json")
	if err != nil {
		return blueprintSpecFile{}, fmt.Errorf("read embedded blueprints: %w", err)
	}
	var spec blueprintSpecFile
	if err := json.Unmarshal(raw, &spec); err != nil {
		return blueprintSpecFile{}, fmt.Errorf("parse embedded blueprints: %w", err)
	}
	return spec, nil
}

// ListBlueprints returns all built-in embedded workflow blueprints.
func (s *SQLiteService) ListBlueprints() ([]BlueprintSummary, error) {
	spec, err := loadBlueprintSpec()
	if err != nil {
		return nil, err
	}
	out := make([]BlueprintSummary, 0, len(spec.Blueprints))
	for name, bp := range spec.Blueprints {
		out = append(out, BlueprintSummary{
			Name:        name,
			Description: bp.Description,
			PhaseCount:  len(bp.Phases),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ExpandBlueprint turns a named blueprint into unified TaskNodes and TaskEdges,
// automatically synthesizing merge_gate join nodes for parallel groups.
func (s *SQLiteService) ExpandBlueprint(name, objective string) ([]TaskNode, []TaskEdge, error) {
	spec, err := loadBlueprintSpec()
	if err != nil {
		return nil, nil, err
	}
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		key = "single"
	}
	bp, ok := spec.Blueprints[key]
	if !ok {
		return nil, nil, fmt.Errorf("unknown blueprint: %q", name)
	}

	var nodes []TaskNode
	var edges []TaskEdge
	var upstream []string
	var openGroup string
	var openGroupUpstream []string
	var openGroupMembers []string
	order := 0

	flushGroup := func() {
		if openGroup == "" {
			return
		}
		if len(openGroupMembers) > 1 {
			gateID := "merge." + openGroup
			order++
			nodes = append(nodes, TaskNode{
				ID:            gateID,
				NodeKind:      NodeKindMergeGate,
				Title:         "Merge gate: " + openGroup,
				Objective:     "Verify parallel handoffs, file ownership, and absence of conflicts for group " + openGroup,
				Role:          "merge-gate",
				Status:        NodePending,
				OrderIndex:    order,
				ParallelGroup: openGroup,
				CanEditFiles:  false,
				MaxRetries:    1,
			})
			for _, memberID := range openGroupMembers {
				edges = append(edges, TaskEdge{FromNodeID: memberID, ToNodeID: gateID})
			}
			upstream = []string{gateID}
		} else {
			upstream = append([]string(nil), openGroupMembers...)
		}
		openGroup = ""
		openGroupUpstream = nil
		openGroupMembers = nil
	}

	for _, ph := range bp.Phases {
		roleCard, exists := spec.Roles[ph.Role]
		if !exists {
			return nil, nil, fmt.Errorf("blueprint %q references unknown role %q", key, ph.Role)
		}
		order++
		node := TaskNode{
			ID:                ph.ID,
			NodeKind:          NodeKindPhase,
			Title:             fmt.Sprintf("%s (%s)", ph.ID, ph.Role),
			Objective:         strings.TrimSpace(objective + " — " + roleCard.Responsibility),
			Role:              ph.Role,
			Status:            NodePending,
			OrderIndex:        order,
			ParallelGroup:     ph.Group,
			CanEditFiles:      roleCard.CanEditFiles,
			OwnedPathPrefixes: append([]string(nil), roleCard.OwnedPathPrefixes...),
			Acceptance:        roleCard.Ownership,
			MaxRetries:        2,
		}

		if ph.Mode == "parallel" && ph.Group != "" {
			if openGroup != ph.Group {
				flushGroup()
				openGroup = ph.Group
				openGroupUpstream = append([]string(nil), upstream...)
			}
			nodes = append(nodes, node)
			for _, dep := range openGroupUpstream {
				edges = append(edges, TaskEdge{FromNodeID: dep, ToNodeID: node.ID})
			}
			openGroupMembers = append(openGroupMembers, node.ID)
			continue
		}

		flushGroup()
		nodes = append(nodes, node)
		for _, dep := range upstream {
			edges = append(edges, TaskEdge{FromNodeID: dep, ToNodeID: node.ID})
		}
		upstream = []string{node.ID}
	}
	flushGroup()

	return nodes, edges, nil
}

// ExportMarkdown renders a one-way human-readable Markdown projection of a WorkItem
// under <workspaceRoot>/docs/plans/<date>-<id>.md.
func (s *SQLiteService) ExportMarkdown(ctx context.Context, workspaceRoot, workItemID string) (string, string, error) {
	detail, err := s.GetWorkItem(ctx, workspaceRoot, workItemID)
	if err != nil {
		return "", "", err
	}
	if detail == nil {
		return "", "", fmt.Errorf("work item not found: %s", workItemID)
	}

	md := renderWorkItemMarkdown(*detail)
	relPath := detail.PlanMDPath
	if relPath == "" {
		datePrefix := detail.CreatedAt
		if len(datePrefix) >= 10 {
			datePrefix = datePrefix[:10]
		} else {
			datePrefix = s.now().Format("2006-01-02")
		}
		relPath = filepath.ToSlash(filepath.Join("docs", "plans", fmt.Sprintf("%s-%s.md", datePrefix, detail.ID)))
	}

	absPath := filepath.Join(workspaceRoot, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return "", "", fmt.Errorf("create plans dir: %w", err)
	}
	tmpPath := absPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(md), 0o644); err != nil {
		return "", "", fmt.Errorf("write markdown projection: %w", err)
	}
	if err := os.Rename(tmpPath, absPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", "", fmt.Errorf("commit markdown projection: %w", err)
	}

	db, _, err := s.openDB(ctx, workspaceRoot)
	if err == nil && detail.PlanMDPath != relPath {
		_, _ = db.ExecContext(ctx, `UPDATE work_items SET plan_md_path = ?, updated_at = ? WHERE id = ?`, relPath, s.nowISO(), detail.ID)
	}

	return relPath, md, nil
}

func renderWorkItemMarkdown(d WorkItemDetail) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", d.Title)
	fmt.Fprintf(&b, "> jtao WorkItem Projection (`%s`)\n", d.ID)
	fmt.Fprintf(&b, "> status: `%s` | route: `%s` | blueprint: `%s`\n", d.Status, d.Route, d.Blueprint)
	fmt.Fprintf(&b, "> updated: %s\n\n", d.UpdatedAt)
	fmt.Fprintf(&b, "## Objective\n\n%s\n\n", d.Objective)

	depsByNode := make(map[string][]string)
	for _, e := range d.Edges {
		depsByNode[e.ToNodeID] = append(depsByNode[e.ToNodeID], e.FromNodeID)
	}

	b.WriteString("## Task DAG\n\n")
	for _, n := range d.Nodes {
		box := "[ ]"
		if n.Status == NodeDone || n.Status == NodeSkipped {
			box = "[x]"
		}
		fmt.Fprintf(&b, "- %s **%s** (`%s` · `%s` · role=`%s`): %s\n", box, n.ID, n.Status, n.NodeKind, n.Role, n.Title)
		if deps := depsByNode[n.ID]; len(deps) > 0 {
			fmt.Fprintf(&b, "  - dependsOn: %s\n", strings.Join(deps, ", "))
		}
		if n.Acceptance != "" {
			fmt.Fprintf(&b, "  - acceptance: %s\n", n.Acceptance)
		}
		if n.VerifySpec != nil {
			cmd := strings.TrimSpace(n.VerifySpec.Command + " " + strings.Join(n.VerifySpec.Args, " "))
			fmt.Fprintf(&b, "  - verify: `%s` (exit=%d)\n", cmd, n.VerifySpec.ExpectedExitCode)
		}
		if len(n.EvidenceRefs) > 0 {
			fmt.Fprintf(&b, "  - evidence: %s\n", strings.Join(n.EvidenceRefs, ", "))
		}
	}
	return b.String()
}
