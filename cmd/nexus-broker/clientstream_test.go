package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// newStreamTestRegistry builds a registry with a chosen per-lease replay bound
// and a discarded logger.
func newStreamTestRegistry(t *testing.T, limit int) *Registry {
	t.Helper()
	r := NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	r.useClientReplayBuffer(limit)
	return r
}

// newStreamTestLease mints a lease on r, failing the test if it cannot.
func newStreamTestLease(t *testing.T, r *Registry) string {
	t.Helper()
	id, err := r.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}
	return id
}

// ioFrame is a client-bound IO frame carrying a payload of n bytes of JSON
// string content, so a test can size the buffer in real encoded bytes.
func ioFrame(leaseID string, n int) brokerframe.Frame {
	payload, _ := json.Marshal(strings.Repeat("x", n))
	return brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalIO,
		Payload: payload,
	}
}

// TestClientStream_SequencesAreMonotonicAndPerLease is the core of the story:
// every client-bound frame carries the next sequence for ITS lease, and two
// leases never share a counter.
func TestClientStream_SequencesAreMonotonicAndPerLease(t *testing.T) {
	reg := newStreamTestRegistry(t, defaultClientReplayBufferBytes)
	first := newStreamTestLease(t, reg)
	second := newStreamTestLease(t, reg)

	// Interleave the two leases so a shared counter would show up as a gap.
	for i := 1; i <= 5; i++ {
		gotFirst, _, err := reg.SendClientFrame(first, ioFrame(first, 4))
		if err != nil {
			t.Fatalf("send on first lease: %v", err)
		}
		if gotFirst != uint64(i) {
			t.Fatalf("first lease frame %d got seq %d, want %d", i, gotFirst, i)
		}
		gotSecond, _, err := reg.SendClientFrame(second, ioFrame(second, 4))
		if err != nil {
			t.Fatalf("send on second lease: %v", err)
		}
		if gotSecond != uint64(i) {
			t.Fatalf("second lease frame %d got seq %d, want %d", i, gotSecond, i)
		}
	}
}

// TestClientStream_SequenceReachesTheClientOnTheWire proves the number is not
// merely bookkeeping: it is stamped on the encoded frame the client receives,
// on every signal, counting from 1.
func TestClientStream_SequenceReachesTheClientOnTheWire(t *testing.T) {
	reg := newStreamTestRegistry(t, defaultClientReplayBufferBytes)
	leaseID := newStreamTestLease(t, reg)

	client := newWSConn(nil)
	if _, err := reg.AttachClient(leaseID, client); err != nil {
		t.Fatalf("attach client: %v", err)
	}

	signals := []brokerframe.Signal{
		brokerframe.SignalReady,
		brokerframe.SignalIO,
		brokerframe.SignalShutdown,
	}
	for _, sig := range signals {
		if _, _, err := reg.SendClientFrame(leaseID, brokerframe.Frame{LeaseID: leaseID, Signal: sig}); err != nil {
			t.Fatalf("send %s: %v", sig, err)
		}
	}

	for i, want := range signals {
		data := <-client.send
		f, err := brokerframe.Decode(data)
		if err != nil {
			t.Fatalf("decode client frame %d: %v", i, err)
		}
		if f.Signal != want {
			t.Fatalf("frame %d: got signal %q, want %q", i, f.Signal, want)
		}
		if f.Seq != uint64(i+1) {
			t.Fatalf("frame %d (%s): got seq %d, want %d", i, want, f.Seq, i+1)
		}
	}
}

// TestClientStream_InstanceBoundFramesAreNotSequenced pins the decided scope:
// only client-bound frames carry a sequence, so the dial-back side is untouched.
func TestClientStream_InstanceBoundFramesAreNotSequenced(t *testing.T) {
	reg := newStreamTestRegistry(t, defaultClientReplayBufferBytes)
	leaseID := newStreamTestLease(t, reg)

	instance := newWSConn(nil)
	reg.SetSpawnSecret(leaseID, testSpawnSecret)
	if err := reg.AttachInstance(leaseID, instance, testSpawnSecret); err != nil {
		t.Fatalf("attach instance: %v", err)
	}

	gw := NewGateway(slog.New(slog.NewTextHandler(io.Discard, nil)), reg, nil, nil)
	t.Cleanup(gw.Shutdown)

	frame := ioFrame(leaseID, 4)
	raw, err := brokerframe.Encode(frame)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	gw.forwardToInstance(leaseID, frame, raw)

	got, err := brokerframe.Decode(<-instance.send)
	if err != nil {
		t.Fatalf("decode instance frame: %v", err)
	}
	if got.Seq != 0 {
		t.Fatalf("instance-bound frame carries seq %d; instance-bound frames must not be sequenced", got.Seq)
	}
}

// TestClientStream_SecretIsStrippedOnClientBoundFrames guards the invariant the
// Frame doc states: the per-spawn secret must never be echoed to a client. The
// re-encode that stamps the sequence is where that is enforced.
func TestClientStream_SecretIsStrippedOnClientBoundFrames(t *testing.T) {
	reg := newStreamTestRegistry(t, defaultClientReplayBufferBytes)
	leaseID := newStreamTestLease(t, reg)

	client := newWSConn(nil)
	if _, err := reg.AttachClient(leaseID, client); err != nil {
		t.Fatalf("attach client: %v", err)
	}

	if _, _, err := reg.SendClientFrame(leaseID, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalIO,
		Secret:  testSpawnSecret,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	data := <-client.send
	if strings.Contains(string(data), testSpawnSecret) {
		t.Fatalf("client-bound frame carries the spawn secret: %s", data)
	}
	f, err := brokerframe.Decode(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Secret != "" {
		t.Fatalf("client-bound frame carries secret %q", f.Secret)
	}
}

// TestClientStream_NoClientAttachedIsBufferedNotLost covers the first of the two
// loss paths: a frame produced while nothing is connected is still sequenced and
// still retained.
func TestClientStream_NoClientAttachedIsBufferedNotLost(t *testing.T) {
	reg := newStreamTestRegistry(t, defaultClientReplayBufferBytes)
	leaseID := newStreamTestLease(t, reg)

	seq, outcome, err := reg.SendClientFrame(leaseID, ioFrame(leaseID, 16))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if seq != 1 {
		t.Fatalf("got seq %d, want 1", seq)
	}
	if outcome != clientFrameBuffered {
		t.Fatalf("got outcome %v, want clientFrameBuffered", outcome)
	}

	frames, complete, err := reg.ClientReplayFrom(leaseID, 0)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !complete || len(frames) != 1 {
		t.Fatalf("got %d frames complete=%v, want 1 frame complete=true", len(frames), complete)
	}
}

// TestClientStream_FullSendQueueStillBuffers covers the second loss path: an
// attached client whose 256-slot send queue is full. The existing drop-on-full
// behaviour is preserved (the frame does NOT reach the socket) but the frame is
// retained so it is recoverable, and the caller learns which happened.
func TestClientStream_FullSendQueueStillBuffers(t *testing.T) {
	reg := newStreamTestRegistry(t, defaultClientReplayBufferBytes)
	leaseID := newStreamTestLease(t, reg)

	client := newWSConn(nil)
	if _, err := reg.AttachClient(leaseID, client); err != nil {
		t.Fatalf("attach client: %v", err)
	}
	// Fill the send queue so the next queue() returns false, exactly as a client
	// that has stopped reading its socket would.
	for len(client.send) < cap(client.send) {
		if !client.queue([]byte("{}")) {
			t.Fatal("queue refused before the buffer was full")
		}
	}

	seq, outcome, err := reg.SendClientFrame(leaseID, ioFrame(leaseID, 16))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if outcome != clientFrameDropped {
		t.Fatalf("got outcome %v, want clientFrameDropped", outcome)
	}
	if seq != 1 {
		t.Fatalf("got seq %d, want 1", seq)
	}
	frames, complete, err := reg.ClientReplayFrom(leaseID, 0)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !complete || len(frames) != 1 {
		t.Fatalf("a frame dropped by a full send queue was not retained: %d frames complete=%v",
			len(frames), complete)
	}
}

// retainedSeqs decodes the sequences currently held in the buffer, oldest
// first, so a test can assert WHICH frames survived rather than only how many.
func retainedSeqs(t *testing.T, s *clientStream) []uint64 {
	t.Helper()
	frames, _ := s.replayFrom(0)
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

// TestClientStream_EvictsOldestAtByteBound is the bound assertion: retention is
// capped in BYTES, not frames, and the oldest go first — so what survives is
// always the most recent contiguous tail.
//
// The bound is deliberately expressed in bytes rather than "n frames": the
// encoded size of a frame varies (the sequence itself grows a digit at a time),
// which is exactly why a frame count is the wrong unit for a memory bound.
func TestClientStream_EvictsOldestAtByteBound(t *testing.T) {
	const limit = 2048
	s := newClientStream(limit)

	const sent = 40
	for i := 0; i < sent; i++ {
		if _, _, err := s.send(ioFrame("lease", 100)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	if got := s.bufferedBytes(); got > limit {
		t.Fatalf("buffered %d bytes, over the %d-byte bound", got, limit)
	}
	held := s.bufferedFrames()
	if held == 0 || held >= sent {
		t.Fatalf("buffered %d of %d frames; want some evicted and some retained", held, sent)
	}
	if got := s.lastSeq(); got != sent {
		t.Fatalf("last seq %d, want %d — eviction must not disturb the counter", got, sent)
	}

	// The survivors are the NEWEST frames, contiguous, ending at the last sent.
	seqs := retainedSeqs(t, s)
	if len(seqs) != held {
		t.Fatalf("replay returned %d frames but %d are buffered", len(seqs), held)
	}
	if seqs[len(seqs)-1] != sent {
		t.Fatalf("newest retained seq is %d, want %d — eviction took the wrong end", seqs[len(seqs)-1], sent)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("retained sequences are not contiguous: %v", seqs)
		}
	}

	// Asking for a range whose head was evicted is reported as incomplete, and
	// asking from the oldest survivor is not.
	if _, complete := s.replayFrom(0); complete {
		t.Fatalf("replay from 0 reported complete, but %d frames were evicted", sent-held)
	}
	frames, complete := s.replayFrom(seqs[0] - 1)
	if !complete || len(frames) != held {
		t.Fatalf("replay from the oldest survivor: %d frames complete=%v, want %d complete=true",
			len(frames), complete, held)
	}
}

// TestClientStream_OversizedFrameIsNotRetained pins the honest reading of the
// bound: a single frame bigger than the whole buffer leaves the buffer empty
// rather than breaching it.
func TestClientStream_OversizedFrameIsNotRetained(t *testing.T) {
	s := newClientStream(64)
	if _, _, err := s.send(ioFrame("lease", 4096)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := s.bufferedBytes(); got != 0 {
		t.Fatalf("buffered %d bytes for a frame larger than the 64-byte bound, want 0", got)
	}
	if got := s.lastSeq(); got != 1 {
		t.Fatalf("last seq %d, want 1 — an unretained frame still consumes a sequence", got)
	}
}

// TestClientStream_ZeroBoundSequencesWithoutRetaining covers the documented
// meaning of `client_replay_buffer_bytes: 0`: loss stays DETECTABLE, nothing is
// kept.
func TestClientStream_ZeroBoundSequencesWithoutRetaining(t *testing.T) {
	s := newClientStream(0)
	for i := 0; i < 3; i++ {
		seq, _, err := s.send(ioFrame("lease", 32))
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		if seq != uint64(i+1) {
			t.Fatalf("got seq %d, want %d", seq, i+1)
		}
	}
	if got := s.bufferedBytes(); got != 0 {
		t.Fatalf("buffered %d bytes at a zero bound, want 0", got)
	}
}

// TestClientStream_RemoveReleasesBufferMemory checks the teardown sink: the
// retained bytes are freed by Registry.Remove, not merely orphaned for the
// collector — which is what a still-held lease pointer would otherwise pin.
func TestClientStream_RemoveReleasesBufferMemory(t *testing.T) {
	reg := newStreamTestRegistry(t, defaultClientReplayBufferBytes)
	leaseID := newStreamTestLease(t, reg)

	for i := 0; i < 20; i++ {
		if _, _, err := reg.SendClientFrame(leaseID, ioFrame(leaseID, 512)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	// Hold the stream the way anything outliving the map entry would.
	reg.mu.Lock()
	stream := reg.leases[leaseID].clientStream
	reg.mu.Unlock()
	if stream.bufferedBytes() == 0 {
		t.Fatal("nothing buffered before Remove; the test would prove nothing")
	}

	reg.Remove(leaseID)

	if got := stream.bufferedBytes(); got != 0 {
		t.Fatalf("%d bytes still retained after Remove", got)
	}
	if got := stream.bufferedFrames(); got != 0 {
		t.Fatalf("%d frames still retained after Remove", got)
	}
	if _, _, err := reg.SendClientFrame(leaseID, ioFrame(leaseID, 8)); !errors.Is(err, errUnknownLease) {
		t.Fatalf("send on a removed lease: got %v, want errUnknownLease", err)
	}
	if _, _, err := reg.ClientReplayFrom(leaseID, 0); !errors.Is(err, errUnknownLease) {
		t.Fatalf("replay on a removed lease: got %v, want errUnknownLease", err)
	}
}

// TestClientStream_ReplayFrom covers the seam the resume handshake reads from.
func TestClientStream_ReplayFrom(t *testing.T) {
	tests := []struct {
		name         string
		limit        int
		sent         int
		after        uint64
		wantFrames   int
		wantComplete bool
	}{
		{name: "nothing sent, caller at zero", limit: 4096, sent: 0, after: 0, wantFrames: 0, wantComplete: true},
		{name: "caller is current", limit: 4096, sent: 4, after: 4, wantFrames: 0, wantComplete: true},
		{name: "caller is behind, all retained", limit: 4096, sent: 4, after: 2, wantFrames: 2, wantComplete: true},
		{name: "caller ahead of the broker", limit: 4096, sent: 2, after: 9, wantFrames: 0, wantComplete: false},
		{name: "retention disabled", limit: 0, sent: 3, after: 0, wantFrames: 0, wantComplete: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newClientStream(tc.limit)
			for i := 0; i < tc.sent; i++ {
				if _, _, err := s.send(ioFrame("lease", 50)); err != nil {
					t.Fatalf("send %d: %v", i, err)
				}
			}
			frames, complete := s.replayFrom(tc.after)
			if len(frames) != tc.wantFrames {
				t.Fatalf("got %d frames, want %d", len(frames), tc.wantFrames)
			}
			if complete != tc.wantComplete {
				t.Fatalf("got complete=%v, want %v", complete, tc.wantComplete)
			}
		})
	}
}

// TestClientStream_ConcurrentSendsKeepWireOrder proves the lock covers sequence
// assignment AND the hand-off, so the bytes on the socket are in sequence order
// even under concurrency. It is the assertion `go test -race` backs up.
func TestClientStream_ConcurrentSendsKeepWireOrder(t *testing.T) {
	s := newClientStream(1 << 20)
	client := newWSConn(nil)
	if _, evicted := s.attach(client, nil, nil); evicted != nil {
		t.Fatalf("attach displaced %v on a fresh stream", evicted)
	}

	const senders, each = 8, 32
	done := make(chan struct{})
	for i := 0; i < senders; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < each; j++ {
				if _, _, err := s.send(ioFrame("lease", 8)); err != nil {
					t.Errorf("send: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < senders; i++ {
		<-done
	}

	var prev uint64
	for i := 0; i < senders*each; i++ {
		select {
		case data := <-client.send:
			f, err := brokerframe.Decode(data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if f.Seq != prev+1 {
				t.Fatalf("frame %d on the wire carries seq %d, want %d — wire order must match sequence order",
					i, f.Seq, prev+1)
			}
			prev = f.Seq
		default:
			t.Fatalf("only %d of %d frames reached the send queue", i, senders*each)
		}
	}
}

// TestClientReplayBufferBytes_Config covers the config key: its default, the
// explicit disable, and the refusal of a value with no reading.
func TestClientReplayBufferBytes_Config(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		want    int
		wantErr string
	}{
		{
			name: "absent takes the documented default",
			yaml: "listen_addr: \":8080\"\n",
			want: defaultClientReplayBufferBytes,
		},
		{
			name: "explicit value is honoured",
			yaml: "client_replay_buffer_bytes: 4096\n",
			want: 4096,
		},
		{
			name: "zero disables retention",
			yaml: "client_replay_buffer_bytes: 0\n",
			want: 0,
		},
		{
			name:    "negative is a boot failure",
			yaml:    "client_replay_buffer_bytes: -1\n",
			wantErr: "client_replay_buffer_bytes",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfigFromBytes([]byte(tc.yaml))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got error %v, want one naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("load config: %v", err)
			}
			if cfg.ClientReplayBufferBytes != tc.want {
				t.Fatalf("got %d, want %d", cfg.ClientReplayBufferBytes, tc.want)
			}
		})
	}
}

// TestGateway_ClientBoundFramesAreSequencedEndToEnd drives the real gateway over
// real sockets: everything an instance sends its client arrives sequenced from 1.
func TestGateway_ClientBoundFramesAreSequencedEndToEnd(t *testing.T) {
	wsURL, registry := newTestGateway(t)

	leaseID, err := registry.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("new lease: %v", err)
	}

	registry.SetSpawnSecret(leaseID, testSpawnSecret)
	instance := dial(t, wsURL+instanceWSPath)
	defer instance.Close(websocket.StatusNormalClosure, "")
	writeFrame(t, instance, brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalRegister,
		Secret:  testSpawnSecret,
	})
	waitFor(t, func() bool { return registry.InstanceConn(leaseID) != nil })

	client := dial(t, wsURL+ClientWSPath(leaseID))
	defer client.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return registry.ClientConn(leaseID) != nil })

	const count = 5
	for i := 0; i < count; i++ {
		writeFrame(t, instance, brokerframe.Frame{
			LeaseID: leaseID,
			Signal:  brokerframe.SignalIO,
			Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)),
		})
	}
	for i := 0; i < count; i++ {
		got := readFrame(t, client)
		if got.Seq != uint64(i+1) {
			t.Fatalf("client frame %d got seq %d, want %d", i, got.Seq, i+1)
		}
		if string(got.Payload) != fmt.Sprintf(`{"n":%d}`, i) {
			t.Fatalf("client frame %d got payload %s", i, got.Payload)
		}
	}

	// The same frames the client saw are still retained for replay.
	frames, complete, err := registry.ClientReplayFrom(leaseID, 0)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !complete || len(frames) != count {
		t.Fatalf("replay: %d frames complete=%v, want %d complete=true", len(frames), complete, count)
	}
}
