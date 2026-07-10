// Package agui is a hand-rolled codec for the AG-UI ("Agent-User Interaction")
// protocol: an open, event-based protocol that standardizes how AI agents
// stream structured and unstructured output, tool calls, and state mutations to
// user-facing applications.
//
// This package is codec-only. It contains the canonical AG-UI event model, the
// RunAgentInput request shape, JSON encode/decode, and a Server-Sent Events
// (SSE) reader/writer over text/event-stream. It has no dependency on the Nexus
// engine or event bus so that both the serve plugin and the consume client can
// reuse it.
//
// # Targeted spec version
//
// This implementation targets the AG-UI protocol as documented at
// https://docs.ag-ui.com (concepts/events, concepts/interrupts) as fetched
// 2026-07-10. The taxonomy tracked here corresponds to AG-UI spec version
// v1 (the JSON/SSE wire, RFC 6902 JSON Patch StateDelta, and the terminal-run
// interrupt/resume model). Event structs are deliberately kept isolated so that
// spec drift can be absorbed with localized edits.
//
// No third-party AG-UI SDK is used; this is pure standard-library Go.
package agui

// SpecVersion is the AG-UI protocol spec version this codec targets.
const SpecVersion = "v1 (docs.ag-ui.com, 2026-07-10)"
