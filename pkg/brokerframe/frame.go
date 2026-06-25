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
const Version = 1

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
	Version int `json:"version"`

	// LeaseID identifies the broker lease this frame belongs to. It is
	// assigned by the broker at spawn time and echoed by the instance.
	LeaseID string `json:"lease_id"`

	// Signal is the lifecycle phase or payload kind.
	Signal Signal `json:"signal"`

	// SessionID is the engine session ID. Set on SignalSessionIDReport
	// frames; empty otherwise.
	SessionID string `json:"session_id,omitempty"`

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
