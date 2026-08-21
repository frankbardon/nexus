// Package a2a is a hand-rolled codec for the Agent2Agent (A2A) protocol: an
// open standard for agent-to-agent communication built around a Task lifecycle,
// Messages composed of Parts, and Artifacts carrying task output.
//
// This package is codec-only. It contains the canonical A2A data model, the
// operation parameter/response objects, the JSON-RPC 2.0 binding, the
// HTTP+JSON/REST binding route shapes, the Server-Sent Events streaming
// transport in both framings, the Agent Card discovery types, the A2A service
// parameters (A2A-Version / A2A-Extensions), the Nexus protocol extension, and
// the protocol error model with its per-binding mappings. It has no dependency
// on the Nexus engine or event bus so that both a serve plugin and an outbound
// client can reuse it.
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
// # Streaming
//
// SSEWriter and SSEReader implement the streaming transport for
// SendStreamingMessage and SubscribeToTask. Both enforce the stream contract of
// specification section 11.7 rather than merely serializing frames: a stream
// opens with a Task or a Message, carries only update events afterwards, keeps
// task and context identity stable across frames, follows the task state
// transition graph, and closes the moment a frame reports a terminal state.
// Terminal means COMPLETED, FAILED, CANCELED and REJECTED alike — a failed run
// closes its stream exactly as a successful one does.
//
// Status and artifact updates may interleave after the opening frame. A
// stricter reading of the section 11.7 ordering ("all status updates, then all
// artifact updates") is not implementable, since the COMPLETED status frame
// that closes a stream necessarily follows the artifact frames carrying the
// output it completes.
//
// # Streams parked on INPUT_REQUIRED
//
// TASK_STATE_INPUT_REQUIRED (and TASK_STATE_AUTH_REQUIRED with it) is an
// interruption, not a termination: the task is still live and the client is
// expected to resume it by sending a new message carrying the same taskId and
// contextId. The specification's close rule keys off terminal states, so it
// does not fire here, and this package deliberately does not invent an extra
// close.
//
// The chosen behavior, and the reasoning for it:
//
//   - The INPUT_REQUIRED status frame is written and flushed like any other,
//     carrying the agent's question in Status.Message, and the stream stays
//     open. Closing on a non-terminal state would be indistinguishable,
//     client-side, from a dropped connection: the client cannot tell whether to
//     answer the question, retry, or re-subscribe. Keeping the stream open
//     keeps that signal unambiguous, and lets the answer's effects (the
//     WORKING transition and everything after) arrive on the connection the
//     client is already reading.
//   - Because a parked stream would otherwise leave a client waiting forever,
//     SSEWriter.Interrupted reports the parked condition explicitly. It exists
//     so a serving layer must make a policy decision rather than inherit an
//     indefinite wait by accident: apply an input deadline, and on expiry drive
//     the task to a real terminal state (FAILED or CANCELED) and let the
//     ordinary terminal-close rule end the stream. Ending a wait through a
//     state transition keeps the task store, any concurrent subscriber, and the
//     client's own view in agreement; a silent hangup would not.
//   - SSEWriter.WriteComment and SSEWriter.Ping emit SSE comment records so a
//     parked stream can be kept alive through proxies with idle read timeouts
//     without emitting a protocol frame that says nothing happened.
//
// # Discovery
//
// AgentCard and its subordinate types model the manifest served at
// AgentCardPath. Use EncodeAgentCard to serve it and ValidateAgentCard to check
// it. AgentCapabilities serializes its three booleans unconditionally, so a
// card always states a capability rather than leaving a client to infer it from
// an absent key; its zero value is the honest card for an agent supporting none
// of them.
//
// Specification section 4.4 defers the card's field list entirely to a2a.proto,
// which is the authority here. Three of its choices are easy to get wrong by
// analogy with other manifest formats, so they are called out: the card is FLAT
// (no nested identity object), there is NO card-level protocol version (the A2A
// version is declared per AgentInterface), and the transport is an OPEN-FORM
// STRING — "JSONRPC", "GRPC", "HTTP+JSON" — not a ProtoJSON enum. See
// ProtocolBinding.
//
// # Version negotiation policy
//
// ParseServiceParams implements section 3.6.2 literally: an absent A2A-Version
// means 0.3, which this codec does not speak and therefore rejects. Because
// that reading refuses well-behaved 1.0 clients whose HTTP layer merely omitted
// a header, ParseServiceParamsAssuming lets a serving host choose the fallback
// explicitly. The choice belongs to the host, which knows whether it ever
// served 0.3; it is not a codec-level default to be inherited by accident.
//
// # The Nexus extension
//
// NexusExtensionURI names the one extension this codebase defines. It carries
// the Nexus telemetry that has no canonical A2A representation — thinking
// steps, tool calls and results, subagent progress, token usage — as the typed
// NexusEvent union. See extension.go for the two carriers and the rationale.
//
// # Scope
//
// The push-notification operations are out of scope: the
// TaskPushNotificationConfig data type exists because SendMessageConfiguration
// references it, but no push operation is implemented, and DecodeCall reports
// the push-config methods as unsupported. Agent cards built by NewAgentCard
// therefore declare both capabilities.pushNotifications and
// capabilities.extendedAgentCard as false. The gRPC binding is likewise not
// implemented; only the two HTTP bindings are.
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
