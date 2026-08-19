// Package a2aconform is the shared A2A conformance vector suite: a declarative,
// transport-neutral corpus that every mapping producing A2A output in this
// repository must satisfy.
//
// # Why this package exists
//
// Two independent mappings turn Nexus activity into A2A frames, and they share
// only the wire types in pkg/a2a — no mapping logic at all:
//
//   - plugins/io/a2a maps the ENGINE BUS (agent.turn.start, tool.result,
//     hitl.requested, ...) onto A2A.
//   - the session broker maps the BROKER IO ENVELOPE (the flat ioMessage union
//     forwarded over the dial-back WebSocket) onto A2A.
//
// Nothing in the type system makes the two agree. Without a shared corpus they
// drift silently: one completes a task where the other interrupts it, one puts
// artifacts before the terminal status and the other after, one closes a stream
// that the other parks. A client that works against one Nexus deployment then
// fails against the other, and the failure surfaces as a partner bug report
// rather than as a test.
//
// So the vectors here describe expected A2A OUTPUT ONLY. A vector never
// mentions a bus event and never mentions an ioMessage — it names an abstract
// step ("the agent produced final text", "the agent asked the human a
// question") and states the exact A2A frame sequence that step must produce.
// Each mapping supplies a Driver that realizes the abstract steps in its own
// vocabulary; the runner does the comparing. A vector that can only be
// satisfied by knowing which mapping produced the frames is a broken vector.
//
// # THE RULE
//
// ANY NEW A2A BEHAVIOR MUST ADD A VECTOR HERE BEFORE IT IS IMPLEMENTED IN A
// SECOND MAPPING.
//
// Building a second mapping first and back-filling vectors afterwards is
// exactly the failure this package exists to prevent: the back-filled vector
// is written from the second mapping's observed output, so it encodes the drift
// instead of catching it. Add the vector, watch it fail, then implement.
//
// Corollary: if a mapping cannot satisfy a vector, do not weaken the vector.
// Either the mapping has a bug, or the vector encodes a wrong expectation and
// the fix belongs in the vector WITH a rationale saying why the old expectation
// was wrong. Silently relaxing an expectation to make a run green removes the
// only thing keeping the two mappings honest.
//
// # Why the corpus lives here and not with either mapping
//
// It cannot live in plugins/io/a2a: that is one of the two mappings under test,
// and a shared oracle owned by one participant is not shared. It cannot live in
// cmd/nexus-broker either: that is a main package, so nothing can import it,
// and it is the other participant besides. pkg/a2a is the one thing both
// mappings already depend on, which makes a sub-package of it the only home
// that both can reach without either depending on the other. The layering
// mirrors pkg/agui/aguiclient.
//
// This package must never import a mapping. It depends on pkg/a2a, the standard
// library and nothing else.
//
// # Corpus format
//
// The vectors are JSON documents under vectors/, embedded at build time. They
// are data rather than Go code on purpose: adding a vector is a data-only
// change, the corpus is trivially readable by a reviewer who does not read Go,
// and a JSON document structurally cannot reference either mapping's types.
// Decoding is strict (unknown fields are an error), so a typo in a vector is a
// build-time failure rather than an expectation that silently never runs.
//
// # What a vector asserts
//
// Vectors pin the CANONICAL A2A frame stream — the frames an ordinary client
// sees. Nexus extension telemetry (see a2a.NexusExtensionURI) is deliberately
// out of scope: it is delivered only to clients that opted in with
// A2A-Extensions, so it is not part of the output a conforming A2A client is
// entitled to, and pinning it here would make the corpus mapping-specific.
// A driver must attach as a NON-extension observer.
//
// Beyond an exact frame-by-frame comparison, the runner independently replays
// the observed frames through a2a.SSEWriter, so every vector also asserts the
// stream contract of specification section 11.7: the stream opens with a Task,
// task and context identity stay stable, every status transition is legal, and
// nothing is written after a terminal frame.
//
// # Deviations this corpus deliberately pins
//
// The effort made four decisions that read as spec deviations at a glance. They
// are encoded here precisely so that a second implementer cannot "fix" them
// into divergence:
//
//  1. STATUS AND ARTIFACT FRAMES INTERLEAVE after the opening frame. A strict
//     reading of section 11.7 ("all status updates, then all artifact updates")
//     is not implementable, because the COMPLETED frame that closes a stream
//     necessarily follows the artifacts whose production it completes. Vectors
//     assert Expect.StatusFollowsArtifact to hold this open.
//  2. STREAMS PARK, THEY DO NOT CLOSE, on a non-terminal INPUT_REQUIRED.
//     Closing on a non-terminal state is indistinguishable client-side from a
//     dropped connection. Vector hitl-parks-stream-open pins the stream staying
//     open, and pins a2a.SSEWriter.Interrupted reporting the parked condition.
//  3. ARTIFACTS PRECEDE THE TERMINAL STATUS FRAME. Section 11.7 closes a stream
//     the moment a terminal state is reported, so an artifact emitted after
//     COMPLETED would be refused and the client would see a finished task with
//     no output. Vectors assert Expect.ArtifactsPrecedeTerminal.
//  4. THE TASK COMPLETES AT THE END OF THE AGENT TURN, not at the raw terminal
//     model response. Nexus publishes output and runs its output gates between
//     the two, so completing at the model response would publish the model's
//     draft rather than what Nexus decided to say. Vectors encode this with an
//     assert_active step placed between the agent_text step and the output/
//     turn_end steps.
package a2aconform
