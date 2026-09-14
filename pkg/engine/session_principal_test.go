package engine

import (
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestSetPrincipalID_BindsAndReads(t *testing.T) {
	ws, _ := newTagTestEngine(t)

	if _, ok := ws.PrincipalID(); ok {
		t.Fatal("a fresh session reports a principal")
	}
	if err := ws.SetPrincipalID("alice"); err != nil {
		t.Fatalf("SetPrincipalID: %v", err)
	}
	id, ok := ws.PrincipalID()
	if !ok || id != "alice" {
		t.Errorf("PrincipalID() = (%q, %v), want alice", id, ok)
	}
}

// A turn under a caller with no verified identity must clear the previous
// turn's rather than inherit it — the failure worse than having none.
func TestSetPrincipalID_EmptyClears(t *testing.T) {
	ws, _ := newTagTestEngine(t)

	if err := ws.SetPrincipalID("alice"); err != nil {
		t.Fatalf("SetPrincipalID: %v", err)
	}
	if err := ws.SetPrincipalID(""); err != nil {
		t.Fatalf("clearing SetPrincipalID: %v", err)
	}
	if id, ok := ws.PrincipalID(); ok {
		t.Errorf("PrincipalID() = %q after a clear, want absent", id)
	}
}

// The label is reserved, so it is unreachable from the general bus path a
// plugin or an agent's session_tags tools would use.
func TestPrincipalIDLabel_NotWritableOverTheBus(t *testing.T) {
	ws, bus := newTagTestEngine(t)

	veto, err := bus.EmitVetoable("before:session.tag.set", &events.SessionTagSetRequest{
		SchemaVersion: events.SessionTagSetRequestVersion,
		Key:           ReservedPrincipalIDKey,
		Value:         "forged",
	})
	if err != nil {
		t.Fatalf("EmitVetoable: %v", err)
	}
	if !veto.Vetoed {
		t.Fatal("writing _principal_id over the bus was not vetoed")
	}
	if id, ok := ws.PrincipalID(); ok {
		t.Errorf("PrincipalID() = %q, want nothing written", id)
	}
}

// Identity and request headers share the reserved namespace but must not
// disturb one another: clearing either leaves the other standing.
func TestPrincipalIDAndRequestHeadersAreIndependent(t *testing.T) {
	ws, _ := newTagTestEngine(t)

	if err := ws.SetPrincipalID("alice"); err != nil {
		t.Fatalf("SetPrincipalID: %v", err)
	}
	if err := ws.SetRequestHeaders(map[string]string{"tenant": "acme"}); err != nil {
		t.Fatalf("SetRequestHeaders: %v", err)
	}

	if err := ws.SetRequestHeaders(nil); err != nil {
		t.Fatalf("clearing headers: %v", err)
	}
	if id, ok := ws.PrincipalID(); !ok || id != "alice" {
		t.Errorf("clearing headers disturbed the principal: (%q, %v)", id, ok)
	}

	if err := ws.SetRequestHeaders(map[string]string{"tenant": "acme"}); err != nil {
		t.Fatalf("SetRequestHeaders: %v", err)
	}
	if err := ws.SetPrincipalID(""); err != nil {
		t.Fatalf("clearing principal: %v", err)
	}
	headers, err := ws.RequestHeaders()
	if err != nil {
		t.Fatalf("RequestHeaders: %v", err)
	}
	if headers["tenant"] != "acme" {
		t.Errorf("clearing the principal disturbed the headers: %v", headers)
	}
}

// A request header can never be mistaken for the verified identity, whatever a
// caller names it.
func TestRequestHeaderCannotOccupyThePrincipalKey(t *testing.T) {
	ws, _ := newTagTestEngine(t)

	if err := ws.SetRequestHeaders(map[string]string{"principal_id": "forged"}); err != nil {
		t.Fatalf("SetRequestHeaders: %v", err)
	}
	if id, ok := ws.PrincipalID(); ok {
		t.Errorf("a header named principal_id became the verified identity %q", id)
	}
	headers, err := ws.RequestHeaders()
	if err != nil {
		t.Fatalf("RequestHeaders: %v", err)
	}
	if headers["principal_id"] != "forged" {
		t.Errorf("the header itself was lost: %v", headers)
	}
}
