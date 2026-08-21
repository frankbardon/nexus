package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// readUntilClose drains conn until it closes and returns the close status the
// peer used. A frame still in flight when the socket is displaced is read and
// discarded, so the assertion is about the CLOSE rather than about how much of
// the stream happened to arrive first.
func readUntilClose(t *testing.T, conn *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			status := websocket.CloseStatus(err)
			if status == -1 {
				t.Fatalf("socket ended without a close status: %v", err)
			}
			return status
		}
	}
}

// waitClosed blocks until wc has been shut down, so a test can read the status
// it was closed with. Eviction closes the displaced socket off the attach path,
// so the close is concurrent with the attach returning by design.
func waitClosed(t *testing.T, wc *wsConn) {
	t.Helper()
	select {
	case <-wc.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the displaced connection was never closed")
	}
}

// TestRegistry_AttachClientDisplacesTheExistingConnection is the mechanism at
// the registry level: a lease still carries exactly ONE client, and the newest
// attach is the one that keeps it.
//
// It also pins what displacement must not break — the replay cursor still
// applies, and frames sent after the swap go to the new socket and only to it.
func TestRegistry_AttachClientDisplacesTheExistingConnection(t *testing.T) {
	reg := newStreamTestRegistry(t, defaultClientReplayBufferBytes)
	leaseID := newStreamTestLease(t, reg)

	first := newWSConn(nil)
	att, err := reg.AttachClient(leaseID, first)
	if err != nil {
		t.Fatalf("attach first client: %v", err)
	}
	if att.evicted {
		t.Fatal("the first attach reported an eviction; there was nothing to evict")
	}

	// Three frames reach the first socket, then it goes silent — the half-open
	// case, which from the broker's side is indistinguishable from a healthy one.
	sendClientFrames(t, reg, leaseID, 3, 16)

	second := newWSConn(nil)
	att, err = reg.AttachClientFrom(leaseID, second, 1)
	if err != nil {
		t.Fatalf("attach second client: %v", err)
	}
	if !att.evicted {
		t.Fatal("the second attach did not report displacing the first")
	}
	if got := reg.ClientConn(leaseID); got != second {
		t.Fatal("the lease's client is not the connection that attached last")
	}

	// The eviction composes with ?from_seq= rather than bypassing it: the new
	// socket is owed frames 2 and 3.
	replay := second.takeReplay()
	if len(replay) != 2 {
		t.Fatalf("staged %d replay frames on the displacing connection, want 2", len(replay))
	}

	// The displaced socket is closed with the distinguishable code, so its client
	// can tell this from a crash (4500) and from a release (going away).
	waitClosed(t, first)
	if first.closeStatus != evictedCloseStatus {
		t.Errorf("displaced connection closed with %d, want %d",
			first.closeStatus, evictedCloseStatus)
	}
	if first.closeReason != evictedCloseReason {
		t.Errorf("displaced connection close reason %q, want %q",
			first.closeReason, evictedCloseReason)
	}

	// Live frames follow the client, not the corpse. The displaced socket's queue
	// still holds what it was sent BEFORE the swap (nothing drains a test conn),
	// so the assertion is that it does not grow.
	staleQueued := len(first.send)
	if _, outcome, err := reg.SendClientFrame(leaseID, ioFrame(leaseID, 16)); err != nil {
		t.Fatalf("send after eviction: %v", err)
	} else if outcome != clientFrameDelivered {
		t.Fatalf("frame outcome after eviction = %v, want delivered", outcome)
	}
	if len(second.send) != 1 {
		t.Fatalf("displacing connection queued %d frames, want 1", len(second.send))
	}
	if len(first.send) != staleQueued {
		t.Fatalf("displaced connection queued %d frames after being evicted, want %d",
			len(first.send), staleQueued)
	}
}

// TestRegistry_DisplacedConnectionDoesNotDetachItsSuccessor guards the ordering
// hazard the swap creates: the evicted socket's read pump ends moments AFTER the
// new one is attached and runs the ordinary detach path. If that detach were not
// identity-checked it would unattach the live client, and the lease would go
// silent with a socket still open on it.
func TestRegistry_DisplacedConnectionDoesNotDetachItsSuccessor(t *testing.T) {
	reg := newStreamTestRegistry(t, defaultClientReplayBufferBytes)
	leaseID := newStreamTestLease(t, reg)

	first := newWSConn(nil)
	if _, err := reg.AttachClient(leaseID, first); err != nil {
		t.Fatalf("attach first client: %v", err)
	}
	second := newWSConn(nil)
	if _, err := reg.AttachClient(leaseID, second); err != nil {
		t.Fatalf("attach second client: %v", err)
	}

	reg.DetachClient(leaseID, first)

	if got := reg.ClientConn(leaseID); got != second {
		t.Fatal("a displaced connection's detach unattached the client that superseded it")
	}
	if _, outcome, err := reg.SendClientFrame(leaseID, ioFrame(leaseID, 16)); err != nil {
		t.Fatalf("send: %v", err)
	} else if outcome != clientFrameDelivered {
		t.Fatalf("frame outcome = %v, want delivered — the live client was unattached", outcome)
	}
}

// TestClientEviction_ReconnectDisplacesAndResumes is the story end to end, over
// real sockets: a client that reconnects while its previous socket still looks
// attached wins the lease, is replayed what it missed, and keeps streaming —
// while the socket it displaced is told, in a code, exactly what happened to it.
func TestClientEviction_ReconnectDisplacesAndResumes(t *testing.T) {
	wsURL, reg := newTestGateway(t)
	leaseID, err := reg.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}

	stale := dialClientWithQuery(t, wsURL, leaseID, "")
	waitFor(t, func() bool { return reg.ClientConn(leaseID) != nil })
	first := reg.ClientConn(leaseID)

	// Two frames go out to the socket that is about to die. The client receives
	// the first and is cut off before the second, which is what makes the
	// reconnect's ?from_seq= worth honouring.
	sendClientFrames(t, reg, leaseID, 2, 16)
	if got := readFrame(t, stale).Seq; got != 1 {
		t.Fatalf("first frame seq %d, want 1", got)
	}

	fresh := dialClientWithQuery(t, wsURL, leaseID, fromSeqQueryParam+"=1")
	waitFor(t, func() bool {
		got := reg.ClientConn(leaseID)
		return got != nil && got != first
	})

	// The displaced socket learns WHY it went away, and the code is neither the
	// crash code nor a plain going-away: the lease is alive and now belongs to
	// somebody else's socket.
	if got := readUntilClose(t, stale); got != evictedCloseStatus {
		t.Fatalf("displaced socket closed with %d, want %d (crash is %d, release is %d)",
			got, evictedCloseStatus, crashCloseStatus, websocket.StatusGoingAway)
	}

	// The reconnect picked up the replay cursor: it is handed the frame it missed
	// before anything new, and the live stream continues on the same numbering.
	sendClientFrames(t, reg, leaseID, 1, 16)
	seqs, frames := readSeqs(t, fresh, 2)
	if seqs[0] != 2 || seqs[1] != 3 {
		t.Fatalf("displacing connection read %v, want [2 3] — the replay cursor was bypassed", seqs)
	}
	for i, f := range frames {
		if f.Signal != brokerframe.SignalIO {
			t.Fatalf("frame %d signal %q, want %q — no gap was due", i, f.Signal, brokerframe.SignalIO)
		}
	}
}

// TestClientEviction_OnlyTheOwnerCanDisplace is the security-relevant half.
//
// Eviction is a capability: whoever can reach it can end somebody else's live
// session. It is safe ONLY because ownership is checked before the upgrade, so
// this test proves a valid, perfectly authenticated stranger is refused with the
// same indistinguishable 404 an unknown lease gets AND that the refusal leaves
// the owner's socket exactly where it was.
func TestClientEviction_OnlyTheOwnerCanDisplace(t *testing.T) {
	env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), nil)
	leaseID := newOwnedLease(t, env.reg, ownerPrincipal)

	owner, _, err := dialLease(t, env.wsURL, leaseID, ownerToken)
	if err != nil {
		t.Fatalf("owner refused: %v", err)
	}
	t.Cleanup(func() { _ = owner.Close(websocket.StatusNormalClosure, "") })
	waitFor(t, func() bool { return env.reg.ClientConn(leaseID) != nil })
	attached := env.reg.ClientConn(leaseID)

	// A stranger holding a credential this broker accepts, aimed at a lease it
	// does not own.
	conn, resp, dialErr := dialLease(t, env.wsURL, leaseID, otherToken)
	assertRefusedBeforeUpgrade(t, http.StatusNotFound, conn, resp, dialErr)

	// ...and identically to a lease that never existed, so the refusal is not a
	// lease-id oracle either.
	unknown := doAuthed(t, http.MethodGet, env.httpURL+ClientWSPath("no-such-lease"), otherToken, "")
	unowned := doAuthed(t, http.MethodGet, env.httpURL+ClientWSPath(leaseID), otherToken, "")
	assertIdenticalRefusals(t, http.StatusNotFound, unknown, unowned)

	// Nothing was displaced: the same connection is still the lease's client...
	if got := env.reg.ClientConn(leaseID); got != attached {
		t.Fatal("a refused stranger displaced the owner's connection")
	}
	// ...and it is still streaming, which is the assertion that would fail if the
	// eviction had run and only the bookkeeping had been restored.
	sendClientFrames(t, env.reg, leaseID, 1, 16)
	if got := readFrame(t, owner).Seq; got != 1 {
		t.Fatalf("owner read seq %d after the refused attach, want 1", got)
	}
}

// TestClientEviction_IsAudited pins the operator-facing half: ending somebody's
// live stream is not something the broker may do quietly. The record names the
// lease, the principal on whose authority the socket was closed, and the reason
// the displaced client was given — the three facts an operator needs to tell a
// legitimate reconnect from a client stuck in a reconnect war.
func TestClientEviction_IsAudited(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), logger)
	leaseID := newOwnedLease(t, env.reg, ownerPrincipal)

	stale, _, err := dialLease(t, env.wsURL, leaseID, ownerToken)
	if err != nil {
		t.Fatalf("owner refused: %v", err)
	}
	t.Cleanup(func() { _ = stale.Close(websocket.StatusNormalClosure, "") })
	waitFor(t, func() bool { return env.reg.ClientConn(leaseID) != nil })
	first := env.reg.ClientConn(leaseID)

	fresh, _, err := dialLease(t, env.wsURL, leaseID, ownerToken)
	if err != nil {
		t.Fatalf("the owner's reconnect was refused: %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close(websocket.StatusNormalClosure, "") })
	waitFor(t, func() bool {
		got := env.reg.ClientConn(leaseID)
		return got != nil && got != first
	})
	waitFor(t, func() bool { return strings.Contains(buf.String(), "displaced") })

	record := ""
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "displaced the lease's previous client") {
			record = line
		}
	}
	if record == "" {
		t.Fatalf("no eviction was audited; the log holds:\n%s", buf.String())
	}
	for _, want := range []string{
		`"level":"WARN"`,
		`"lease_id":"` + leaseID + `"`,
		`"principal_id":"` + ownerPrincipal + `"`,
		`"reason":"` + evictedCloseReason + `"`,
	} {
		if !strings.Contains(record, want) {
			t.Errorf("eviction record is missing %s:\n%s", want, record)
		}
	}
}

// TestClientEviction_TheOwnerCanDisplaceItself is the same route from the other
// side, on the SAME authenticated fixture: what refuses a stranger must not also
// refuse the legitimate holder, or the fix would be indistinguishable from the
// bug it replaces.
func TestClientEviction_TheOwnerCanDisplaceItself(t *testing.T) {
	env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), nil)
	leaseID := newOwnedLease(t, env.reg, ownerPrincipal)

	stale, _, err := dialLease(t, env.wsURL, leaseID, ownerToken)
	if err != nil {
		t.Fatalf("owner refused: %v", err)
	}
	waitFor(t, func() bool { return env.reg.ClientConn(leaseID) != nil })
	first := env.reg.ClientConn(leaseID)

	fresh, _, err := dialLease(t, env.wsURL, leaseID, ownerToken)
	if err != nil {
		t.Fatalf("the owner's reconnect was refused: %v", err)
	}
	t.Cleanup(func() { _ = fresh.Close(websocket.StatusNormalClosure, "") })
	waitFor(t, func() bool {
		got := env.reg.ClientConn(leaseID)
		return got != nil && got != first
	})

	if got := readUntilClose(t, stale); got != evictedCloseStatus {
		t.Fatalf("displaced socket closed with %d, want %d", got, evictedCloseStatus)
	}
	sendClientFrames(t, env.reg, leaseID, 1, 16)
	if got := readFrame(t, fresh).Seq; got != 1 {
		t.Fatalf("the reconnected owner read seq %d, want 1", got)
	}
}
