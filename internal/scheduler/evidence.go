package scheduler

import (
	"fmt"
	"os"
	"strings"

	"github.com/JacobZyy/jtao/internal/workflow"
)

const evidenceRefsPromptRule = "For `evidenceRefs`, every item MUST be strict ASCII `scheme:value` format (for example `file:path/to/file`, `receipt:<id>`, `artifact:<id>`). Never put natural language, command execution summaries, Chinese fullwidth colons (`：`), or placeholders into `evidenceRefs`. If this node has a `verifySpec`, the runner automatically executes it and injects the real `receipt:<id>` — do NOT write command output text into `evidenceRefs`; leave `evidenceRefs` as `[]` if you have no extra `scheme:value` references."

type sanitizedEvidenceRefs struct {
	Refs            []string
	DroppedFreeText []string
}

// sanitizeEvidenceRefsForSubmit merges runner-generated receipt IDs with sub-agent
// evidenceRefs before workflow submission:
//   - Valid ASCII scheme:value refs and runner receipts are preserved (deduplicated).
//   - Placeholder/forged refs are NEVER filtered out so workflow.SubmitNodeResult rejects them.
//   - Non-scheme free-text entries from the sub-agent are filtered out ONLY when at least one
//     valid evidence ref (such as a runner receipt or valid file:/artifact: ref) is present;
//     otherwise they are preserved unmodified so workflow.SubmitNodeResult rejects the submission.
func sanitizeEvidenceRefsForSubmit(agentRefs, receiptIDs []string) sanitizedEvidenceRefs {
	seen := make(map[string]struct{}, len(agentRefs)+len(receiptIDs))
	var valid []string
	var placeholders []string
	var freeText []string

	addUnique := func(dst *[]string, raw string) {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return
		}
		if _, dup := seen[trimmed]; dup {
			return
		}
		seen[trimmed] = struct{}{}
		*dst = append(*dst, trimmed)
	}

	for _, ref := range agentRefs {
		trimmed := strings.TrimSpace(ref)
		if trimmed == "" {
			continue
		}
		if err := workflow.ValidateEvidenceRef(trimmed); err == nil {
			addUnique(&valid, trimmed)
			continue
		}
		if workflow.IsPlaceholderEvidenceRef(trimmed) {
			addUnique(&placeholders, trimmed)
			continue
		}
		addUnique(&freeText, trimmed)
	}

	for _, id := range receiptIDs {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		if err := workflow.ValidateEvidenceRef(trimmed); err == nil {
			addUnique(&valid, trimmed)
		} else {
			addUnique(&placeholders, trimmed)
		}
	}

	if len(placeholders) > 0 || len(valid) == 0 {
		combined := make([]string, 0, len(valid)+len(placeholders)+len(freeText))
		combined = append(combined, valid...)
		combined = append(combined, placeholders...)
		combined = append(combined, freeText...)
		return sanitizedEvidenceRefs{Refs: combined}
	}

	return sanitizedEvidenceRefs{
		Refs:            valid,
		DroppedFreeText: freeText,
	}
}

func recordDroppedEvidenceRefs(nodeID string, dropped []string) string {
	if len(dropped) == 0 {
		return ""
	}
	note := fmt.Sprintf("filtered non-scheme evidenceRefs: %s", strings.Join(dropped, "; "))
	fmt.Fprintf(os.Stderr, "[jtao] node %s: %s\n", nodeID, note)
	return note
}
