package icm

import "github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"

// Schema-version constants for icm.* event payloads. ICM emits these on
// top of the generic plan.created/plan.progress surface so basic UIs see
// stage-level transitions while richer UIs render iteration/turn/item
// detail.
//
// Canonical definitions live in icmtypes so the runtime sub-package can
// emit them without forming an import cycle. The constants and types are
// re-exported here for ergonomic use by subscribers that already import
// the main icm package.

const (
	ICMRunStartedVersion     = icmtypes.ICMRunStartedVersion
	ICMRunCompletedVersion   = icmtypes.ICMRunCompletedVersion
	ICMRunHaltedVersion      = icmtypes.ICMRunHaltedVersion
	ICMStageStartedVersion   = icmtypes.ICMStageStartedVersion
	ICMStageCompletedVersion = icmtypes.ICMStageCompletedVersion
	ICMStageFailedVersion    = icmtypes.ICMStageFailedVersion
	ICMStageIterationVersion = icmtypes.ICMStageIterationVersion
	ICMTurnVersion           = icmtypes.ICMTurnVersion
	ICMFanoutItemVersion     = icmtypes.ICMFanoutItemVersion
)

// ICMPredicateFailedVersion re-exports the canonical version constant
// from icmtypes for callers used to the icm.ICMPredicateFailedVersion
// name.
const ICMPredicateFailedVersion = icmtypes.ICMPredicateFailedVersion

// ConditionResult is re-exported from icmtypes so existing
// icm.ConditionResult references in this package and its sub-packages
// keep working. The canonical definition lives in icmtypes to avoid a
// cycle with the session sub-package.
type ConditionResult = icmtypes.ConditionResult

// ICMPredicateFailed is re-exported from icmtypes. The predicates
// sub-package emits values of this type; subscribers can keep using the
// short icm.ICMPredicateFailed name.
type ICMPredicateFailed = icmtypes.ICMPredicateFailed

// Lifecycle event payload re-exports. The runtime sub-package emits these
// directly; the icm package surfaces the type aliases for ergonomic
// consumer code.
type (
	ICMRunStarted     = icmtypes.ICMRunStarted
	ICMRunCompleted   = icmtypes.ICMRunCompleted
	ICMRunHalted      = icmtypes.ICMRunHalted
	ICMStageStarted   = icmtypes.ICMStageStarted
	ICMStageCompleted = icmtypes.ICMStageCompleted
	ICMStageFailed    = icmtypes.ICMStageFailed
	ICMStageIteration = icmtypes.ICMStageIteration
	ICMTurn           = icmtypes.ICMTurn
	ICMFanoutItem     = icmtypes.ICMFanoutItem
)
