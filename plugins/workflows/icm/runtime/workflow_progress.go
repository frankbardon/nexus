package runtime

import (
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
)

// emitWorkflowProgress publishes the engine-generic events.WorkflowProgress
// payload that powers IO plugins' dedicated workflow status surface
// (the TUI right-rail workflow panel; the browser chip indicator).
//
// This sits alongside the icm.* event family rather than replacing it:
//   - icm.* gives detailed audit-trail-style rows in the scrollback.
//   - workflow.progress gives the one-glance "where are we now" status,
//     shaped identically across every workflow plugin (icm, planexec, ...).
//
// stageID may be empty for run-level transitions (Started before stages,
// Completed after the last stage, Halted on cancel). The helper looks up
// stage index/total + display label off the loaded Workflow so call sites
// pass only what changed.
func (o *Orchestrator) emitWorkflowProgress(p events.WorkflowProgress) {
	if o.Bus == nil {
		return
	}
	p.SchemaVersion = events.WorkflowProgressVersion
	if p.WorkflowID == "" {
		p.WorkflowID = o.InstanceID
		if p.WorkflowID == "" {
			p.WorkflowID = "nexus.workflows.icm"
		}
	}
	if p.RunID == "" {
		p.RunID = o.RunID
	}
	if p.WorkflowName == "" && o.Workflow != nil {
		p.WorkflowName = lastPathElement(o.Workflow.Root)
	}
	if p.StageTotal == 0 && o.Workflow != nil {
		p.StageTotal = len(o.Workflow.Stages)
	}
	if p.Stage != "" && o.Workflow != nil {
		for i := range o.Workflow.Stages {
			s := &o.Workflow.Stages[i]
			if s.ID == p.Stage {
				if p.StageIndex == 0 {
					p.StageIndex = i + 1
				}
				if p.StageLabel == "" {
					p.StageLabel = s.Display
					if p.StageLabel == "" {
						p.StageLabel = s.ID
					}
				}
				break
			}
		}
	}
	_ = o.Bus.Emit("workflow.progress", p)
}

// failureNamesFromConds reduces a ConditionResult slice to the names of
// the failed conditions only. Workflow status panels render the names
// (not the full feedback text) so users see at-a-glance what didn't
// converge without overflowing the panel.
func failureNamesFromConds(conds []icmtypes.ConditionResult) []string {
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
