package brokerframe

import (
	"encoding/json"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	in := Frame{
		LeaseID:   "lease-123",
		Signal:    SignalIO,
		SessionID: "sess-abc",
		Payload:   json.RawMessage(`{"kind":"io.output","text":"hello"}`),
	}

	data, err := Encode(in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	out, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if out.Version != Version {
		t.Errorf("Version = %d, want %d", out.Version, Version)
	}
	if out.LeaseID != in.LeaseID {
		t.Errorf("LeaseID = %q, want %q", out.LeaseID, in.LeaseID)
	}
	if out.Signal != in.Signal {
		t.Errorf("Signal = %q, want %q", out.Signal, in.Signal)
	}
	if out.SessionID != in.SessionID {
		t.Errorf("SessionID = %q, want %q", out.SessionID, in.SessionID)
	}
	if string(out.Payload) != string(in.Payload) {
		t.Errorf("Payload = %s, want %s", out.Payload, in.Payload)
	}
}

// TestEncodeDecodeRoundTripWithSecret is the round trip a register frame takes
// once the spawn secret is in play: the value must survive Encode → Decode
// unchanged, because the broker compares it byte-for-byte against what it
// minted and any mangling would refuse a legitimate instance.
func TestEncodeDecodeRoundTripWithSecret(t *testing.T) {
	in := Frame{
		LeaseID: "lease-123",
		Signal:  SignalRegister,
		Secret:  "0f1e2d3c4b5a69788796a5b4c3d2e1f0",
	}

	data, err := Encode(in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.Secret != in.Secret {
		t.Errorf("Secret = %q, want %q", out.Secret, in.Secret)
	}
	if out.LeaseID != in.LeaseID || out.Signal != in.Signal {
		t.Errorf("frame = %+v, want lease %q signal %q", out, in.LeaseID, in.Signal)
	}
}

// TestEncodeOmitsAbsentSecret pins the wire shape a peer that sets no secret
// produces: the key is ABSENT, not present-and-empty.
//
// That matters in one direction specifically — an unauthenticated deployment
// must put nothing secret-shaped on the wire — and it is also what makes the
// skew test below a real test rather than a restatement of the encoder.
func TestEncodeOmitsAbsentSecret(t *testing.T) {
	data, err := Encode(Frame{LeaseID: "l", Signal: SignalRegister})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := probe["secret"]; present {
		t.Errorf("encoded frame carries a secret key with none set: %s", data)
	}

	out, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.Secret != "" {
		t.Errorf("Secret = %q, want empty", out.Secret)
	}
}

// TestDecodeToleratesFrameWithoutSecret is the version-skew guarantee, written
// against literal bytes rather than against Encode's output on purpose: this is
// exactly what an instance binary built before the spawn-secret protocol puts on
// the wire, and it must DECODE cleanly.
//
// If it did not, the broker would answer a version-skewed instance with "invalid
// register frame" — indistinguishable from corruption — instead of the specific
// "your binary may predate the spawn-secret protocol" diagnosis that tells an
// operator what to do.
func TestDecodeToleratesFrameWithoutSecret(t *testing.T) {
	const legacy = `{"version":1,"lease_id":"lease-legacy","signal":"register"}`
	out, err := Decode([]byte(legacy))
	if err != nil {
		t.Fatalf("a pre-secret register frame failed to decode: %v", err)
	}
	if out.LeaseID != "lease-legacy" || out.Signal != SignalRegister {
		t.Errorf("frame = %+v, want lease-legacy/register", out)
	}
	if out.Secret != "" {
		t.Errorf("Secret = %q, want empty for a frame that carries no secret", out.Secret)
	}
}

func TestEncodeStampsVersion(t *testing.T) {
	data, err := Encode(Frame{LeaseID: "l", Signal: SignalRegister})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if probe.Version != Version {
		t.Errorf("encoded version = %d, want %d", probe.Version, Version)
	}
}

func TestEncodeRejectsInvalidSignal(t *testing.T) {
	if _, err := Encode(Frame{LeaseID: "l", Signal: Signal("bogus")}); err == nil {
		t.Fatal("expected error for invalid signal, got nil")
	}
}

func TestDecodeRejectsInvalidSignal(t *testing.T) {
	if _, err := Decode([]byte(`{"lease_id":"l","signal":"bogus"}`)); err == nil {
		t.Fatal("expected error for invalid signal, got nil")
	}
}

func TestDecodeRejectsMalformedJSON(t *testing.T) {
	if _, err := Decode([]byte(`{not json`)); err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestAllSignalsValid(t *testing.T) {
	for _, s := range []Signal{SignalRegister, SignalReady, SignalSessionIDReport, SignalShutdown, SignalIO} {
		if !s.valid() {
			t.Errorf("signal %q should be valid", s)
		}
	}
}
