package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/coder/websocket"

	"github.com/frankbardon/nexus/pkg/brokerframe"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// sendClientFrames pushes n client-bound IO frames of the given payload size
// through the registry, failing the test on the first refusal. It is the
// stand-in for an instance streaming output: the frames take the same path
// (Registry.SendClientFrame) whether they came off a dial-back socket or not.
func sendClientFrames(t *testing.T, reg *Registry, leaseID string, n, payload int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, _, err := reg.SendClientFrame(leaseID, ioFrame(leaseID, payload)); err != nil {
			t.Fatalf("send client frame %d: %v", i, err)
		}
	}
}

// retainedLeaseSeqs reports which sequences a lease's replay buffer still
// holds, oldest first, so a test can state its expectations against the actual
// eviction rather than against an assumed frame size.
func retainedLeaseSeqs(t *testing.T, reg *Registry, leaseID string) []uint64 {
	t.Helper()
	frames, _, err := reg.ClientReplayFrom(leaseID, 0)
	if err != nil {
		t.Fatalf("replay from 0: %v", err)
	}
	out := make([]uint64, 0, len(frames))
	for _, data := range frames {
		f, err := brokerframe.Decode(data)
		if err != nil {
			t.Fatalf("decode retained frame: %v", err)
		}
		out = append(out, f.Seq)
	}
	return out
}

// dialClientWithQuery dials the client WebSocket for a lease with an arbitrary
// query string (no leading "?"), which is how a resume is expressed on the
// wire. It fails the test if the handshake is refused.
func dialClientWithQuery(t *testing.T, wsURL, leaseID, query string) *websocket.Conn {
	t.Helper()
	target := wsURL + ClientWSPath(leaseID)
	if query != "" {
		target += "?" + query
	}
	conn := dial(t, target)
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

// readSeqs reads n frames and returns their sequences in arrival order,
// alongside the decoded frames, so a test can assert ORDER and not merely
// presence.
func readSeqs(t *testing.T, conn *websocket.Conn, n int) ([]uint64, []brokerframe.Frame) {
	t.Helper()
	seqs := make([]uint64, 0, n)
	frames := make([]brokerframe.Frame, 0, n)
	for i := 0; i < n; i++ {
		f := readFrame(t, conn)
		seqs = append(seqs, f.Seq)
		frames = append(frames, f)
	}
	return seqs, frames
}

// decodeGap reads the stream-gap payload off a frame, failing the test if the
// frame is not one.
func decodeGap(t *testing.T, f brokerframe.Frame) clientGap {
	t.Helper()
	if f.Signal != brokerframe.SignalStreamGap {
		t.Fatalf("expected a %s frame, got %s", brokerframe.SignalStreamGap, f.Signal)
	}
	if f.Seq != 0 {
		t.Fatalf("gap frame carries seq %d; a gap notice is per-connection and must be unsequenced", f.Seq)
	}
	var gap clientGap
	if err := json.Unmarshal(f.Payload, &gap); err != nil {
		t.Fatalf("decode gap payload %s: %v", f.Payload, err)
	}
	return gap
}

// TestParseFromSeq pins the parameter's three states apart: absent, present
// (including an explicit zero), and malformed — which must read as absent so a
// client bug costs the resume rather than the connection.
func TestParseFromSeq(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		want    uint64
		present bool
	}{
		{name: "absent", query: "", present: false},
		{name: "empty value", query: "from_seq=", present: false},
		{name: "explicit zero is present", query: "from_seq=0", want: 0, present: true},
		{name: "positive", query: "from_seq=42", want: 42, present: true},
		{name: "not a number", query: "from_seq=abc", present: false},
		{name: "negative", query: "from_seq=-1", present: false},
		{name: "float", query: "from_seq=1.5", present: false},
		{name: "overflows uint64", query: "from_seq=99999999999999999999999", present: false},
		{name: "another parameter only", query: "ticket=abc", present: false},
		{name: "composes with a ticket", query: "ticket=abc&from_seq=7", want: 7, present: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("parse query %q: %v", tc.query, err)
			}
			got, present := parseFromSeq(values)
			if present != tc.present || got != tc.want {
				t.Fatalf("parseFromSeq(%q) = (%d, %v), want (%d, %v)",
					tc.query, got, present, tc.want, tc.present)
			}
		})
	}
}

// TestClientStream_ResumeFromReportsTheOwedTail covers the resume arithmetic in
// isolation: which frames are owed, and — when the buffer cannot reach back far
// enough — exactly which range is gone.
//
// The evicted cases are driven through a real byte bound rather than a
// hand-built buffer, so the expectations are computed from what eviction
// actually left behind.
func TestClientStream_ResumeFromReportsTheOwedTail(t *testing.T) {
	t.Run("fully covered", func(t *testing.T) {
		s := newClientStream(defaultClientReplayBufferBytes)
		for i := 0; i < 5; i++ {
			if _, _, err := s.send(nil, ioFrame("lease", 8)); err != nil {
				t.Fatalf("send: %v", err)
			}
		}
		got := s.resumeFrom(2)
		if got.gap != nil {
			t.Fatalf("gap reported for a resume the buffer covers: %+v", *got.gap)
		}
		if len(got.frames) != 3 {
			t.Fatalf("replayed %d frames, want 3 (seq 3,4,5)", len(got.frames))
		}
		if got.lastSeq != 5 {
			t.Fatalf("lastSeq = %d, want 5", got.lastSeq)
		}
	})

	t.Run("already current replays nothing", func(t *testing.T) {
		s := newClientStream(defaultClientReplayBufferBytes)
		for i := 0; i < 3; i++ {
			if _, _, err := s.send(nil, ioFrame("lease", 8)); err != nil {
				t.Fatalf("send: %v", err)
			}
		}
		got := s.resumeFrom(3)
		if got.gap != nil || len(got.frames) != 0 {
			t.Fatalf("resume from the newest frame owed %d frames and gap %v, want neither",
				len(got.frames), got.gap)
		}
	})

	t.Run("nothing sent yet", func(t *testing.T) {
		s := newClientStream(defaultClientReplayBufferBytes)
		got := s.resumeFrom(0)
		if got.gap != nil || len(got.frames) != 0 {
			t.Fatalf("resume on an untouched stream owed %d frames and gap %v, want neither",
				len(got.frames), got.gap)
		}
	})

	t.Run("partially covered names the evicted range", func(t *testing.T) {
		const (
			limit = 2048
			sent  = 40
		)
		s := newClientStream(limit)
		for i := 0; i < sent; i++ {
			if _, _, err := s.send(nil, ioFrame("lease", 100)); err != nil {
				t.Fatalf("send: %v", err)
			}
		}
		survivors := retainedSeqs(t, s)
		if len(survivors) == 0 || len(survivors) >= sent {
			t.Fatalf("retained %d of %d frames; the fixture must evict some and keep some",
				len(survivors), sent)
		}
		oldest := survivors[0]

		// Resume from the very first frame: everything between it and the oldest
		// survivor is gone, and what survives is still owed.
		got := s.resumeFrom(1)
		if got.gap == nil {
			t.Fatal("no gap reported for a resume point that was evicted")
		}
		if got.gap.Reason != gapReasonEvicted {
			t.Fatalf("gap reason %q, want %q", got.gap.Reason, gapReasonEvicted)
		}
		if got.gap.RequestedFromSeq != 1 {
			t.Fatalf("gap echoes requested_from_seq %d, want 1", got.gap.RequestedFromSeq)
		}
		if got.gap.MissingFromSeq != 2 || got.gap.MissingThroughSeq != oldest-1 {
			t.Fatalf("gap names %d..%d, want %d..%d",
				got.gap.MissingFromSeq, got.gap.MissingThroughSeq, 2, oldest-1)
		}
		if len(got.frames) != len(survivors) {
			t.Fatalf("replayed %d frames, want the %d survivors", len(got.frames), len(survivors))
		}

		// And resuming from the oldest survivor's predecessor is complete: the
		// gap is reported for a genuinely missing range, not for every reconnect.
		if edge := s.resumeFrom(oldest - 1); edge.gap != nil {
			t.Fatalf("gap reported at the exact edge of what is retained: %+v", *edge.gap)
		}
	})

	t.Run("uncovered names every frame sent", func(t *testing.T) {
		// Retention disabled: sequencing continues, nothing is kept, so a resume
		// can be answered only with the truth about what is gone.
		s := newClientStream(0)
		for i := 0; i < 6; i++ {
			if _, _, err := s.send(nil, ioFrame("lease", 8)); err != nil {
				t.Fatalf("send: %v", err)
			}
		}
		got := s.resumeFrom(1)
		if got.gap == nil {
			t.Fatal("no gap reported with retention disabled")
		}
		if got.gap.Reason != gapReasonEvicted {
			t.Fatalf("gap reason %q, want %q", got.gap.Reason, gapReasonEvicted)
		}
		if got.gap.MissingFromSeq != 2 || got.gap.MissingThroughSeq != 6 {
			t.Fatalf("gap names %d..%d, want 2..6", got.gap.MissingFromSeq, got.gap.MissingThroughSeq)
		}
		if len(got.frames) != 0 {
			t.Fatalf("replayed %d frames with retention disabled, want 0", len(got.frames))
		}
	})

	t.Run("resume point ahead of the stream is reported as a restart", func(t *testing.T) {
		// The restart shape: a lease restored from the journal renumbers from 1
		// with an empty buffer, so a client resuming at its pre-restart position
		// is asking for frames that will never exist under this numbering.
		s := newClientStream(defaultClientReplayBufferBytes)
		for i := 0; i < 3; i++ {
			if _, _, err := s.send(nil, ioFrame("lease", 8)); err != nil {
				t.Fatalf("send: %v", err)
			}
		}
		got := s.resumeFrom(500)
		if got.gap == nil {
			t.Fatal("no gap reported for a resume point ahead of the stream")
		}
		if got.gap.Reason != gapReasonRestarted {
			t.Fatalf("gap reason %q, want %q", got.gap.Reason, gapReasonRestarted)
		}
		if got.gap.RequestedFromSeq != 500 {
			t.Fatalf("gap echoes requested_from_seq %d, want 500", got.gap.RequestedFromSeq)
		}
		if got.gap.MissingFromSeq != 0 || got.gap.MissingThroughSeq != 0 {
			t.Fatalf("gap names range %d..%d; a renumbered stream has no nameable missing range",
				got.gap.MissingFromSeq, got.gap.MissingThroughSeq)
		}
		// The whole retained stream is replayed rather than nothing: the client's
		// position is void, so the useful answer is everything under the new
		// numbering.
		if len(got.frames) != 3 {
			t.Fatalf("replayed %d frames after a restart, want all 3 retained", len(got.frames))
		}
	})
}

// TestClientResume_ReplayedFramesPrecedeLiveOnes is the story's core assertion:
// a reconnecting client is handed what it missed, in order, BEFORE anything the
// instance emits after it reconnects.
func TestClientResume_ReplayedFramesPrecedeLiveOnes(t *testing.T) {
	wsURL, reg := newTestGateway(t)
	leaseID, err := reg.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}

	// Five frames stream out with no client attached — the disconnected window.
	sendClientFrames(t, reg, leaseID, 5, 16)

	// The client reconnects claiming it got as far as seq 2.
	client := dialClientWithQuery(t, wsURL, leaseID, fromSeqQueryParam+"=2")
	waitFor(t, func() bool { return reg.ClientConn(leaseID) != nil })

	// Two more frames arrive live, after the attach.
	sendClientFrames(t, reg, leaseID, 2, 16)

	seqs, frames := readSeqs(t, client, 5)
	want := []uint64{3, 4, 5, 6, 7}
	for i := range want {
		if seqs[i] != want[i] {
			t.Fatalf("frame order %v, want %v (replayed 3-5 must precede live 6-7)", seqs, want)
		}
		if frames[i].Signal != brokerframe.SignalIO {
			t.Fatalf("frame %d signal %q, want %q — no gap notice was due",
				i, frames[i].Signal, brokerframe.SignalIO)
		}
	}
}

// TestClientResume_ReplayIsNotTruncatedByTheSendQueue guards the mechanism that
// makes the previous test true for a real disconnection rather than a tiny one.
//
// The replay bound is in BYTES; a megabyte of token deltas is thousands of
// frames, while a connection's send queue holds 256. Staging the replay ahead
// of that queue is what keeps a long disconnection recoverable — pre-filling the
// queue instead would silently drop everything past the 256th frame, out of the
// very buffer that exists to stop frames being lost.
func TestClientResume_ReplayIsNotTruncatedByTheSendQueue(t *testing.T) {
	wsURL, reg := newTestGateway(t)
	leaseID, err := reg.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}

	const sent = 400 // comfortably past the 256-slot send queue
	sendClientFrames(t, reg, leaseID, sent, 8)
	if held := len(retainedLeaseSeqs(t, reg, leaseID)); held != sent {
		t.Fatalf("buffer retained %d of %d frames; the default bound should hold them all", held, sent)
	}

	client := dialClientWithQuery(t, wsURL, leaseID, fromSeqQueryParam+"=0")
	seqs, _ := readSeqs(t, client, sent)
	for i, got := range seqs {
		if got != uint64(i+1) {
			t.Fatalf("frame %d has seq %d, want %d; the replay was truncated or reordered", i, got, i+1)
		}
	}
}

// TestClientResume_GapNamesTheMissingRangeThenReplaysTheSurvivors is the
// handleable-event half: when the buffer can no longer cover the resume point
// the client is TOLD, first, and told which frames are gone.
func TestClientResume_GapNamesTheMissingRangeThenReplaysTheSurvivors(t *testing.T) {
	wsURL, reg := newTestGateway(t)
	// A bound small enough that a modest stream overruns it.
	reg.useClientReplayBuffer(2048)
	leaseID, err := reg.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}

	const sent = 40
	sendClientFrames(t, reg, leaseID, sent, 100)
	survivors := retainedLeaseSeqs(t, reg, leaseID)
	if len(survivors) == 0 || len(survivors) >= sent {
		t.Fatalf("retained %d of %d frames; the fixture must evict some and keep some",
			len(survivors), sent)
	}

	client := dialClientWithQuery(t, wsURL, leaseID, fromSeqQueryParam+"=1")
	waitFor(t, func() bool { return reg.ClientConn(leaseID) != nil })
	sendClientFrames(t, reg, leaseID, 1, 16) // one live frame after the attach

	seqs, frames := readSeqs(t, client, len(survivors)+2)

	gap := decodeGap(t, frames[0])
	if gap.Reason != gapReasonEvicted {
		t.Fatalf("gap reason %q, want %q", gap.Reason, gapReasonEvicted)
	}
	if gap.RequestedFromSeq != 1 {
		t.Fatalf("gap echoes requested_from_seq %d, want 1", gap.RequestedFromSeq)
	}
	if gap.MissingFromSeq != 2 || gap.MissingThroughSeq != survivors[0]-1 {
		t.Fatalf("gap names %d..%d, want 2..%d", gap.MissingFromSeq, gap.MissingThroughSeq, survivors[0]-1)
	}

	// Then the survivors, in order, then the live frame — the gap first, never
	// after frames the client would already have rendered as continuous.
	want := append(append([]uint64{0}, survivors...), sent+1)
	for i := range want {
		if seqs[i] != want[i] {
			t.Fatalf("frame order %v, want %v (gap, survivors, then live)", seqs, want)
		}
	}
}

// TestClientResume_AbsentOrMalformedFromSeqConnectsAsBefore pins the
// backward-compatibility guarantee AND the degradation rule: no parameter
// behaves exactly as it always did, and a value the broker cannot parse is
// treated as absent rather than refused, so a client bug costs the replay and
// not the session.
func TestClientResume_AbsentOrMalformedFromSeqConnectsAsBefore(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{name: "absent", query: ""},
		{name: "empty", query: fromSeqQueryParam + "="},
		{name: "not a number", query: fromSeqQueryParam + "=abc"},
		{name: "negative", query: fromSeqQueryParam + "=-3"},
		{name: "overflow", query: fromSeqQueryParam + "=99999999999999999999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wsURL, reg := newTestGateway(t)
			leaseID, err := reg.NewLease(anonymousOwner())
			if err != nil {
				t.Fatalf("new lease: %v", err)
			}
			sendClientFrames(t, reg, leaseID, 3, 16)

			client := dialClientWithQuery(t, wsURL, leaseID, tc.query)
			waitFor(t, func() bool { return reg.ClientConn(leaseID) != nil })
			sendClientFrames(t, reg, leaseID, 1, 16)

			// The FIRST frame is the live one: nothing buffered was replayed and
			// no gap was announced.
			f := readFrame(t, client)
			if f.Signal != brokerframe.SignalIO || f.Seq != 4 {
				t.Fatalf("first frame is %s seq %d, want io seq 4 — a connect without a "+
					"usable from_seq must see the live stream only", f.Signal, f.Seq)
			}
		})
	}
}

// TestClientResume_TicketAndFromSeqCompose covers the two query parameters
// side by side on an authenticated broker: the resume rides along with the
// credential, and the credential's rules are untouched by it.
func TestClientResume_TicketAndFromSeqCompose(t *testing.T) {
	newLease := func(t *testing.T, env *testGatewayEnv) string {
		t.Helper()
		leaseID, err := env.reg.NewLease(nexusauth.Principal{ID: ownerPrincipal})
		if err != nil {
			t.Fatalf("new lease: %v", err)
		}
		return leaseID
	}

	t.Run("a valid ticket carries the resume", func(t *testing.T) {
		env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), nil)
		leaseID := newLease(t, env)
		sendClientFrames(t, env.reg, leaseID, 3, 16)

		ticket := env.mintTicket(t, leaseID, ownerPrincipal)
		query := fmt.Sprintf("%s=%s&%s=1", ticketQueryParam, url.QueryEscape(ticket), fromSeqQueryParam)
		client := dialClientWithQuery(t, env.wsURL, leaseID, query)

		seqs, _ := readSeqs(t, client, 2)
		if seqs[0] != 2 || seqs[1] != 3 {
			t.Fatalf("replayed %v, want [2 3]", seqs)
		}
	})

	t.Run("a rejected ticket is still final", func(t *testing.T) {
		env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), nil)
		leaseID := newLease(t, env)
		sendClientFrames(t, env.reg, leaseID, 3, 16)

		// A resume request cannot soften the credential: a bad ticket is refused
		// with the same 401 it gets on its own, and no frame is replayed to a
		// caller that never authenticated.
		query := fmt.Sprintf("%s=not-a-real-ticket&%s=1", ticketQueryParam, fromSeqQueryParam)
		_, resp, err := websocket.Dial(t.Context(), env.wsURL+ClientWSPath(leaseID)+"?"+query, nil)
		if err == nil {
			t.Fatal("a rejected ticket connected when accompanied by from_seq")
		}
		if resp == nil || resp.StatusCode != 401 {
			t.Fatalf("handshake status %v, want 401", resp)
		}
	})

	t.Run("a resume does not bypass ownership", func(t *testing.T) {
		env := newTestGatewayEnv(t, mustAuthChain(t, twoPrincipalAuthYAML), nil)
		leaseID := newLease(t, env)
		sendClientFrames(t, env.reg, leaseID, 3, 16)

		// Another principal's bearer token plus a resume: the unknown-lease
		// refusal is unchanged, so a resume cannot be used to read a stream the
		// caller does not own.
		conn, resp, err := websocket.Dial(t.Context(),
			env.wsURL+ClientWSPath(leaseID)+"?"+fromSeqQueryParam+"=1",
			&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + otherToken}}})
		if err == nil {
			_ = conn.Close(websocket.StatusNormalClosure, "")
			t.Fatal("a non-owner connected with a resume request")
		}
		if resp == nil || resp.StatusCode != 404 {
			t.Fatalf("handshake status %v, want 404", resp)
		}
	})
}
