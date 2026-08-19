// Package brokerframe defines the wire contract exchanged between a Nexus
// session broker, its WebSocket gateway, and the nexus.io.broker plugin that
// each spawned instance dials back with.
//
// The frame is a small, explicit JSON envelope carried over WebSocket. It is
// shared verbatim by the standalone broker binary (cmd/nexus-broker) and the
// future nexus.io.broker plugin (plugins/io/broker) so both ends stay in
// lockstep. Keep this package dependency-free (stdlib only) so it imports
// cleanly from both sides without cycles.
package brokerframe

import (
	"encoding/json"
	"fmt"
)

// Version is the schema version of the broker frame. Bump it on any
// breaking change to the wire shape so both ends can detect a mismatch.
//
// It is not decorative: the broker VALIDATES it on the register frame and
// refuses an instance that declares anything else, because a build mismatch
// otherwise surfaces as a claim timing out with "instance did not become ready
// in time" — a message that reads as a network fault. Bumping this is therefore
// a deliberate, load-bearing act: every instance binary older than the bump
// stops registering with the brokers that carry it.
const Version = 1

// Environment variable names the broker injects when spawning an instance
// and the nexus.io.broker plugin reads to discover its dial-back target.
// They are defined here so the spawner (cmd/nexus-broker) and the plugin
// (plugins/io/broker) share a single source of truth.
const (
	// EnvBrokerAddr holds the WebSocket address of the broker's instance
	// dial-back endpoint (e.g. "ws://127.0.0.1:8080/instance").
	EnvBrokerAddr = "NEXUS_BROKER_ADDR"

	// EnvLeaseID holds the lease ID the broker assigned to the spawned
	// instance. The plugin echoes it in its register frame.
	EnvLeaseID = "NEXUS_BROKER_LEASE_ID"

	// EnvSpawnSecret holds a per-spawn secret the broker generates and hands
	// to the child process. The plugin echoes it in its register frame
	// (Frame.Secret) and the broker requires it to match the value it minted
	// for that lease — on every registration, whether or not the broker
	// authenticates its clients.
	//
	// It exists because the lease id ALONE is a poor authenticator for the
	// dial-back socket: it travels in ws_urls, client requests and logs, so
	// anything that observes one could otherwise impersonate an instance. This
	// secret never leaves the broker↔child channel — it is passed through the
	// environment (not argv, which is world-readable on most systems) and is
	// never logged or reported on any HTTP surface.
	EnvSpawnSecret = "NEXUS_BROKER_SPAWN_SECRET"
)

// Signal identifies the lifecycle phase or payload kind a Frame carries.
type Signal string

const (
	// SignalRegister is sent by a freshly spawned instance to claim its
	// lease with the broker once its dial-back WebSocket is established.
	SignalRegister Signal = "register"

	// SignalReady is sent by an instance once it has booted and is able to
	// accept IO frames.
	SignalReady Signal = "ready"

	// SignalSessionIDReport carries the engine session ID an instance
	// allocated, so the broker can persist it for later -recall resume.
	SignalSessionIDReport Signal = "session-id-report"

	// SignalShutdown signals an orderly teardown of the lease, in either
	// direction (broker → instance to stop, or instance → broker on exit).
	SignalShutdown Signal = "shutdown"

	// SignalIO carries an opaque IO payload (Frame.Payload) between the
	// client and the instance. The broker forwards these without parsing
	// their contents.
	SignalIO Signal = "io"
)

// valid reports whether s is a recognized signal.
func (s Signal) valid() bool {
	switch s {
	case SignalRegister, SignalReady, SignalSessionIDReport, SignalShutdown, SignalIO:
		return true
	default:
		return false
	}
}

// Frame is the JSON envelope exchanged over the broker WebSocket.
type Frame struct {
	// Version is the frame schema version. Encode stamps the current
	// Version; Decode tolerates older/newer values so callers can decide
	// how to react to a mismatch.
	//
	// The broker is the caller that decides: it refuses a register frame whose
	// version is not its own and logs the skew by name. Tolerating the value
	// here rather than rejecting it in Decode is what makes that possible — a
	// decode error could not tell a skewed build from a corrupt frame.
	Version int `json:"version"`

	// LeaseID identifies the broker lease this frame belongs to. It is
	// assigned by the broker at spawn time and echoed by the instance.
	LeaseID string `json:"lease_id"`

	// Signal is the lifecycle phase or payload kind.
	Signal Signal `json:"signal"`

	// Secret is the per-spawn secret the broker handed the instance through
	// EnvSpawnSecret, echoed on SignalRegister frames so the broker can prove
	// the dialing process is one IT spawned rather than anything that learned
	// the lease id.
	//
	// It is OPTIONAL on the wire (`omitempty`) and that is deliberate, not
	// laxity: an instance binary predating this field encodes no `secret` key
	// at all, and its frame must still DECODE cleanly so the broker can answer
	// with a specific diagnosis instead of a JSON error. Whether an absent
	// secret is ACCEPTED is the broker's policy decision, not this package's —
	// and the broker's answer is now no, unconditionally, with or without an
	// `auth:` block.
	//
	// It is meaningless on every other signal and must never be echoed back to
	// a client: the broker forwards SignalIO frames verbatim, so nothing may
	// ever populate this on one.
	Secret string `json:"secret,omitempty"`

	// SessionID is the engine session ID. Set on SignalSessionIDReport
	// frames; empty otherwise.
	SessionID string `json:"session_id,omitempty"`

	// Seq is the broker-assigned, per-lease sequence number of a CLIENT-BOUND
	// frame. It counts from 1 and increments by exactly one for every frame the
	// broker sends a lease's client, whatever the signal, so a client can detect
	// a gap rather than silently believing a truncated stream.
	//
	// THE BROKER ASSIGNS IT, ON CLIENT-BOUND FRAMES ONLY. An instance never sets
	// it and never reads it: instance-bound frames carry no sequence at all, so
	// the dial-back side needs no protocol awareness and nothing about it
	// changed. Anything an instance puts here is overwritten by the broker
	// before the frame reaches a client.
	//
	// It is OPTIONAL on the wire (`omitempty`) and zero means "unsequenced" —
	// which is what every instance-bound frame is, and what every frame from a
	// broker predating this field was. That is why adding it needed no Version
	// bump: an older peer decodes a sequenced frame cleanly and ignores the key.
	//
	// The sequence is per LEASE, not per broker or per connection: two leases
	// number independently from 1, and the counter lives and dies with the lease
	// (it does not survive a broker restart).
	Seq uint64 `json:"seq,omitempty"`

	// Payload is an opaque IO payload, meaningful on SignalIO frames. The
	// broker forwards it untouched between client and instance.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Encode marshals a Frame to JSON for transmission over the WebSocket. It
// stamps the current schema Version and validates the signal.
func Encode(f Frame) ([]byte, error) {
	if !f.Signal.valid() {
		return nil, fmt.Errorf("brokerframe: invalid signal %q", f.Signal)
	}
	f.Version = Version
	data, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("brokerframe: encode: %w", err)
	}
	return data, nil
}

// Decode unmarshals a Frame from JSON received over the WebSocket and
// validates the signal.
func Decode(data []byte) (Frame, error) {
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return Frame{}, fmt.Errorf("brokerframe: decode: %w", err)
	}
	if !f.Signal.valid() {
		return Frame{}, fmt.Errorf("brokerframe: invalid signal %q", f.Signal)
	}
	return f, nil
}
