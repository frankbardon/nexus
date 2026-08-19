package main

import (
	"container/list"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/frankbardon/nexus/pkg/brokerframe"
)

// defaultClientReplayBufferBytes is the per-lease replay bound used when
// `client_replay_buffer_bytes` is not configured.
//
// 1 MiB is chosen against the shape of the traffic rather than a frame count:
// a lease's client-bound stream is mostly token deltas of a few bytes each,
// punctuated by the occasional tool result of tens of kilobytes. A megabyte
// therefore holds many seconds of streaming for a chatty agent, and still holds
// several whole tool results for a lease that only emits large frames — while
// bounding the broker's exposure to bound x max_concurrent (8 MiB at the
// default cap of 8).
const defaultClientReplayBufferBytes = 1 << 20

// clientFrameOutcome reports what happened to a client-bound frame AFTER the
// broker sequenced it and retained it for replay. The sequence is assigned and
// the frame is buffered in every case: the outcome describes delivery, not
// retention, which is the distinction that makes a gap recoverable.
type clientFrameOutcome int

const (
	// clientFrameDelivered means the frame was handed to an attached client's
	// send queue.
	clientFrameDelivered clientFrameOutcome = iota

	// clientFrameBuffered means no client was attached, so the frame was
	// retained for replay and nothing else.
	clientFrameBuffered

	// clientFrameDropped means a client WAS attached but its send queue was
	// full, so the frame never reached the socket. It is retained for replay
	// exactly as the unattached case is.
	clientFrameDropped
)

// bufferedFrame is one retained client-bound frame: its broker-assigned
// sequence and the exact bytes that were (or would have been) written.
type bufferedFrame struct {
	seq  uint64
	data []byte
}

// clientStream is one lease's client-bound frame sequencer and replay buffer.
//
// It exists because the gateway's two loss paths — no client attached, and an
// attached client whose 256-slot send queue is full — both used to log and
// continue, leaving the client with a silently truncated stream. Sequencing
// makes the loss DETECTABLE and the buffer makes it RECOVERABLE.
//
// The buffer is bounded in BYTES, not frames. Client-bound payloads range from
// a few-byte token delta to a hundred-kilobyte tool result, so a frame count
// says nothing about how much memory a lease can pin; a byte bound says exactly.
// Oldest frames are evicted first, so what survives is always the most recent
// tail — the part a reconnecting client actually needs.
//
// It is entirely IN-MEMORY and dies with its lease. Nothing here is journaled,
// so a broker restart starts every lease's sequence again at 1 with an empty
// buffer.
//
// It owns its own mutex rather than borrowing Registry.mu, and that lock is held
// across sequence assignment, buffering AND the non-blocking hand-off to the
// client's send queue. That is what guarantees the bytes reach the socket in
// sequence order even if two goroutines ever send on one lease at once: a client
// that sees seq N+1 can trust it did not miss N to a race.
type clientStream struct {
	mu sync.Mutex

	// next is the sequence the next frame will carry. Sequences count from 1,
	// so zero on the wire always means "unsequenced".
	next uint64

	// limit is the byte bound. Zero (or negative) disables retention entirely:
	// frames are still sequenced, so loss stays detectable, but nothing is kept.
	limit int

	// bytes is the sum of len(data) over buf. Tracked incrementally rather than
	// recomputed so eviction stays O(evicted) instead of O(buffered).
	bytes int

	// buf holds *bufferedFrame oldest-first.
	buf *list.List
}

// newClientStream returns a stream that retains at most limit bytes. A
// non-positive limit disables retention while leaving sequencing intact.
func newClientStream(limit int) *clientStream {
	return &clientStream{next: 1, limit: limit, buf: list.New()}
}

// send sequences one client-bound frame, retains it for replay, and hands it to
// conn's send queue. conn may be nil, which is the "no client attached" case:
// the frame is still sequenced and still retained, so a client that attaches (or
// re-attaches) later can be told what it missed.
//
// The frame is RE-ENCODED here, which is also where Secret is cleared: a
// client-bound frame must never carry the per-spawn secret, and re-encoding is
// the one place every such frame passes through.
//
// It returns the assigned sequence and what became of the frame. An encode
// failure returns the error WITHOUT consuming a sequence, so the numbering a
// client sees stays gapless for reasons that are not loss.
func (s *clientStream) send(conn *wsConn, f brokerframe.Frame) (uint64, clientFrameOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f.Seq = s.next
	f.Secret = ""
	data, err := brokerframe.Encode(f)
	if err != nil {
		return 0, clientFrameDropped, err
	}
	seq := s.next
	s.next++
	s.retainLocked(seq, data)

	if conn == nil {
		return seq, clientFrameBuffered, nil
	}
	if !conn.queue(data) {
		return seq, clientFrameDropped, nil
	}
	return seq, clientFrameDelivered, nil
}

// retainLocked appends one frame to the buffer and evicts oldest-first until
// the byte bound is satisfied. Caller must hold s.mu.
//
// A single frame larger than the whole bound is NOT retained: it is appended
// and then immediately evicted by the loop, which leaves the buffer empty rather
// than over its bound. That is the honest outcome — the alternative is a bound
// that one big tool result can breach.
func (s *clientStream) retainLocked(seq uint64, data []byte) {
	if s.limit <= 0 {
		return
	}
	s.buf.PushBack(&bufferedFrame{seq: seq, data: data})
	s.bytes += len(data)
	for s.bytes > s.limit {
		front := s.buf.Front()
		if front == nil {
			// Unreachable while bytes is tracked correctly; belt and braces so a
			// bookkeeping bug can never spin here.
			s.bytes = 0
			return
		}
		s.bytes -= len(front.Value.(*bufferedFrame).data)
		s.buf.Remove(front)
	}
}

// replayFrom returns the retained frames whose sequence is greater than after,
// oldest first, and reports whether the buffer can still account for EVERY frame
// in that range.
//
// complete is false when the requested resume point has already been evicted —
// i.e. the caller asked for frames the broker no longer holds. A resume path
// must treat that as an unrecoverable gap and say so, rather than replaying a
// partial tail that would read as a complete one.
//
// This is the seam the resume handshake reads from; nothing in this story calls
// it beyond its tests.
func (s *clientStream) replayFrom(after uint64) ([][]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replayFromLocked(after)
}

// replayFromLocked is replayFrom's body. Caller must hold s.mu. It exists so
// resumeFrom can take the replay AND the surrounding sequence bounds under a
// single acquisition: reading them separately would let an eviction land in
// between and make the reported missing range describe a buffer state that
// never existed.
func (s *clientStream) replayFromLocked(after uint64) ([][]byte, bool) {
	// Nothing has been sent yet, or the caller is already current: an empty
	// replay is complete by definition.
	if after >= s.next-1 {
		return nil, after == s.next-1
	}

	var out [][]byte
	complete := false
	for e := s.buf.Front(); e != nil; e = e.Next() {
		bf := e.Value.(*bufferedFrame)
		if bf.seq <= after {
			continue
		}
		if !complete {
			// The first frame we keep must be exactly the one after the caller's
			// position; anything later means the gap was evicted.
			complete = bf.seq == after+1
		}
		out = append(out, bf.data)
	}
	return out, complete
}

// Gap reasons. They are wire values on a stream-gap frame, so a client can
// branch on them without parsing prose.
const (
	// gapReasonEvicted means the frames immediately after the client's resume
	// point have already been evicted from the replay buffer. The missing range
	// names exactly which frames the broker can no longer supply.
	gapReasonEvicted = "evicted"

	// gapReasonRestarted means the resume point is AHEAD of this lease's
	// stream — the client claims to have seen frames the broker has not sent.
	// The one way that happens in practice is a lease restored from the journal
	// after a broker restart: the buffer is in-memory, so the restored lease
	// numbers again from 1. The client's position is void, not merely stale, so
	// the broker replays everything it holds and says why.
	gapReasonRestarted = "restarted"
)

// clientGap is the payload of a stream-gap frame: the range of client-bound
// frames the broker cannot supply to a resuming client, and why.
//
// It is a NAMED RANGE rather than a bare "you missed something" flag because
// the client is the only party that can decide what to do about it, and that
// decision depends on how much is gone: a UI can re-render from its own
// transcript for a handful of token deltas and must tell the user for a
// thousand.
type clientGap struct {
	// Reason is gapReasonEvicted or gapReasonRestarted.
	Reason string `json:"reason"`

	// RequestedFromSeq echoes the from_seq the client presented, so a client
	// with several sockets in flight can tell which request this answers.
	RequestedFromSeq uint64 `json:"requested_from_seq"`

	// MissingFromSeq and MissingThroughSeq are the inclusive bounds of the
	// frames the broker no longer holds. Both are omitted when nothing is
	// nameable — a restarted stream that has sent nothing yet, or one whose
	// buffer still covers its whole history.
	MissingFromSeq    uint64 `json:"missing_from_seq,omitempty"`
	MissingThroughSeq uint64 `json:"missing_through_seq,omitempty"`
}

// encode renders the gap as the client-bound frame that announces it. The frame
// is deliberately unsequenced (Seq stays zero) — see SignalStreamGap.
func (g clientGap) encode(leaseID string) ([]byte, error) {
	payload, err := json.Marshal(g)
	if err != nil {
		return nil, fmt.Errorf("encoding stream gap payload: %w", err)
	}
	data, err := brokerframe.Encode(brokerframe.Frame{
		LeaseID: leaseID,
		Signal:  brokerframe.SignalStreamGap,
		Payload: payload,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding stream gap frame: %w", err)
	}
	return data, nil
}

// clientResume is what a reconnecting client is owed: the retained frames to
// replay, oldest first, and the gap that precedes them when the buffer could
// not cover the requested resume point.
type clientResume struct {
	// frames are the retained encoded frames with seq > the effective resume
	// point, oldest first.
	frames [][]byte

	// gap is nil when the buffer covered the resume point exactly. When it is
	// set the client must be told BEFORE it is handed frames.
	gap *clientGap

	// lastSeq is the highest sequence the lease had assigned at the instant the
	// resume was computed. It is for the audit record, not the wire.
	lastSeq uint64
}

// resumeFrom computes what a client resuming after sequence `after` is owed:
// the retained tail, plus an explicit gap when the buffer no longer reaches
// back to that point.
//
// Everything is read under ONE acquisition of s.mu, so the replayed frames and
// the range the gap names always describe the same buffer.
//
// Two shapes of gap are possible and they are told apart deliberately:
//
//   - The resume point is behind what the buffer still holds — frames were
//     evicted. The client missed a specific, nameable range.
//   - The resume point is ahead of the stream — the client's numbering came
//     from a different stream (a lease restored across a broker restart
//     renumbers from 1). Reporting that as an eviction would name an inverted
//     range and tell the client to expect frames that will never come, so it
//     gets its own reason and the whole retained buffer is replayed instead.
func (s *clientStream) resumeFrom(after uint64) clientResume {
	s.mu.Lock()
	defer s.mu.Unlock()

	last := s.next - 1
	reason := ""
	effective := after
	if after > last {
		// The client is ahead of us. Its position means nothing against THIS
		// stream, so resume it from the start of what we hold rather than from a
		// sequence that will never be reached.
		effective = 0
		reason = gapReasonRestarted
	}

	frames, complete := s.replayFromLocked(effective)
	if complete && reason == "" {
		return clientResume{frames: frames, lastSeq: last}
	}
	if reason == "" {
		reason = gapReasonEvicted
	}

	gap := clientGap{Reason: reason, RequestedFromSeq: after}
	if !complete {
		// The missing range runs from the first frame the client did not have to
		// the last one the buffer cannot produce: either the frame before the
		// oldest one still retained, or — with nothing retained at all — every
		// frame the lease has sent.
		gap.MissingFromSeq = effective + 1
		gap.MissingThroughSeq = last
		if front := s.buf.Front(); front != nil {
			gap.MissingThroughSeq = front.Value.(*bufferedFrame).seq - 1
		}
	}
	return clientResume{frames: frames, gap: &gap, lastSeq: last}
}

// lastSeq returns the sequence of the most recently assigned frame, or 0 if the
// stream has sent nothing.
func (s *clientStream) lastSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next - 1
}

// bufferedBytes and bufferedFrames report the current retention, for tests and
// for the operator-facing surfaces a later story may want.
func (s *clientStream) bufferedBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

func (s *clientStream) bufferedFrames() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

// release drops every retained frame and frees the buffer's memory. It is
// called from Registry.Remove — the single teardown sink — so a lease's
// retention cannot outlive the lease even if something else still holds a
// pointer to it (a projected record, an A2A binding, a test).
//
// The sequence counter is deliberately NOT reset: a released lease is gone, and
// leaving the counter alone keeps a late send after teardown from re-issuing a
// sequence a client has already seen.
func (s *clientStream) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Init()
	s.bytes = 0
}
