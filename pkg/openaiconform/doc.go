// Package openaiconform is the shared OpenAI Responses conformance corpus: one
// declarative definition of the canonical request vectors, the settled wire
// invariants and the reply fixtures that EVERY serializer and parser for
// OpenAI's `/v1/responses` surface in this repository must satisfy.
//
// # Why this package exists
//
// Two independent implementations build a Responses request body and decode a
// Responses reply, and they share no code at all:
//
//   - plugins/providers/openai — the synchronous provider (responses.go,
//     responses_reply.go).
//   - plugins/llm/batch — the batch coordinator's OpenAI adapter
//     (openai_responses.go, openai_surface.go).
//
// The duplication is deliberate and is not going away: the provider's builders
// are unexported methods on plugin state (auth mode, multimodal config, prompt
// registry) that the coordinator neither has nor should have, calling into the
// provider package would be the plugin-to-plugin call CLAUDE.md forbids, and
// lifting the serializer into pkg/ would be a large extraction of code that has
// only just stabilized. See openai_surface.go for that bargain stated in full.
//
// What the duplication costs is drift. The two agree today — both send
// `store: false`, both write an explicit `strict: false` on every function
// tool, both translate the Responses lifecycle into the same finish-reason
// vocabulary — and nothing in the type system keeps them agreeing tomorrow. A
// wire fix applied to one and not the other does not fail a build; it surfaces
// as an HTTP 400 on batched traffic only, months later, as a customer report.
//
// So this package holds the corpus and the checking, and each implementation's
// own test package supplies a thin driver that produces a body (or parses a
// reply) with its own code. Neither plugin imports the other; both import this.
//
// # THE RULE
//
// ANY CHANGE TO THE RESPONSES WIRE SHAPE MUST LAND HERE FIRST.
//
// Change the vector or the invariant, watch BOTH suites fail, then fix both
// implementations. Fixing one implementation and back-filling the corpus from
// its observed output encodes the drift instead of catching it.
//
// Corollary: if one surface cannot satisfy a vector, do not weaken the vector
// and do not quietly drop a key out of the expected body. Either that surface
// has a bug, or the difference is legitimate — and a legitimate difference goes
// in RequestDivergences or ReplyDivergences, WITH a rationale, where a reviewer
// sees it in the diff.
//
// # Why the corpus lives here and not with either implementation
//
// It cannot live in plugins/providers/openai: that is one of the two
// implementations under test, and a shared oracle owned by one participant is
// not shared — nor could plugins/llm/batch import it without becoming a
// plugin-to-plugin dependency. It cannot live in plugins/llm/batch for the
// mirror-image reason. pkg/events is the one thing both already depend on, so a
// sibling package under pkg/ that depends on nothing but pkg/events and the
// standard library is the only home both can reach. The layering mirrors
// pkg/a2a/a2aconform, which exists for exactly this reason on the A2A side.
//
// This package must never import a plugin. It adds no dependency to the root
// module, and it is imported only from _test.go files.
//
// # How drift is actually caught
//
// An invariant suite alone would not catch the failure mode this package exists
// for. Invariants check values that someone thought to name; the realistic
// drift is a KEY SET divergence — one surface gains a field, or stops writing
// one, and every named check still passes.
//
// So each vector carries the WHOLE expected body, and CheckBody compares key
// sets in both directions: a key the surface produced and the vector does not
// expect is reported as drift, and so is a key the vector expects and the
// surface did not produce. The named invariants (see Invariants) run on top of
// that, so a body that drifted in a way the vector happens to permit still
// fails on the invariant it violated, and the failure names the invariant
// rather than a diff line.
//
// Legitimate per-surface differences are RequestDivergences and
// ReplyDivergences: an explicit list, each entry naming the key, the surfaces
// allowed to produce it, and why. The list is enforced in both directions — a
// key declared provider-only is REQUIRED ABSENT on the batch surface — so
// "batch quietly grew a `stream` field" fails just as loudly as "provider
// stopped sending one".
//
// # Corpus scope
//
// The vectors cover what BOTH surfaces claim to serialize: text messages,
// function tools, structured output, temperature, the resolved reasoning
// configuration, and assistant/tool round trips. They deliberately do not cover
// the provider-only halves — multimodal content parts, prompt-registry
// decoration, tool_choice, tool filtering, predicted-output degradation and the
// replay of a previous turn's encrypted reasoning Items — because the batch
// coordinator documents those as out of its v1 scope. A vector that exercised
// one would be testing the provider alone, which its own package already does.
//
// A provider driver must therefore build with no prompt registry configured:
// the corpus pins serialization, not decoration.
//
// # A corner this corpus deliberately does not pin
//
// The two reply paths disagree about an EMPTY `error` object. The provider
// treats any non-nil `error` as a failed run; the batch decoder additionally
// requires a code or a message. No OpenAI reply carries a populated-but-empty
// error object, so pinning either behaviour would be inventing a wire contract
// rather than recording one. It is named here so that the omission is a
// decision on the record rather than an oversight.
package openaiconform
