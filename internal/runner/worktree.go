package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorktreeHandle tracks an isolated git worktree created for a Solo run or Team node.
type WorktreeHandle struct {
	Enabled       bool   `json:"enabled"`
	BaseRef       string `json:"baseRef"`
	Path          string `json:"path"`
	WorkspacePath string `json:"workspacePath"`
	InitialHead   string `json:"initialHead"`
	Preserved     bool   `json:"preserved"`
	CleanupReason string `json:"cleanupReason,omitempty"`
}

// WorktreeChanges summarizes git working tree and commit deltas inside a worktree.
type WorktreeChanges struct {
	Changed       bool     `json:"changed"`
	HasDirtyFiles bool     `json:"hasDirtyFiles"`
	HeadChanged   bool     `json:"headChanged"`
	Files         []string `json:"files,omitempty"`
	CurrentHead   string   `json:"currentHead"`
}

// WorktreeManager creates, inspects, and cleans up isolated git worktrees.
type WorktreeManager struct{}

// Prepare creates a detached git worktree under os.TempDir() anchored at baseRef.
func (WorktreeManager) Prepare(workspaceRoot, runID, baseRef string) (WorktreeHandle, error) {
	if strings.TrimSpace(baseRef) == "" {
		baseRef = "HEAD"
	}
	safeRun := sanitizeDirToken(runID)
	parentDir, err := os.MkdirTemp("", fmt.Sprintf("jtao-wt-%s-", safeRun))
	if err != nil {
		return WorktreeHandle{}, fmt.Errorf("create worktree temp dir: %w", err)
	}
	repoDir := filepath.Join(parentDir, "repo")

	if _, err := runGit(workspaceRoot, "worktree", "add", "--detach", repoDir, baseRef); err != nil {
		_ = os.RemoveAll(parentDir)
		return WorktreeHandle{}, fmt.Errorf("git worktree add: %w", err)
	}
	head, err := runGit(repoDir, "rev-parse", "HEAD")
	if err != nil {
		_ = os.RemoveAll(parentDir)
		return WorktreeHandle{}, fmt.Errorf("read worktree HEAD: %w", err)
	}

	return WorktreeHandle{
		Enabled:       true,
		BaseRef:       baseRef,
		Path:          repoDir,
		WorkspacePath: parentDir,
		InitialHead:   head,
	}, nil
}

// DetectChanges reports dirty files or new commits inside a worktree.
func (WorktreeManager) DetectChanges(h WorktreeHandle) (WorktreeChanges, error) {
	if !h.Enabled || h.Path == "" {
		return WorktreeChanges{}, nil
	}
	statusOut, err := runGit(h.Path, "status", "--porcelain")
	if err != nil {
		return WorktreeChanges{}, err
	}
	currentHead, err := runGit(h.Path, "rev-parse", "HEAD")
	if err != nil {
		return WorktreeChanges{}, err
	}

	var files []string
	for _, line := range strings.Split(statusOut, "\n") {
		if len(line) < 4 {
			continue
		}
		files = append(files, strings.TrimSpace(line[3:]))
	}
	hasDirty := len(files) > 0
	headChanged := h.InitialHead != "" && currentHead != h.InitialHead

	return WorktreeChanges{
		Changed:       hasDirty || headChanged,
		HasDirtyFiles: hasDirty,
		HeadChanged:   headChanged,
		Files:         files,
		CurrentHead:   currentHead,
	}, nil
}

// Finalize preserves the worktree when edits exist or the run ended in an inspectable
// non-clean status, and removes it cleanly otherwise.
func (WorktreeManager) Finalize(workspaceRoot string, h WorktreeHandle, finalStatus string, preserveOnChange, forceCleanup bool) (WorktreeHandle, error) {
	if !h.Enabled || h.Path == "" {
		return h, nil
	}

	changes, err := (WorktreeManager{}).DetectChanges(h)
	if err != nil && !forceCleanup {
		h.Preserved = true
		h.CleanupReason = "detect-changes-error"
		return h, err
	}

	shouldPreserve := !forceCleanup && ((preserveOnChange && changes.Changed) || preserveForStatus(finalStatus))
	if shouldPreserve {
		h.Preserved = true
		if changes.Changed {
			h.CleanupReason = "changes-detected"
		} else {
			h.CleanupReason = "status-" + strings.ToLower(strings.TrimSpace(finalStatus))
		}
		return h, nil
	}

	_, rmErr := runGit(workspaceRoot, "worktree", "remove", "--force", h.Path)
	if h.WorkspacePath != "" {
		_ = os.RemoveAll(h.WorkspacePath)
	}
	h.Path = ""
	h.WorkspacePath = ""
	h.Preserved = false
	if forceCleanup {
		h.CleanupReason = "force-cleanup"
	} else {
		h.CleanupReason = "no-output"
	}
	return h, rmErr
}

func preserveForStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "blocked", "human-gate", "stopped", "failed", "backoff":
		return true
	default:
		return false
	}
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		return trimmed, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, trimmed)
	}
	return trimmed, nil
}

func sanitizeDirToken(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "run"
	}
	if b.Len() > 32 {
		return b.String()[:32]
	}
	return b.String()
}
