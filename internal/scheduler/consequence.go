package scheduler

import "strings"

// Outcome enumerates the finite Solo iteration outcomes.
type Outcome string

const (
	OutcomeSuccess    Outcome = "success"
	OutcomeNoop       Outcome = "noop"
	OutcomeBlocked    Outcome = "blocked"
	OutcomeInfraRetry Outcome = "infra-retry"
	OutcomeHumanGate  Outcome = "human-gate"
	OutcomeStopped    Outcome = "stopped"
	OutcomeFailed     Outcome = "failed"
)

// TurnConsequence is the single owner for mapping an Outcome to all control-plane effects.
type TurnConsequence struct {
	AllowMaterial      bool
	DefaultFailure     string
	BackoffAction      func(failureClass string) string
	RunStatus          func(shouldStop bool) string
	OverridesFailGate  bool
	ResetsBackoff      bool
	CadenceClass       string // active_work | monitor_wait | human_gate
	StopByDefault      bool
	RequiresOperator   bool
	CountsAsNoopPoll   bool
	TerminatesCleanly  bool
}

// SelfReport is the typed S-channel declaration emitted by the agent.
type SelfReport struct {
	Declared      bool   `json:"declared"`
	ProgressMade  bool   `json:"progressMade"`
	BlockedReason string `json:"blockedReason,omitempty"`
	Summary       string `json:"summary,omitempty"`
}

var operatorGateFailureClasses = map[string]bool{
	"ownership-gate":   true,
	"safety-gate":      true,
	"budget-exhausted": true,
}

var retryableFailureClasses = map[string]bool{
	"runtime-error":       true,
	"tool-error":          true,
	"rate-limited":        true,
	"provider-overloaded": true,
}

// TurnConsequences is the single authoritative finite mapping for all Solo outcomes.
var TurnConsequences = map[Outcome]TurnConsequence{
	OutcomeSuccess: {
		AllowMaterial:     true,
		DefaultFailure:    "none",
		BackoffAction:     func(string) string { return "none" },
		RunStatus:         func(stop bool) string { return condStatus(stop, "done", "running") },
		ResetsBackoff:     true,
		CadenceClass:      "active_work",
		StopByDefault:     false,
		RequiresOperator:  false,
		CountsAsNoopPoll:  false,
		TerminatesCleanly: true,
	},
	OutcomeNoop: {
		AllowMaterial:    false,
		DefaultFailure:   "none",
		BackoffAction:    func(string) string { return "none" },
		RunStatus:        func(stop bool) string { return condStatus(stop, "done", "running") },
		ResetsBackoff:    true,
		CadenceClass:     "monitor_wait",
		StopByDefault:    false,
		RequiresOperator: false,
		CountsAsNoopPoll: true,
	},
	OutcomeBlocked: {
		AllowMaterial:    false,
		DefaultFailure:   "no-progress",
		BackoffAction:    func(string) string { return "shrink-scope" },
		RunStatus:        func(stop bool) string { return condStatus(stop, "blocked", "running") },
		ResetsBackoff:    false,
		CadenceClass:     "monitor_wait",
		StopByDefault:    false,
		RequiresOperator: false,
		CountsAsNoopPoll: false,
	},
	OutcomeInfraRetry: {
		AllowMaterial:  false,
		DefaultFailure: "runtime-error",
		BackoffAction: func(fc string) string {
			if retryableFailureClasses[fc] {
				return "retry-with-backoff"
			}
			return "none"
		},
		RunStatus:         func(bool) string { return "backoff" },
		OverridesFailGate: true,
		ResetsBackoff:     false,
		CadenceClass:      "monitor_wait",
		StopByDefault:     false,
		RequiresOperator:  false,
		CountsAsNoopPoll:  false,
	},
	OutcomeHumanGate: {
		AllowMaterial:    false,
		DefaultFailure:   "safety-gate",
		BackoffAction:    func(string) string { return "human-gate" },
		RunStatus:        func(bool) string { return "human-gate" },
		ResetsBackoff:    false,
		CadenceClass:     "human_gate",
		StopByDefault:    true,
		RequiresOperator: true,
		CountsAsNoopPoll: false,
	},
	OutcomeStopped: {
		AllowMaterial:    false,
		DefaultFailure:   "stop-requested",
		BackoffAction:    func(string) string { return "none" },
		RunStatus:        func(bool) string { return "stopped" },
		ResetsBackoff:    false,
		CadenceClass:     "monitor_wait",
		StopByDefault:    true,
		RequiresOperator: false,
		CountsAsNoopPoll: false,
	},
	OutcomeFailed: {
		AllowMaterial:    false,
		DefaultFailure:   "runtime-error",
		BackoffAction:    func(string) string { return "none" },
		RunStatus:        func(bool) string { return "failed" },
		ResetsBackoff:    false,
		CadenceClass:     "monitor_wait",
		StopByDefault:    true,
		RequiresOperator: false,
		CountsAsNoopPoll: false,
	},
}

// IsMaterialTurn enforces S-channel confirmation: bare outcome="success" without
// selfReport.progressMade=true does not count as material progress.
func IsMaterialTurn(outcome Outcome, report SelfReport) bool {
	row, ok := TurnConsequences[outcome]
	if !ok {
		return false
	}
	return row.AllowMaterial && report.ProgressMade
}

// DeriveFailureClass returns the declared failure class or the table default.
func DeriveFailureClass(outcome Outcome, declared string) string {
	trimmed := strings.TrimSpace(declared)
	if trimmed != "" {
		return trimmed
	}
	if row, ok := TurnConsequences[outcome]; ok {
		return row.DefaultFailure
	}
	return "runtime-error"
}

// DeriveRunStatus computes the persisted run status from outcome, failure class, and stop flag.
func DeriveRunStatus(outcome Outcome, failureClass string, shouldStop bool) string {
	row, ok := TurnConsequences[outcome]
	if !ok {
		return condStatus(shouldStop, "done", "running")
	}
	if row.OverridesFailGate {
		return row.RunStatus(shouldStop)
	}
	if operatorGateFailureClasses[strings.TrimSpace(failureClass)] || row.RequiresOperator {
		return "human-gate"
	}
	return row.RunStatus(shouldStop)
}

func condStatus(cond bool, whenTrue, whenFalse string) string {
	if cond {
		return whenTrue
	}
	return whenFalse
}
