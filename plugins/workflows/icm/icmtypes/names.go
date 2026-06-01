package icmtypes

import "strings"

// InstanceSuffix returns the portion of a plugin instance ID after the
// last "/" separator. Multi-instance ICM plugins use the form
// "nexus.workflows.icm/<suffix>" (e.g. "nexus.workflows.icm/script");
// the default instance has no suffix.
//
// Lives in icmtypes so registration (runtime) and dispatch (predicates)
// can both call the same helper without forming an import cycle.
func InstanceSuffix(instanceID string) string {
	if i := strings.LastIndexByte(instanceID, '/'); i >= 0 && i+1 < len(instanceID) {
		return instanceID[i+1:]
	}
	return ""
}

// StagePostureName returns the registry name a stage (or verifier)
// posture is registered under. Default instance: "icm.<stageID>".
// Suffixed instance: "icm.<suffix>.<stageID>".
//
// Both registration in the plugin and lookup in the orchestrator must
// pass through this helper to stay in sync.
func StagePostureName(instanceID, stageID string) string {
	if suffix := InstanceSuffix(instanceID); suffix != "" {
		return "icm." + suffix + "." + stageID
	}
	return "icm." + stageID
}

// StageOutputSchemaName returns the engine SchemaRegistry key used for
// a stage's `output` schema (when output.format=json and output.schema
// is set). The synthesized "output" predicate that backs that schema
// shares this name with the predicate variant — see PredicateSchemaName.
func StageOutputSchemaName(instanceID, stageID string) string {
	return StagePostureName(instanceID, stageID) + ".output"
}

// PredicateSchemaName returns the canonical schema-registry name for a
// per-predicate JSON schema (output validator or loop `until` condition
// using type=schema). Format: "<stage_posture>.<predicate_name>".
//
// Predicates author the workspace-relative file path in YAML; the icm
// plugin reads + registers each schema under the name produced here at
// Ready(). The predicate dispatcher MUST use this same helper to look
// up the registered schema — the raw file path is registration-time
// metadata, not a runtime lookup key.
func PredicateSchemaName(instanceID, stageID, predicateName string) string {
	return StagePostureName(instanceID, stageID) + "." + predicateName
}
