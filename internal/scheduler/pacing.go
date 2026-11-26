package scheduler

import (
	"math"
	"time"
)

// PacingConfig configures unattended Solo quota, cadence backoff, quiet shutdown, and safe bypass.
type PacingConfig struct {
	Enabled        bool          `json:"enabled"`
	QuotaMaxTurns  int           `json:"quotaMaxTurns,omitempty"`  // Max material turns per QuotaWindow (0 = unlimited)
	QuotaWindow    time.Duration `json:"quotaWindow,omitempty"`    // Sliding window (default 1h)
	CadenceBase    time.Duration `json:"cadenceBase,omitempty"`    // Initial monitor_wait delay (0 = disabled)
	CadenceMax     time.Duration `json:"cadenceMax,omitempty"`     // Cap for exponential cadence delay
	QuietThreshold int           `json:"quietThreshold,omitempty"` // Consecutive noop turns before quiet stop (0 = disabled)
	SafeBypass     bool          `json:"safeBypass,omitempty"`     // Grant 1 read-only probe before human-gate stop
}

// ShouldRunDecision is the pure decision returned by ResolveShouldRun.
type ShouldRunDecision struct {
	Action       string `json:"action"` // run | wait | ask | quiet
	ReasonCode   string `json:"reasonCode"`
	NextWakeMs   int64  `json:"nextWakeMs,omitempty"`
	CadenceClass string `json:"cadenceClass,omitempty"`
}

// PacingState persists rolling quota spends, cadence backoff, and noop counters in runs.pacing_json.
type PacingState struct {
	QuotaSpendMs    []int64            `json:"quotaSpendMs,omitempty"`
	CadenceClass    string             `json:"cadenceClass,omitempty"`
	CadenceUntilMs  int64              `json:"cadenceUntilMs,omitempty"`
	CadenceStep     int                `json:"cadenceStep,omitempty"`
	ConsecutiveNoop int                `json:"consecutiveNoop,omitempty"`
	SafeBypassUsed  bool               `json:"safeBypassUsed,omitempty"`
	LastDecision    *ShouldRunDecision `json:"lastDecision,omitempty"`
}

// QuotaState summarizes remaining capacity in the rolling quota window.
type QuotaState struct {
	Exhausted bool
	Used      int
	Limit     int
	FreedAtMs int64
}

const minWakeMs int64 = 50

// ComputeQuotaState evaluates rolling material spend inside cfg.QuotaWindow.
func ComputeQuotaState(spendMs []int64, nowMs int64, cfg PacingConfig) *QuotaState {
	if !cfg.Enabled || cfg.QuotaMaxTurns <= 0 {
		return nil
	}
	window := cfg.QuotaWindow
	if window <= 0 {
		window = time.Hour
	}
	cutoff := nowMs - window.Milliseconds()
	var active []int64
	for _, ts := range spendMs {
		if ts > cutoff {
			active = append(active, ts)
		}
	}
	exhausted := len(active) >= cfg.QuotaMaxTurns
	freedAt := nowMs
	if exhausted && len(active) > 0 {
		oldest := active[0]
		for _, ts := range active[1:] {
			if ts < oldest {
				oldest = ts
			}
		}
		freedAt = oldest + window.Milliseconds()
	}
	return &QuotaState{
		Exhausted: exhausted,
		Used:      len(active),
		Limit:     cfg.QuotaMaxTurns,
		FreedAtMs: freedAt,
	}
}

// ChargeQuotaSpend prunes expired timestamps and records a new spend ONLY if material == true.
func ChargeQuotaSpend(spendMs []int64, material bool, nowMs int64, cfg PacingConfig) []int64 {
	window := cfg.QuotaWindow
	if window <= 0 {
		window = time.Hour
	}
	cutoff := nowMs - window.Milliseconds()
	out := make([]int64, 0, len(spendMs)+1)
	for _, ts := range spendMs {
		if ts > cutoff {
			out = append(out, ts)
		}
	}
	if material && cfg.Enabled && cfg.QuotaMaxTurns > 0 {
		out = append(out, nowMs)
	}
	return out
}

// AdvanceCadence updates cadence class and exponential monitor_wait timer from a turn outcome.
func AdvanceCadence(prev PacingState, outcome Outcome, material bool, nowMs int64, cfg PacingConfig) (string, int64, int) {
	class := deriveCadenceClass(outcome, material)
	if !cfg.Enabled || cfg.CadenceBase <= 0 {
		return class, 0, 0
	}
	if class == "human_gate" {
		return class, -1, 0
	}
	if class == "active_work" {
		return class, 0, 0
	}
	// monitor_wait: exponential backoff clamped by CadenceMax.
	step := prev.CadenceStep + 1
	maxDelay := cfg.CadenceMax
	if maxDelay < cfg.CadenceBase {
		maxDelay = cfg.CadenceBase * 8
	}
	mult := math.Pow(2, float64(step-1))
	delayMs := int64(float64(cfg.CadenceBase.Milliseconds()) * mult)
	if delayMs > maxDelay.Milliseconds() {
		delayMs = maxDelay.Milliseconds()
	}
	return class, nowMs + delayMs, step
}

func deriveCadenceClass(outcome Outcome, material bool) string {
	row, ok := TurnConsequences[outcome]
	if !ok {
		return "monitor_wait"
	}
	if row.RequiresOperator {
		return "human_gate"
	}
	if row.CadenceClass == "active_work" {
		if material {
			return "active_work"
		}
		return "monitor_wait"
	}
	return row.CadenceClass
}

// ResolveShouldRun is a pure control-plane gate deciding run | wait | ask | quiet.
// operatorGateAcknowledged=true (fresh Run or explicit Resume) clears prior operator gates
// and noop counters, while NEVER bypassing the rolling quota money-gate.
func ResolveShouldRun(
	lastOutcome Outcome,
	lastStatus string,
	lastFailureClass string,
	quota *QuotaState,
	state PacingState,
	cfg PacingConfig,
	operatorGateAcknowledged bool,
	nowMs int64,
) ShouldRunDecision {
	// 1. Unreleased operator gate -> ask.
	needsOperator := false
	if row, ok := TurnConsequences[lastOutcome]; ok && row.RequiresOperator {
		needsOperator = true
	}
	if !operatorGateAcknowledged && (needsOperator || lastStatus == "human-gate") {
		return ShouldRunDecision{Action: "ask", ReasonCode: "operator_gate", CadenceClass: state.CadenceClass}
	}

	// 2. Provider budget exhausted -> ask operator.
	if !operatorGateAcknowledged && lastFailureClass == "budget-exhausted" {
		return ShouldRunDecision{Action: "ask", ReasonCode: "budget_exhausted", CadenceClass: state.CadenceClass}
	}

	// 3. Rolling quota exhausted -> wait until earliest spend ages out (never bypassed).
	if quota != nil && quota.Exhausted {
		wake := quota.FreedAtMs - nowMs
		if wake < minWakeMs {
			wake = minWakeMs
		}
		return ShouldRunDecision{
			Action:       "wait",
			ReasonCode:   "quota_exhausted",
			NextWakeMs:   wake,
			CadenceClass: state.CadenceClass,
		}
	}

	// 4. Cadence timer or cadence human gate.
	if !operatorGateAcknowledged && state.CadenceUntilMs < 0 {
		return ShouldRunDecision{Action: "ask", ReasonCode: "cadence_human_gate", CadenceClass: state.CadenceClass}
	}
	if !operatorGateAcknowledged && state.CadenceUntilMs > nowMs {
		wake := state.CadenceUntilMs - nowMs
		if wake < minWakeMs {
			wake = minWakeMs
		}
		return ShouldRunDecision{
			Action:       "wait",
			ReasonCode:   "cadence_wait",
			NextWakeMs:   wake,
			CadenceClass: state.CadenceClass,
		}
	}

	// 5. Consecutive unchanged polls -> quiet stop.
	if !operatorGateAcknowledged && cfg.QuietThreshold > 0 && state.ConsecutiveNoop >= cfg.QuietThreshold {
		return ShouldRunDecision{Action: "quiet", ReasonCode: "unchanged_poll", CadenceClass: state.CadenceClass}
	}

	return ShouldRunDecision{Action: "run", ReasonCode: "eligible", CadenceClass: state.CadenceClass}
}
