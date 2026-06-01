package icmtypes

import (
	"fmt"
	"strings"
)

// FormatStageStarted renders an ICMStageStarted event as a one-line
// progress string suitable for the thinking-step stream. Phase ("icm.stage")
// is returned alongside so subscribers can label the row consistently.
func FormatStageStarted(ev ICMStageStarted) (phase, content string) {
	return "icm.stage", fmt.Sprintf("▶ stage %d: %s (posture: %s)", ev.Order, ev.StageID, ev.PostureName)
}

// FormatStageCompleted renders an ICMStageCompleted event. Includes
// iteration count and convergence flag when relevant.
func FormatStageCompleted(ev ICMStageCompleted) (phase, content string) {
	var parts []string
	parts = append(parts, "✓ "+ev.StageID+" done")
	if ev.IterationsRun > 0 {
		parts = append(parts, fmt.Sprintf("%d iter", ev.IterationsRun))
	}
	if ev.ConvergenceFailed {
		parts = append(parts, "convergence failed")
	}
	if ev.ArtifactPath != "" {
		parts = append(parts, "→ "+ev.ArtifactPath)
	}
	return "icm.stage", strings.Join(parts, " · ")
}

// FormatStageFailed renders an ICMStageFailed event.
func FormatStageFailed(ev ICMStageFailed) (phase, content string) {
	return "icm.stage", fmt.Sprintf("✗ %s failed: %s", ev.StageID, ev.Reason)
}

// FormatStageIteration renders an ICMStageIteration event. The first
// iteration of a stage has no exit failures yet; later iterations list
// the failing predicate names so the reader sees what didn't converge.
func FormatStageIteration(ev ICMStageIteration) (phase, content string) {
	scope := ev.StageID
	if ev.ItemID != "" {
		scope = ev.StageID + "[" + ev.ItemID + "]"
	}
	base := fmt.Sprintf("↻ %s iter %d/%d", scope, ev.Iteration, ev.MaxIterations)
	if len(ev.ExitFailures) > 0 {
		names := failureNames(ev.ExitFailures)
		base += " — last fail: " + strings.Join(names, ", ")
	}
	return "icm.iter", base
}

// FormatTurn renders an ICMTurn event. Turns are the inner retry layer
// inside a single stage invocation; the failures here come from
// output.validators that rejected the artifact.
func FormatTurn(ev ICMTurn) (phase, content string) {
	scope := ev.StageID
	if ev.ItemID != "" {
		scope = ev.StageID + "[" + ev.ItemID + "]"
	}
	if ev.Iteration > 0 {
		scope = fmt.Sprintf("%s iter %d", scope, ev.Iteration)
	}
	base := fmt.Sprintf("⟳ %s turn %d/%d", scope, ev.Turn, ev.MaxTurns)
	if len(ev.LastFailures) > 0 {
		names := failureNames(ev.LastFailures)
		base += " — fail: " + strings.Join(names, ", ")
	}
	return "icm.turn", base
}

// FormatFanoutItem renders an ICMFanoutItem event for any item lifecycle
// boundary (active, completed, failed).
func FormatFanoutItem(ev ICMFanoutItem) (phase, content string) {
	marker := "•"
	switch ev.Status {
	case "completed":
		marker = "✓"
	case "failed":
		marker = "✗"
	case "active":
		marker = "▸"
	}
	base := fmt.Sprintf("%s %s[%s] %d/%d %s", marker, ev.StageID, ev.ItemID, ev.Index, ev.Total, ev.Status)
	if ev.Error != "" {
		base += " — " + ev.Error
	}
	return "icm.fanout", base
}

// FormatPredicateFailed renders an ICMPredicateFailed event with the
// container ("output.validators", "loop.until", or "verifier") and the
// predicate name. The feedback string can be long — the caller may
// truncate before sending to a constrained UI.
func FormatPredicateFailed(ev ICMPredicateFailed) (phase, content string) {
	scope := ev.StageID
	if ev.ItemID != "" {
		scope = ev.StageID + "[" + ev.ItemID + "]"
	}
	base := fmt.Sprintf("✗ %s %s/%s (%s)", scope, ev.Container, ev.PredicateName, ev.PredicateType)
	if ev.Feedback != "" {
		base += " — " + ev.Feedback
	}
	return "icm.predicate", base
}

// FormatRunStarted renders an ICMRunStarted event.
func FormatRunStarted(ev ICMRunStarted) (phase, content string) {
	return "icm.run", fmt.Sprintf("▶ workflow %q (%d stages, run=%s)", ev.WorkspaceName, ev.Stages, ev.RunID)
}

// FormatRunCompleted renders an ICMRunCompleted event.
func FormatRunCompleted(ev ICMRunCompleted) (phase, content string) {
	base := fmt.Sprintf("✓ workflow completed: %d stages, %ds", ev.StagesRun, ev.ElapsedSeconds)
	if ev.AggregatePath != "" {
		base += " → " + ev.AggregatePath
	}
	return "icm.run", base
}

// FormatRunHalted renders an ICMRunHalted event.
func FormatRunHalted(ev ICMRunHalted) (phase, content string) {
	scope := "workflow"
	if ev.HaltedAtStage != "" {
		scope = "stage " + ev.HaltedAtStage
	}
	verb := "halted"
	if ev.Cancelled {
		verb = "cancelled"
	}
	return "icm.run", fmt.Sprintf("✗ %s %s: %s (%ds)", scope, verb, ev.Reason, ev.ElapsedSeconds)
}

func failureNames(conds []ConditionResult) []string {
	out := make([]string, 0, len(conds))
	for _, c := range conds {
		if c.Verdict == "pass" {
			continue
		}
		name := c.Name
		if name == "" {
			name = c.Type
		}
		out = append(out, name)
	}
	return out
}
