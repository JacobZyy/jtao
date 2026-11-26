package workflow

import (
	"regexp"
	"strings"
)

var (
	slashCommandPattern = regexp.MustCompile(`^/(plan|team|subagent|solo|harness|single|direct|readonly|read-only|debug|review|spec|design|verify|ops)\b[:\s-]*`)
	resumePrefixPattern = regexp.MustCompile(`(?i)^(?:continue|resume|继续|接着做|恢复)(?:[\s,;:!?。、，！？-]+|$)`)
	ackPrefixPattern    = regexp.MustCompile(`(?i)^(?:ok(?:ay)?|yes|y|sure|approved?|confirmed?|go\s+ahead|sounds\s+good|可以的?|好的?|收到|同意|认可|确认|没问题)(?:[\s,;:!?。、，！？-]+|$)`)
)

var intentAliases = map[string]string{
	"subagent":       "team",
	"solo":           "harness",
	"readonly":       "read-only",
	"specification":  "spec",
	"implementation": "implement",
	"root-cause":     "debug",
}

var routeForIntent = map[string]string{
	"implement": "implement",
	"plan":      "implement",
	"spec":      "design",
	"design":    "design",
	"debug":     "debug",
	"review":    "verify",
	"verify":    "verify",
	"ops":       "ops",
	"team":      "team",
	"harness":   "harness",
}

var terminalStatuses = map[WorkItemStatus]bool{
	WorkDone:      true,
	WorkBlocked:   true,
	WorkCancelled: true,
}

// ClassifyPolicy deterministically classifies a turn using ONLY explicit commands,
// protocol prefixes, and active WorkItem state — never guessing semantics from prose.
func (s *SQLiteService) ClassifyPolicy(in PolicyClassifyInput) PolicyDecision {
	rawMsg := strings.TrimSpace(in.Message)
	intent, cleaned := extractExplicitIntent(rawMsg, in.ExplicitIntent)

	mode := in.PolicyMode
	if mode != PolicyStrict {
		mode = PolicyAdaptive
	}

	usableActive := in.ActiveItem != nil && !terminalStatuses[in.ActiveItem.Status]
	activeID := ""
	if usableActive {
		activeID = in.ActiveItem.ID
	}

	if rawMsg == "" && intent == "" {
		return PolicyDecision{
			Disposition:   DispositionNoop,
			Continuation:  "none",
			Persistence:   PersistNone,
			RouteHint:     "none",
			ExecutionHost: HostSingle,
			Reason:        "empty-message",
		}
	}

	// Explicit read-only/direct request never creates a plan.
	if intent == "direct" || intent == "read-only" || intent == "analysis" || intent == "status" || intent == "explain" {
		return PolicyDecision{
			Disposition:   DispositionDirect,
			Continuation:  "none",
			Persistence:   PersistNone,
			RouteHint:     "none",
			ExecutionHost: HostSingle,
			Reason:        "explicit-direct-intent",
			CleanedPrompt: cleaned,
		}
	}

	// Explicit resume prefix without a replacement objective resumes active work.
	if intent == "" && resumePrefixPattern.MatchString(rawMsg) {
		tail := strings.TrimSpace(resumePrefixPattern.ReplaceAllString(rawMsg, ""))
		if tail == "" {
			if usableActive {
				return PolicyDecision{
					Disposition:   DispositionPlanned,
					Continuation:  "explicit-resume",
					Persistence:   PersistReuse,
					RouteHint:     in.ActiveItem.Route,
					ExecutionHost: hostForRoute(in.ActiveItem.Route),
					Reason:        "explicit-resume-active-work-item",
					CleanedPrompt: rawMsg,
					ActiveItemID:  activeID,
				}
			}
			return PolicyDecision{
				Disposition:   DispositionBlocked,
				Continuation:  "missing",
				Persistence:   PersistNone,
				RouteHint:     "none",
				ExecutionHost: HostSingle,
				Reason:        "explicit-resume-without-active-work-item",
				CleanedPrompt: rawMsg,
			}
		}
	}

	// Pure acknowledgement attaches only to same client/session active WorkItem.
	if intent == "" && ackPrefixPattern.MatchString(rawMsg) {
		tail := strings.TrimSpace(ackPrefixPattern.ReplaceAllString(rawMsg, ""))
		if tail == "" {
			if usableActive && isSameSession(in.ActiveItem, in.Client, in.SessionID) {
				return PolicyDecision{
					Disposition:   DispositionPlanned,
					Continuation:  "same-session-ack",
					Persistence:   PersistReuse,
					RouteHint:     in.ActiveItem.Route,
					ExecutionHost: hostForRoute(in.ActiveItem.Route),
					Reason:        "same-session-acknowledgement",
					CleanedPrompt: rawMsg,
					ActiveItemID:  activeID,
				}
			}
			return PolicyDecision{
				Disposition:   DispositionNoop,
				Continuation:  "none",
				Persistence:   PersistNone,
				RouteHint:     "none",
				ExecutionHost: HostSingle,
				Reason:        "standalone-acknowledgement",
				CleanedPrompt: rawMsg,
			}
		}
	}

	route := routeForIntent[intent]
	if route == "" {
		route = "implement"
	}
	host := hostForRoute(route)

	wantsPlan := isPlanIntent(intent) || mode == PolicyStrict || strings.TrimSpace(in.WorkItemID) != "" || in.NewWorkItem
	if !wantsPlan {
		return PolicyDecision{
			Disposition:   DispositionGuarded,
			Continuation:  "none",
			Persistence:   PersistNone,
			RouteHint:     route,
			ExecutionHost: host,
			Reason:        "default-guarded-turn",
			CleanedPrompt: cleaned,
		}
	}

	// Reuse active WorkItem when not starting a new one and declared ID matches (or is blank).
	if usableActive && !in.NewWorkItem {
		declaredID := CanonicalizeWorkItemID(in.WorkItemID)
		if declaredID == "" || declaredID == in.ActiveItem.ID {
			return PolicyDecision{
				Disposition:   DispositionPlanned,
				Continuation:  "none",
				Persistence:   PersistReuse,
				RouteHint:     route,
				ExecutionHost: host,
				Reason:        "reuse-active-work-item",
				CleanedPrompt: cleaned,
				ActiveItemID:  activeID,
			}
		}
	}

	return PolicyDecision{
		Disposition:   DispositionPlanned,
		Continuation:  "none",
		Persistence:   PersistCreate,
		RouteHint:     route,
		ExecutionHost: host,
		Reason:        "create-planned-work-item",
		CleanedPrompt: cleaned,
	}
}

func extractExplicitIntent(message, explicit string) (string, string) {
	if trimmed := strings.ToLower(strings.TrimSpace(explicit)); trimmed != "" {
		if alias, ok := intentAliases[trimmed]; ok {
			trimmed = alias
		}
		return trimmed, message
	}
	loc := slashCommandPattern.FindStringSubmatchIndex(strings.ToLower(message))
	if loc == nil {
		return "", message
	}
	cmd := strings.ToLower(message[loc[2]:loc[3]])
	if cmd == "single" {
		return "", strings.TrimSpace(message[loc[1]:])
	}
	if alias, ok := intentAliases[cmd]; ok {
		cmd = alias
	}
	rest := strings.TrimSpace(message[loc[1]:])
	if rest == "" {
		rest = message
	}
	return cmd, rest
}

func isPlanIntent(intent string) bool {
	switch intent {
	case "plan", "team", "harness", "spec", "design":
		return true
	default:
		return false
	}
}

func hostForRoute(route string) ExecutionHost {
	switch route {
	case "team":
		return HostTeam
	case "harness":
		return HostHarness
	default:
		return HostSingle
	}
}

func isSameSession(item *WorkItem, client, sessionID string) bool {
	if item == nil {
		return false
	}
	planClient := strings.ToLower(strings.TrimSpace(item.OriginClient))
	reqClient := strings.ToLower(strings.TrimSpace(client))
	if planClient == "" || reqClient == "" || planClient == "unknown" || reqClient == "unknown" || planClient != reqClient {
		return false
	}
	planSess := strings.TrimSpace(item.OriginSessionID)
	reqSess := strings.TrimSpace(sessionID)
	if planSess == "" && reqSess == "" {
		return true
	}
	return planSess != "" && reqSess != "" && planSess == reqSess
}
