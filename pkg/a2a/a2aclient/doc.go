// Package a2aclient is a pure-Go client for the Agent2Agent (A2A) protocol. It
// resolves a remote agent's Agent Card, sends messages over either HTTP
// binding, consumes the Server-Sent Events stream a streaming call answers
// with, resumes an interrupted task, cancels a task and reads one back.
//
// It is transport-only. It has no dependency on the Nexus engine, the event bus
// or any plugin, and it reimplements no wire concern: every byte it writes and
// reads goes through pkg/a2a's codec, SSE reader and error model. A serve
// plugin and this client therefore agree on the wire by construction rather
// than by review.
//
// # Bindings
//
// A2A defines two HTTP bindings and this package speaks both. JSONRPC is the
// default because it is the binding every A2A implementation is expected to
// expose; HTTP+JSON is selectable with WithBinding. The choice affects only
// framing — the same operations, parameter objects and errors are reachable
// either way, and a stream produced by either binding is decoded by the same
// reader, which auto-detects the framing per record.
//
// # Discovery
//
// New takes the remote's BASE URL, not an operation endpoint. The client fetches
// the Agent Card from AgentCardPath under that base on first use and reads the
// endpoint for the selected binding out of it, so an operator configures one URL
// rather than one URL per binding. Capabilities reports what the card promises:
// streaming support, the bindings on offer, the declared security schemes and
// the protocol extensions. An operator who was handed a card out of band (which
// specification section 8.2 sanctions as "Direct Configuration") supplies it
// with WithCard, and one who knows the endpoint outright supplies it with
// WithJSONRPCEndpoint or WithRESTEndpoint — either of which skips discovery
// entirely.
//
// # Credentials
//
// Credentials are supplied through the CredentialSource seam, not baked in.
// The zero configuration — no source at all — works against an open endpoint,
// which is what a loopback development agent is. Concrete bearer, OAuth2 and
// mTLS sources are built on this interface elsewhere; nothing in this package
// knows how a credential is obtained, only that it is applied to each request
// and may be refreshed per call.
//
// # Robustness
//
// A remote agent is somebody else's process and may be wrong. Every way this
// package has found for a remote to be wrong produces a typed error rather than
// a hang, a panic or a silently truncated result:
//
//   - A card that will not parse, will not validate, or is answered with an
//     HTTP error is a *CardError.
//   - A binding the card does not expose, a protocol version it declares that
//     this client does not speak, or a streaming call to an agent whose card
//     says it does not stream, is a *BindingError.
//   - An HTTP failure carrying no decodable A2A error body is an *HTTPError;
//     one that does carry an A2A error body is the *a2a.Error it encodes, so
//     errors.As recovers the protocol taxonomy.
//   - Anything that goes wrong with a stream is a *StreamError carrying a
//     Reason: a malformed frame, a frame that violates the stream contract
//     (wrong opening frame, mid-stream identity change, illegal state
//     transition), a stream that ends before reaching a terminal state, a
//     non-SSE response where SSE was promised, an idle stream, or a
//     cancellation.
//
// # Timeouts, retries and cancellation
//
// Timeout and retry policy live here rather than in a caller, because they are
// properties of talking to a remote over HTTP and every caller would otherwise
// reinvent them. See RetryPolicy for what is retried and why the answer differs
// between an idempotent read and a message send.
//
// Cancelling the context passed to any call aborts the in-flight HTTP request
// promptly. For a stream that means the reader goroutine and the idle watchdog
// both exit and the response body is closed; no goroutine outlives the call.
//
// No third-party A2A SDK is used; this is standard-library Go on top of pkg/a2a.
package a2aclient
