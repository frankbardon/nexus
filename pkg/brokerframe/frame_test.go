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
