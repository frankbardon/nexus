// Package a2a is a hand-rolled codec for the Agent2Agent (A2A) protocol: an
// open standard for agent-to-agent communication built around a Task lifecycle,
// Messages composed of Parts, and Artifacts carrying task output.
//
// This package is codec-only. It contains the canonical A2A data model, the
// operation parameter/response objects, the JSON-RPC 2.0 binding, the
// HTTP+JSON/REST binding route shapes, the A2A service parameters
// (A2A-Version / A2A-Extensions), and the protocol error model with its
// per-binding mappings. It has no dependency on the Nexus engine or event bus
// so that both a serve plugin and an outbound client can reuse it.
//
// # Targeted spec version
//
// This implementation targets A2A specification 1.0.x. The wire facts were taken
// from https://raw.githubusercontent.com/a2aproject/A2A/main/docs/specification.md
// and the canonical Protocol Buffer definition at
// https://raw.githubusercontent.com/a2aproject/A2A/main/specification/a2a.proto,
// both fetched 2026-08-18.
//
// 1.0 is a breaking revision of 0.3.x. Two differences bite hardest and are
// implemented here deliberately:
//
//   - JSON-RPC method names are PascalCase operation names ("SendMessage",
//     "GetTask"), not the 0.3-era dotted forms ("message/send", "tasks/get").
//     See specification section 9.4.
//   - Part is flattened. There are no separate TextPart/FilePart/DataPart
//     types; a Part is a single struct with a content oneof (text, raw, url,
//     data) plus mediaType, filename and metadata that apply to every kind.
//
// # JSON conventions
//
// All JSON field names are camelCase (specification section 5.5), even though
// the proto uses snake_case. Enums serialize as their SCREAMING_SNAKE_CASE proto
// names ("TASK_STATE_WORKING", "ROLE_USER") per ProtoJSON. Timestamps serialize
// as ISO 8601 UTC strings with millisecond precision (section 5.6.1); see
// Timestamp.
//
// # Scope
//
// Streaming transport (Server-Sent Events) and the Agent Card discovery types
// are intentionally not in this file set; they are layered on top of these
// types. The push-notification operations are likewise out of scope: the
// TaskPushNotificationConfig data type exists because SendMessageConfiguration
// references it, but no push operation is implemented, and DecodeCall reports
// the push-config methods as unsupported.
//
// No third-party A2A SDK is used; this is pure standard-library Go.
package a2a

// SpecVersion identifies the A2A specification revision this codec was written
// against, including the fetch date of the source documents.
const SpecVersion = "1.0.x (a2aproject/A2A specification.md + a2a.proto, 2026-08-18)"

// ProtocolVersion is the Major.Minor A2A protocol version this codec speaks. It
// is the value sent and expected in the A2A-Version service parameter. Patch
// versions are never used on the wire (specification section 3.6).
const ProtocolVersion = "1.0"
