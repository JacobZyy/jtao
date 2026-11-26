package scheduler

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/JacobZyy/jtao/internal/workflow"
)

// OwnershipViolation identifies a file modified outside a node's permitted scope.
type OwnershipViolation struct {
	NodeID            string   `json:"nodeId"`
	Role              string   `json:"role"`
	File              string   `json:"file"`
	CanEditFiles      bool     `json:"canEditFiles"`
	OwnedPathPrefixes []string `json:"ownedPathPrefixes,omitempty"`
}

// FileConflict identifies a file edited by multiple parallel upstream nodes.
type FileConflict struct {
	File    string   `json:"file"`
	NodeIDs []string `json:"nodeIds"`
}

// MergeGateResult is the deterministic verdict for joining parallel Team branches.
type MergeGateResult struct {
	OK                  bool                 `json:"ok"`
	Group               string               `json:"group"`
	BlockedNodeIDs      []string             `json:"blockedNodeIds,omitempty"`
	OwnershipViolations []OwnershipViolation `json:"ownershipViolations,omitempty"`
	Conflicts           []FileConflict       `json:"conflicts,omitempty"`
	TouchedFiles        []string             `json:"touchedFiles,omitempty"`
	MergedFindings      []string             `json:"mergedFindings,omitempty"`
	MergedNextSteps     []string             `json:"mergedNextSteps,omitempty"`
	Question            string               `json:"question,omitempty"`
	NextAction          string               `json:"nextAction,omitempty"`
}

// AssertEditableParallelOwnership validates that no editable node uses an empty wildcard prefix.
func AssertEditableParallelOwnership(detail workflow.WorkItemDetail) error {
	for _, n := range detail.Nodes {
		if !n.CanEditFiles {
			continue
		}
		for _, raw := range n.OwnedPathPrefixes {
			norm := normalizeRelPath(raw)
			if norm == "" || norm == "." || norm == "*" {
				return fmt.Errorf("editable node %q cannot declare wildcard ownedPathPrefix %q", n.ID, raw)
			}
		}
	}
	return nil
}

// EvaluateMergeGate deterministically checks upstream handoffs, file ownership boundaries,
// and parallel file conflicts before allowing parallel Team branches to merge.
func EvaluateMergeGate(group string, upstream []TeamNodeRun) MergeGateResult {
	res := MergeGateResult{Group: group}

	fileOwners := make(map[string][]string)
	touchedSet := make(map[string]struct{})
	findingSet := make(map[string]struct{})
	nextStepSet := make(map[string]struct{})

	for _, u := range upstream {
		st := strings.ToLower(strings.TrimSpace(u.Status))
		handoffSt := ""
		if u.Handoff != nil {
			handoffSt = strings.ToLower(strings.TrimSpace(u.Handoff.Status))
		}
		if st == "blocked" || st == "failed" || handoffSt == "blocked" || handoffSt == "needs-input" || handoffSt == "fail" {
			res.BlockedNodeIDs = append(res.BlockedNodeIDs, u.NodeID)
		}

		// Collect touched files from actual worktree diff + reported handoff.
		seenInNode := make(map[string]struct{})
		for _, raw := range u.FilesTouched {
			norm := normalizeRelPath(raw)
			if norm == "" {
				continue
			}
			if _, dup := seenInNode[norm]; dup {
				continue
			}
			seenInNode[norm] = struct{}{}
			touchedSet[norm] = struct{}{}
			fileOwners[norm] = append(fileOwners[norm], u.NodeID)

			if !u.CanEditFiles {
				res.OwnershipViolations = append(res.OwnershipViolations, OwnershipViolation{
					NodeID:       u.NodeID,
					Role:         u.Role,
					File:         norm,
					CanEditFiles: false,
				})
				continue
			}
			if len(u.OwnedPathPrefixes) > 0 && !matchesAnyPrefix(norm, u.OwnedPathPrefixes) {
				res.OwnershipViolations = append(res.OwnershipViolations, OwnershipViolation{
					NodeID:            u.NodeID,
					Role:              u.Role,
					File:              norm,
					CanEditFiles:      true,
					OwnedPathPrefixes: u.OwnedPathPrefixes,
				})
			}
		}

		if u.Handoff != nil {
			for _, f := range u.Handoff.Findings {
				f = strings.TrimSpace(f)
				if f == "" {
					continue
				}
				if _, ok := findingSet[f]; !ok {
					findingSet[f] = struct{}{}
					res.MergedFindings = append(res.MergedFindings, f)
				}
			}
			for _, ns := range u.Handoff.NextSteps {
				ns = strings.TrimSpace(ns)
				if ns == "" {
					continue
				}
				if _, ok := nextStepSet[ns]; !ok {
					nextStepSet[ns] = struct{}{}
					res.MergedNextSteps = append(res.MergedNextSteps, ns)
				}
			}
		}
	}

	for file, owners := range fileOwners {
		if len(owners) > 1 {
			sort.Strings(owners)
			res.Conflicts = append(res.Conflicts, FileConflict{File: file, NodeIDs: owners})
		}
	}
	sort.Slice(res.Conflicts, func(i, j int) bool { return res.Conflicts[i].File < res.Conflicts[j].File })

	for file := range touchedSet {
		res.TouchedFiles = append(res.TouchedFiles, file)
	}
	sort.Strings(res.TouchedFiles)

	res.OK = len(res.BlockedNodeIDs) == 0 && len(res.OwnershipViolations) == 0 && len(res.Conflicts) == 0
	if !res.OK {
		var parts []string
		if len(res.BlockedNodeIDs) > 0 {
			parts = append(parts, fmt.Sprintf("%d blocked upstream node(s): %s", len(res.BlockedNodeIDs), strings.Join(res.BlockedNodeIDs, ", ")))
		}
		if len(res.OwnershipViolations) > 0 {
			parts = append(parts, fmt.Sprintf("%d file ownership violation(s)", len(res.OwnershipViolations)))
		}
		if len(res.Conflicts) > 0 {
			parts = append(parts, fmt.Sprintf("%d parallel file conflict(s)", len(res.Conflicts)))
		}
		res.Question = "Resolve " + strings.Join(parts, "; ") + " before merging parallel results."
		if len(res.BlockedNodeIDs) > 0 {
			res.NextAction = "Fix blocked upstream node(s) and re-run `jtao team retry-blocked`."
		} else if len(res.OwnershipViolations) > 0 {
			res.NextAction = "Restrict edits to declared ownedPathPrefixes or reassign file ownership."
		} else {
			res.NextAction = "Assign a single owner per conflicted file before retrying the merge gate."
		}
	}
	return res
}

func normalizeRelPath(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	s = filepath.ToSlash(s)
	s = strings.TrimPrefix(s, "./")
	s = strings.TrimLeft(s, "/")
	return s
}

func matchesAnyPrefix(file string, prefixes []string) bool {
	for _, raw := range prefixes {
		p := normalizeRelPath(raw)
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(file, p) {
				return true
			}
		} else if file == p || strings.HasPrefix(file, p+"/") {
			return true
		}
	}
	return false
}
