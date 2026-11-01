package tap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/nexusauth"
)

const (
	eventsPath  = "/events"
	healthzPath = "/healthz"

	backfillNone    = "none"
	backfillRing    = "ring"
	backfillJournal = "journal"

	// Control frames use a named SSE event so a consumer's onmessage handler
	// sees envelopes only; envelopes themselves carry no event: line, which
	// is what lets a generic EventSource.onmessage take the whole firehose.
	ctlGap      = "gap"
	ctlOverflow = "overflow"
)

type server struct {
	cfg        config
	hub        *hub
	journalDir string // empty when there is no session
	logger     *slog.Logger

	srv  *http.Server
	addr string
}

func (s *server) start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+eventsPath, s.handleEvents)
	mux.HandleFunc("GET "+healthzPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"epoch":   s.hub.epoch,
			"clients": s.hub.clientCount(),
		})
	})

	ln, err := net.Listen("tcp", s.cfg.bind)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.cfg.bind, err)
	}
	s.addr = ln.Addr().String()
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("tap server error", "error", err)
		}
	}()
	s.logger.Info("tap server started", "addr", "http://"+s.addr+eventsPath)
	return nil
}

func (s *server) shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// streamRequest is the parsed consumer choice for one connection.
type streamRequest struct {
	filter   typeFilter
	backfill string
	sinceSeq uint64
	// lastEventID is the raw Last-Event-ID (header, or last_event_id query
	// for clients that cannot set headers); empty when absent.
	lastEventID string
}

func parseStreamRequest(r *http.Request) (streamRequest, error) {
	q := r.URL.Query()
	req := streamRequest{
		filter: typeFilter{
			include: splitPatterns(q.Get("types")),
			exclude: splitPatterns(q.Get("exclude")),
		},
		backfill:    backfillNone,
		lastEventID: strings.TrimSpace(r.Header.Get("Last-Event-ID")),
	}
	if req.lastEventID == "" {
		req.lastEventID = strings.TrimSpace(q.Get("last_event_id"))
	}
	switch b := strings.TrimSpace(q.Get("backfill")); b {
	case "", backfillNone:
	case backfillRing, backfillJournal:
		req.backfill = b
	default:
		return streamRequest{}, fmt.Errorf("backfill must be one of none, ring, journal; got %q", b)
	}
	if v := strings.TrimSpace(q.Get("since_seq")); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return streamRequest{}, fmt.Errorf("since_seq %q is not an unsigned integer", v)
		}
		req.sinceSeq = n
	}
	return req, nil
}

// parseEventID splits "<epoch>:<cursor>".
func parseEventID(id string) (epoch string, cursor uint64, ok bool) {
	i := strings.LastIndexByte(id, ':')
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.ParseUint(id[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return id[:i], n, true
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.cfg.chain.Enabled() {
		if _, err := s.cfg.chain.Validate(r.Context(), r); err != nil {
			denyAuth(w, r, err, s.logger)
			return
		}
	}

	req, err := parseStreamRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	reg, ok := s.hub.register(req.filter, s.cfg.clientBuffer)
	if !ok {
		http.Error(w, "tap shutting down", http.StatusServiceUnavailable)
		return
	}
	c := reg.client
	defer s.hub.unregister(c)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sw := &sseWriter{w: w, f: flusher, epoch: s.hub.epoch}

	// journalSent holds the seqs sent from the journal so the live queue can
	// skip the same events: the journal's wildcard runs before ours on the
	// same dispatch, so an event can be flushed to disk, read here, and then
	// arrive live.
	var journalSent map[uint64]struct{}

	resumed := false
	if req.lastEventID != "" {
		epoch, cursor, ok := parseEventID(req.lastEventID)
		switch {
		case ok && epoch == s.hub.epoch && reg.canResume(cursor):
			for _, f := range reg.snapshot {
				if f.cursor > cursor && req.filter.match(f.typ) {
					sw.frame(f)
				}
			}
			resumed = true
		case ok && epoch != s.hub.epoch:
			sw.control(ctlGap, map[string]any{"reason": "epoch_changed", "last_event_id": req.lastEventID})
		default:
			sw.control(ctlGap, map[string]any{"reason": "resume_point_evicted", "last_event_id": req.lastEventID})
		}
	}

	if !resumed {
		switch req.backfill {
		case backfillRing:
			s.sendRing(sw, req, reg)
		case backfillJournal:
			journalSent = s.sendJournal(sw, req, reg)
			s.sendRing(sw, req, reg)
		}
	}
	sw.flush()

	var tick <-chan time.Time
	if s.cfg.heartbeat > 0 {
		t := time.NewTicker(s.cfg.heartbeat)
		defer t.Stop()
		tick = t.C
	}

	for {
		select {
		case f := <-c.ch:
			if _, dup := journalSent[f.seq]; dup && f.seq != 0 {
				continue
			}
			sw.frame(f)
			sw.flush()
		case <-c.gone:
			// Deliver what is already queued before saying why the stream
			// ends, so the overflow frame names the true last cursor.
		drain:
			for {
				select {
				case f := <-c.ch:
					if _, dup := journalSent[f.seq]; dup && f.seq != 0 {
						continue
					}
					sw.frame(f)
				default:
					break drain
				}
			}
			if c.dropReason == dropOverflow {
				sw.control(ctlOverflow, map[string]any{"last_event_id": sw.lastID})
				s.logger.Warn("tap client dropped: queue overflow", "remote", r.RemoteAddr, "last_event_id", sw.lastID)
			}
			sw.flush()
			return
		case <-tick:
			sw.comment("ping")
			sw.flush()
		case <-r.Context().Done():
			return
		}
		if sw.err != nil {
			return
		}
	}
}

func (s *server) sendRing(sw *sseWriter, req streamRequest, reg registration) {
	for _, f := range reg.snapshot {
		if f.seq != 0 && f.seq <= req.sinceSeq {
			continue
		}
		if req.filter.match(f.typ) {
			sw.frame(f)
		}
	}
}

// sendJournal replays the session journal for seqs not already in the ring
// snapshot, and reports a gap when the journal and ring together cannot cover
// what the hub lost. The journal flushes on turn boundaries by default, so the
// file routinely trails the bus by the current turn; the ring is what covers
// that tail.
func (s *server) sendJournal(sw *sseWriter, req streamRequest, reg registration) map[uint64]struct{} {
	sent := make(map[uint64]struct{})
	if s.journalDir == "" {
		sw.control(ctlGap, map[string]any{"reason": "journal_unavailable"})
		return sent
	}
	rd, err := journal.Open(s.journalDir)
	if err != nil {
		sw.control(ctlGap, map[string]any{"reason": "journal_unavailable", "error": err.Error()})
		return sent
	}

	inRing := make(map[uint64]struct{}, len(reg.snapshot))
	for _, f := range reg.snapshot {
		if f.seq != 0 {
			inRing[f.seq] = struct{}{}
		}
	}

	var last uint64
	err = rd.Iter(func(env journal.Envelope) bool {
		if env.Seq > last {
			last = env.Seq
		}
		if env.Seq <= req.sinceSeq {
			return true
		}
		if _, ok := inRing[env.Seq]; ok {
			return true
		}
		if !s.ceiling().match(env.Type) || !req.filter.match(env.Type) {
			return true
		}
		data, merr := json.Marshal(env)
		if merr != nil {
			return true
		}
		sw.raw(data)
		sent[env.Seq] = struct{}{}
		return sw.err == nil
	})
	if err != nil {
		sw.control(ctlGap, map[string]any{"reason": "journal_read_error", "error": err.Error()})
	}

	// Lost = seqs neither on disk nor still in the ring: those below the
	// first seq the hub ever saw, and those it evicted. Seqs at or below the
	// journal's last are on disk (the writer flushes in seq order).
	lostThrough := reg.evictedMaxSeq
	if reg.minSeq > 1 && reg.minSeq-1 > lostThrough {
		lostThrough = reg.minSeq - 1
	}
	if lostThrough > last && lostThrough > req.sinceSeq {
		after := last
		if req.sinceSeq > after {
			after = req.sinceSeq
		}
		sw.control(ctlGap, map[string]any{
			"reason":      "not_yet_journaled",
			"after_seq":   after,
			"through_seq": lostThrough,
		})
	}
	return sent
}

func (s *server) ceiling() typeFilter {
	return typeFilter{include: s.cfg.include, exclude: s.cfg.exclude}
}

// sseWriter writes SSE frames and latches the first write error.
type sseWriter struct {
	w      http.ResponseWriter
	f      http.Flusher
	epoch  string
	lastID string
	err    error
}

func (sw *sseWriter) write(s string) {
	if sw.err != nil {
		return
	}
	_, sw.err = sw.w.Write([]byte(s))
}

// frame writes a ring/live frame with its resume id.
func (sw *sseWriter) frame(f *frame) {
	id := sw.epoch + ":" + strconv.FormatUint(f.cursor, 10)
	sw.write("id: " + id + "\ndata: ")
	sw.write(string(f.data))
	sw.write("\n\n")
	if sw.err == nil {
		sw.lastID = id
	}
}

// raw writes a journal frame. No id: line, so an EventSource keeps its
// previous Last-Event-ID — a drop mid-backfill re-runs the backfill rather
// than resuming into the middle of it.
func (sw *sseWriter) raw(data []byte) {
	sw.write("data: ")
	sw.write(string(data))
	sw.write("\n\n")
}

func (sw *sseWriter) control(event string, body map[string]any) {
	data, _ := json.Marshal(body)
	sw.write("event: " + event + "\ndata: " + string(data) + "\n\n")
}

func (sw *sseWriter) comment(text string) {
	sw.write(": " + text + "\n\n")
}

func (sw *sseWriter) flush() {
	if sw.err == nil {
		sw.f.Flush()
	}
}

// denyAuth maps a nexusauth denial onto a status code, matching the mapping
// nexus.io.agui and the broker use: kinds are the package's transport contract.
func denyAuth(w http.ResponseWriter, r *http.Request, err error, logger *slog.Logger) {
	kind := nexusauth.KindOf(err)
	status, message := http.StatusUnauthorized, "unauthorized"
	switch kind {
	case nexusauth.KindInsufficientScope:
		status, message = http.StatusForbidden, "insufficient scope"
	case nexusauth.KindUnavailable:
		status, message = http.StatusServiceUnavailable, "authentication temporarily unavailable"
	}
	logger.Warn("tap auth denied", "path", r.URL.Path, "status", status, "denial", err)
	switch status {
	case http.StatusUnauthorized:
		if kind == nexusauth.KindNoCredential {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-tap"`)
		} else {
			w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-tap", error="invalid_token"`)
		}
	case http.StatusForbidden:
		w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-tap", error="insufficient_scope"`)
	case http.StatusServiceUnavailable:
		w.Header().Set("Retry-After", "5")
	}
	http.Error(w, message, status)
}
