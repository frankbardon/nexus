package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// This file owns the broker's operator-visible instrumentation: the process-wide
// counters every subsystem reports into, and GET /metrics, which renders them in
// the Prometheus text exposition format.
//
// WHY IT IS HAND-ROLLED. The exposition format is a documented, stable, plain
// text protocol — a name, optional labels, a number and a newline — and the
// broker is deliberately stdlib plus github.com/coder/websocket. Pulling a client
// library in for what amounts to a few hundred bytes of fmt.Fprintf would be the
// single largest dependency in this binary, and the repo already holds the same
// line for LLM providers (raw net/http, no SDKs). So: no dependency.
//
// WHY THE CARDINALITY RULES ARE ABSOLUTE. Every label value in this file comes
// from a compile-time constant. Nothing is ever labelled by lease id, principal,
// session id or binary path: those are unbounded, operator-controlled or
// caller-controlled, and one of them on a counter turns a scrape into an
// unbounded time series set — a memory leak in the monitoring system rather than
// in the broker. The counter families below take their label values from the
// SAME slice they are constructed with, so a value that is not declared cannot be
// incremented and a declared one is always rendered (as 0 until it happens),
// which is what lets an alert say "spawn failures went from 0 to 1" instead of
// waiting for a series to appear.
//
// COUNTERS ARE MONOTONIC AND LIVE FOR THE PROCESS. Gauges are the opposite: they
// are read from live state at scrape time (see metricsSample), never accumulated,
// so they cannot drift away from what the registry actually holds.

// metricsContentType is the Prometheus text exposition format's media type. The
// version parameter is part of the contract — a scraper uses it to pick a parser
// — so it is spelled out rather than shortened to text/plain.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// metricNamespace prefixes every metric this broker exposes. It exists so a
// scrape target holding several Nexus components cannot collide, and so an
// operator can select the whole broker with one `nexus_broker_` matcher.
const metricNamespace = "nexus_broker_"

// Claim outcomes. They are the label values of nexus_broker_claims_total and the
// exhaustive classification of what POST /claim — and the A2A ingress, which
// boots instances through the same spine — did with a request.
//
// The set is deliberately finer than the HTTP status: three distinct 503s
// (capacity full with no waiting, waited and gave up, never allowed to wait) are
// three different operator problems with three different fixes, and collapsing
// them onto "503" is exactly the loss of information this metric exists to
// prevent.
const (
	// claimOutcomeAccepted is a claim that produced a live, ready instance.
	claimOutcomeAccepted = "accepted"

	// claimOutcomeRejected is a claim the REQUEST was at fault for: a malformed
	// body, an empty config, an unknown binary, or a resume naming a session
	// bound to a binary this broker no longer offers.
	claimOutcomeRejected = "rejected"

	// claimOutcomeNoCapacity is the cap being full with queue_wait_timeout <= 0.
	claimOutcomeNoCapacity = "no_capacity"

	// claimOutcomeQueueTimeout is a claim that parked in the FIFO capacity queue
	// and gave up after queue_wait_timeout.
	claimOutcomeQueueTimeout = "queue_timeout"

	// claimOutcomeQueueFull is a claim refused entry to the queue because it was
	// already holding max_queue_depth waiters.
	claimOutcomeQueueFull = "queue_full"

	// claimOutcomeLeaseLimit is a principal already holding
	// max_leases_per_principal live leases.
	claimOutcomeLeaseLimit = "principal_lease_limit"

	// claimOutcomeQueueLimit is a principal already holding
	// max_queued_per_principal parked claims.
	claimOutcomeQueueLimit = "principal_queue_limit"

	// claimOutcomeCancelled is a caller that hung up while queued. It is not a
	// broker failure and must not be alerted on as one.
	claimOutcomeCancelled = "cancelled"

	// claimOutcomeSpawnFailed is an instance that could not be started, or that
	// exited before signalling ready.
	claimOutcomeSpawnFailed = "spawn_failed"

	// claimOutcomeReadyTimeout is an instance that started but did not signal
	// ready within ready_timeout.
	claimOutcomeReadyTimeout = "ready_timeout"

	// claimOutcomeInternal is everything else — a temp config that could not be
	// written, a spawn secret that could not be minted. Kept as its own value
	// rather than folded into spawn_failed because it names a broker-side fault
	// with no instance involved.
	claimOutcomeInternal = "internal"
)

// claimOutcomes is the declared label set of nexus_broker_claims_total. The
// counter family is built from this slice, so every constant above renders from
// the first scrape and nothing else can be incremented.
var claimOutcomes = []string{
	claimOutcomeAccepted,
	claimOutcomeRejected,
	claimOutcomeNoCapacity,
	claimOutcomeQueueTimeout,
	claimOutcomeQueueFull,
	claimOutcomeLeaseLimit,
	claimOutcomeQueueLimit,
	claimOutcomeCancelled,
	claimOutcomeSpawnFailed,
	claimOutcomeReadyTimeout,
	claimOutcomeInternal,
}

// Spawn-failure reasons. They label nexus_broker_spawn_failures_total, which is
// deliberately NOT redundant with the claim outcomes above: it counts only the
// failures where a process was involved, which is the number an operator wants
// as a rate against deploys rather than against client behaviour.
const (
	// spawnFailureExec is the exec itself failing — a bad path, a run_as this
	// broker cannot set, a fork that could not happen.
	spawnFailureExec = "exec"

	// spawnFailureExitedEarly is a process that started and then died before it
	// signalled ready. This is the config-is-wrong shape.
	spawnFailureExitedEarly = "exited_before_ready"

	// spawnFailureReadyTimeout is a process still alive at ready_timeout that
	// never signalled. This is the too-slow / wedged shape.
	spawnFailureReadyTimeout = "ready_timeout"
)

var spawnFailureReasons = []string{
	spawnFailureExec,
	spawnFailureExitedEarly,
	spawnFailureReadyTimeout,
}

// Frame-drop reasons. They label nexus_broker_frames_dropped_total, the metric
// the replay work made worth alerting on: a client-bound frame that is dropped
// rather than retained is data the client will never see, and until now it
// existed only as a WARN line.
const (
	// frameDropUndecodable is a frame that did not parse as a broker envelope.
	frameDropUndecodable = "undecodable"

	// frameDropLeaseMismatch is a frame naming a different lease than the socket
	// it arrived on is bound to.
	frameDropLeaseMismatch = "lease_mismatch"

	// frameDropNoInstance is a client-originated frame with no instance socket
	// to relay it to.
	frameDropNoInstance = "no_instance"

	// frameDropInstanceBufferFull is a client-originated frame the instance's
	// send queue could not accept.
	frameDropInstanceBufferFull = "instance_buffer_full"

	// frameDropClientBufferFull is an instance-originated frame the client's
	// send queue could not accept. It is retained for replay, but this scrape
	// still records that the live path lost it.
	frameDropClientBufferFull = "client_buffer_full"

	// frameDropLeaseGone is an instance-originated frame for a lease that was
	// removed between the read and the send.
	frameDropLeaseGone = "lease_gone"
)

var frameDropReasons = []string{
	frameDropUndecodable,
	frameDropLeaseMismatch,
	frameDropNoInstance,
	frameDropInstanceBufferFull,
	frameDropClientBufferFull,
	frameDropLeaseGone,
}

// replayGapReasons are the wire values of a stream-gap frame (see clientstream.go).
// They are reused verbatim as label values so the metric and the frame a client
// receives cannot disagree about why a gap happened.
var replayGapReasons = []string{gapReasonEvicted, gapReasonRestarted}

// Config-reload outcomes.
const (
	// reloadOutcomeApplied is a SIGHUP that published a new snapshot.
	reloadOutcomeApplied = "applied"

	// reloadOutcomeRejected is a SIGHUP whose file could not be loaded or whose
	// agent cards could not be rendered. The configuration in force is unchanged.
	reloadOutcomeRejected = "rejected"
)

var reloadOutcomes = []string{reloadOutcomeApplied, reloadOutcomeRejected}

// Restored-lease outcomes: what happened to the leases restart recovery adopted
// from the journal.
const (
	// restoredOutcomeRestored counts leases adopted from the journal at boot. It
	// is the denominator the two below are read against.
	restoredOutcomeRestored = "restored"

	// restoredOutcomeReattached counts restored leases whose instance dialed
	// back within reattach_window — the success this whole recovery path exists
	// for.
	restoredOutcomeReattached = "reattached"

	// restoredOutcomeReaped counts restored leases torn down because nothing
	// reattached in time.
	restoredOutcomeReaped = "reaped"
)

var restoredOutcomes = []string{
	restoredOutcomeRestored,
	restoredOutcomeReattached,
	restoredOutcomeReaped,
}

// claimDurationBuckets are the cumulative upper bounds (in seconds) of
// nexus_broker_claim_duration_seconds.
//
// They are chosen for what a claim actually costs: an instance boot is measured
// in seconds, not milliseconds, and the interesting question is how close a
// deployment sits to its ready_timeout (30s by default) — so the buckets run out
// past it rather than crowding the sub-second end. +Inf is rendered from the
// count and is not listed here.
var claimDurationBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// counterFamily is one metric name with a FIXED, compile-time set of label
// values — the only kind of labelled counter this file has.
//
// Building the family from the same slice the increment constants live in is
// what makes the cardinality bound structural: inc takes a value it can only
// have got from that slice, an undeclared value increments nothing, and every
// declared value is rendered from the first scrape (as 0 until something happens)
// so an alert can be written against a series that is already there.
type counterFamily struct {
	name   string
	help   string
	label  string
	values []string
	counts []atomic.Uint64
}

// newCounterFamily declares a labelled counter. name is namespaced and suffixed
// by the caller so the declaration reads as the metric an operator will see.
func newCounterFamily(name, help, label string, values []string) *counterFamily {
	return &counterFamily{
		name:   name,
		help:   help,
		label:  label,
		values: values,
		counts: make([]atomic.Uint64, len(values)),
	}
}

// inc adds one to the series named by value. An undeclared value is a no-op
// rather than a panic or a new series: a metric must never be able to take a
// process down, and it must never be able to grow a label set at runtime.
func (c *counterFamily) inc(value string) {
	for i := range c.values {
		if c.values[i] == value {
			c.counts[i].Add(1)
			return
		}
	}
}

// add adds n to the series named by value. Used where a batch is counted at once
// (restored leases reattaching or being reaped).
func (c *counterFamily) add(value string, n uint64) {
	if n == 0 {
		return
	}
	for i := range c.values {
		if c.values[i] == value {
			c.counts[i].Add(n)
			return
		}
	}
}

// render writes the family as one HELP line, one TYPE line and one sample per
// declared label value.
func (c *counterFamily) render(b *strings.Builder) {
	writeMetricHeader(b, c.name, "counter", c.help)
	for i := range c.values {
		fmt.Fprintf(b, "%s{%s=\"%s\"} %d\n", c.name, c.label, c.values[i], c.counts[i].Load())
	}
}

// counter is one unlabelled monotonic counter.
type counter struct {
	name  string
	help  string
	value atomic.Uint64
}

func newCounter(name, help string) *counter { return &counter{name: name, help: help} }

func (c *counter) inc() { c.value.Add(1) }

func (c *counter) render(b *strings.Builder) {
	writeMetricHeader(b, c.name, "counter", c.help)
	fmt.Fprintf(b, "%s %d\n", c.name, c.value.Load())
}

// histogram is a fixed-bucket cumulative histogram rendered as the exposition
// format requires: one `_bucket{le="..."}` series per bound plus `le="+Inf"`,
// then `_sum` and `_count`.
//
// The buckets are cumulative on the WIRE but counted per-bucket in memory and
// summed at render time, which keeps the observe path to one atomic add rather
// than one per bound above the observation.
type histogram struct {
	name    string
	help    string
	bounds  []float64
	buckets []atomic.Uint64 // one per bound, plus a trailing overflow bucket
	count   atomic.Uint64
	sumMill atomic.Uint64 // sum in milliseconds, so the total stays integral
}

func newHistogram(name, help string, bounds []float64) *histogram {
	return &histogram{
		name:    name,
		help:    help,
		bounds:  bounds,
		buckets: make([]atomic.Uint64, len(bounds)+1),
	}
}

// observe records one duration. The sum is accumulated in whole milliseconds
// rather than as a float so the counter is exact under concurrency — an atomic
// add on a uint64 is; a read-modify-write on a float would not be.
func (h *histogram) observe(d time.Duration) {
	if d < 0 {
		d = 0
	}
	seconds := d.Seconds()
	idx := len(h.bounds)
	for i, bound := range h.bounds {
		if seconds <= bound {
			idx = i
			break
		}
	}
	h.buckets[idx].Add(1)
	h.count.Add(1)
	h.sumMill.Add(uint64(d.Milliseconds()))
}

func (h *histogram) render(b *strings.Builder) {
	writeMetricHeader(b, h.name, "histogram", h.help)
	var cumulative uint64
	for i, bound := range h.bounds {
		cumulative += h.buckets[i].Load()
		fmt.Fprintf(b, "%s_bucket{le=\"%s\"} %d\n", h.name, formatFloat(bound), cumulative)
	}
	cumulative += h.buckets[len(h.bounds)].Load()
	fmt.Fprintf(b, "%s_bucket{le=\"+Inf\"} %d\n", h.name, cumulative)
	fmt.Fprintf(b, "%s_sum %s\n", h.name, formatFloat(float64(h.sumMill.Load())/1000))
	fmt.Fprintf(b, "%s_count %d\n", h.name, h.count.Load())
}

// brokerMetrics is the process-wide counter set. It is created once in run() and
// wired onto the registry, which every reporting subsystem already holds.
//
// EVERY METHOD IS NIL-RECEIVER SAFE. A nil *brokerMetrics is a supported state,
// not an oversight: the dozens of existing tests that build a Registry, a
// Gateway or a ClaimServer directly wire no metrics, and none of them should have
// to name a dependency they do not exercise. Instrumentation that could nil-panic
// a request path would be a worse bug than the blindness it cures.
type brokerMetrics struct {
	claims        *counterFamily
	spawnFailures *counterFamily
	framesDropped *counterFamily
	replayGaps    *counterFamily
	reloads       *counterFamily
	restored      *counterFamily
	evictions     *counter
	claimDuration *histogram
}

// newBrokerMetrics declares every metric the broker exposes. The declarations
// live in ONE function on purpose: the metric names are a stable operator
// surface (documented in the session-broker guide), and a name invented at a call
// site is a name nobody reviews.
func newBrokerMetrics() *brokerMetrics {
	return &brokerMetrics{
		claims: newCounterFamily(metricNamespace+"claims_total",
			"Instance claims handled by the shared spawn spine, by outcome. Covers POST /claim and the A2A ingress.",
			"outcome", claimOutcomes),
		spawnFailures: newCounterFamily(metricNamespace+"spawn_failures_total",
			"Instance spawns that produced no ready instance, by reason.",
			"reason", spawnFailureReasons),
		framesDropped: newCounterFamily(metricNamespace+"frames_dropped_total",
			"Broker frames discarded rather than relayed, by reason.",
			"reason", frameDropReasons),
		replayGaps: newCounterFamily(metricNamespace+"replay_gaps_total",
			"Stream-gap notices served to a resuming client, by reason.",
			"reason", replayGapReasons),
		reloads: newCounterFamily(metricNamespace+"config_reloads_total",
			"SIGHUP configuration reloads, by outcome.",
			"outcome", reloadOutcomes),
		restored: newCounterFamily(metricNamespace+"restored_leases_total",
			"Leases adopted from the journal at boot, and what became of them.",
			"outcome", restoredOutcomes),
		evictions: newCounter(metricNamespace+"client_evictions_total",
			"Client WebSockets displaced by a newer connection on the same lease."),
		claimDuration: newHistogram(metricNamespace+"claim_duration_seconds",
			"Wall time of an ACCEPTED claim, from request to a ready instance.",
			claimDurationBuckets),
	}
}

// claimOutcome records how one claim ended.
func (m *brokerMetrics) claimOutcome(outcome string) {
	if m == nil {
		return
	}
	m.claims.inc(outcome)
}

// claimAccepted records an accepted claim and how long it took. Duration is
// observed only here — a refused claim's latency measures the refusal, not the
// broker's spawn cost, and mixing the two makes the quantiles meaningless.
func (m *brokerMetrics) claimAccepted(d time.Duration) {
	if m == nil {
		return
	}
	m.claims.inc(claimOutcomeAccepted)
	m.claimDuration.observe(d)
}

// spawnFailed records a spawn that produced no ready instance.
func (m *brokerMetrics) spawnFailed(reason string) {
	if m == nil {
		return
	}
	m.spawnFailures.inc(reason)
}

// frameDropped records one discarded frame.
func (m *brokerMetrics) frameDropped(reason string) {
	if m == nil {
		return
	}
	m.framesDropped.inc(reason)
}

// replayGap records a stream-gap notice served to a resuming client.
func (m *brokerMetrics) replayGap(reason string) {
	if m == nil {
		return
	}
	m.replayGaps.inc(reason)
}

// clientEvicted records a client socket displaced by a newer connection.
func (m *brokerMetrics) clientEvicted() {
	if m == nil {
		return
	}
	m.evictions.inc()
}

// reloadOutcome records the result of a SIGHUP.
func (m *brokerMetrics) reloadOutcome(outcome string) {
	if m == nil {
		return
	}
	m.reloads.inc(outcome)
}

// leasesRestored records how many leases restart recovery adopted.
func (m *brokerMetrics) leasesRestored(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.restored.add(restoredOutcomeRestored, uint64(n))
}

// restoredSettled records how the reattach window ended for the restored leases:
// how many had an instance dial back, and how many were reaped.
func (m *brokerMetrics) restoredSettled(reattached, reaped int) {
	if m == nil {
		return
	}
	if reattached > 0 {
		m.restored.add(restoredOutcomeReattached, uint64(reattached))
	}
	if reaped > 0 {
		m.restored.add(restoredOutcomeReaped, uint64(reaped))
	}
}

// metricsSample is the point-in-time gauge half of a scrape, read from live
// state rather than accumulated. Taken under a single hold of Registry.mu so the
// numbers agree with each other: a scrape that reported slots_in_use from one
// instant and queue_depth from another would show impossible states under load.
type metricsSample struct {
	maxConcurrent int
	slotsInUse    int
	queueDepth    int
	maxQueueDepth int
	spawning      int
	active        int
	draining      int
}

// metricsSample takes the gauge sample. It reuses the SAME fields the capacity
// accounting and GET /leases already read — r.slotsInUse, r.waiters, and
// lease.surfaceState — so there is no second tally for a metric to drift from.
func (r *Registry) metricsSample() metricsSample {
	if r == nil {
		return metricsSample{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := metricsSample{
		maxConcurrent: r.maxConcurrent,
		slotsInUse:    r.slotsInUse,
		queueDepth:    r.waiters.Len(),
		maxQueueDepth: r.maxQueueDepth,
	}
	for _, l := range r.leases {
		switch l.surfaceState() {
		case surfaceStateSpawning:
			s.spawning++
		case surfaceStateDraining:
			s.draining++
		default:
			s.active++
		}
	}
	return s
}

// MetricsServer handles GET /metrics: the Prometheus scrape surface.
//
// It performs no mutation whatsoever. It reads the process counters and one
// registry sample, renders them, and writes them.
type MetricsServer struct {
	logger   *slog.Logger
	registry *Registry
	metrics  *brokerMetrics
	tickets  *ticketStore

	// guard is consulted for ONE thing — whether authentication is configured at
	// all — exactly as LeasesServer consults it. With no `auth:` block there is no
	// identity to scope by and every other route serves anyone, so this one does
	// too rather than inventing a second, stricter posture for a broker the
	// operator has already chosen to run open.
	guard *authGuard

	// adminScope is `auth.admin_scope`. When authentication IS configured, a
	// caller must hold it: the whole point of this endpoint is broker-wide
	// aggregates, which is the same disclosure GET /leases reserves for an
	// operator. An empty adminScope therefore refuses everyone while auth is on —
	// the safe reading of "the operator configured no admin scope", and the same
	// one the leases listing takes.
	adminScope string
}

// NewMetricsServer constructs the scrape handler. guard decides whether the
// admin-scope check engages at all, and adminScope names the scope it requires.
func NewMetricsServer(logger *slog.Logger, registry *Registry, metrics *brokerMetrics, tickets *ticketStore, guard *authGuard, adminScope string) *MetricsServer {
	if logger == nil {
		logger = slog.Default()
	}
	return &MetricsServer{
		logger:     logger,
		registry:   registry,
		metrics:    metrics,
		tickets:    tickets,
		guard:      guard,
		adminScope: adminScope,
	}
}

// Register wires GET /metrics onto a mux. main registers it through the SAME
// guard as POST /claim, so a caller with no credential is refused by exactly the
// middleware that refuses a claim; the admin-scope check below is layered on top
// of that, not instead of it.
func (s *MetricsServer) Register(mux routeMux) {
	mux.HandleFunc("GET /metrics", s.handleMetrics)
}

// handleMetrics authorizes the caller and writes the exposition.
func (s *MetricsServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		// The same status, message and challenge the auth guard writes for an
		// insufficient scope, so a scraper cannot tell "this broker refuses the
		// route" apart from "your token is missing a scope" — there is one
		// vocabulary for that refusal on this binary.
		w.Header().Set("WWW-Authenticate", `Bearer realm="nexus-broker", error="insufficient_scope"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"insufficient scope"}` + "\n"))
		return
	}

	body := s.render()
	w.Header().Set("Content-Type", metricsContentType)
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(body)); err != nil {
		s.logger.Warn("writing the metrics exposition failed", "error", err)
	}
}

// authorized reports whether r's caller may scrape.
//
// The auth-disabled branch comes FIRST and mirrors GET /leases: a broker with no
// `auth:` block serves every route to anyone, and a metrics endpoint that alone
// refused would be a surprise rather than a protection — the same caller can
// already read the whole registry from GET /leases.
func (s *MetricsServer) authorized(r *http.Request) bool {
	if !s.guard.enabled() {
		return true
	}
	if s.adminScope == "" {
		return false
	}
	return callerPrincipal(r).HasScope(s.adminScope)
}

// render assembles the whole exposition. Order is stable — declaration order,
// counters then gauges — so a diff of two scrapes is readable by a human.
func (s *MetricsServer) render() string {
	var b strings.Builder

	if s.metrics != nil {
		s.metrics.claims.render(&b)
		s.metrics.claimDuration.render(&b)
		s.metrics.spawnFailures.render(&b)
		s.metrics.framesDropped.render(&b)
		s.metrics.replayGaps.render(&b)
		s.metrics.evictions.render(&b)
		s.metrics.reloads.render(&b)
		s.metrics.restored.render(&b)
	}

	sample := s.registry.metricsSample()

	// max_concurrent and max_queue_depth are exposed BESIDE the numbers they
	// bound, not instead of them. An operator watching queue_depth climb cannot
	// tell a busy broker from one about to start refusing claims without the
	// ceiling in the same scrape — which is the gap E4-S4 left behind in GET
	// /leases and this closes on both surfaces.
	writeGauge(&b, metricNamespace+"slots_in_use",
		"Capacity slots currently held: one per live lease.", sample.slotsInUse)
	writeGauge(&b, metricNamespace+"max_concurrent",
		"Configured max_concurrent ceiling. 0 means unlimited.", sample.maxConcurrent)
	writeGauge(&b, metricNamespace+"queue_depth",
		"Claims currently parked in the FIFO capacity queue.", sample.queueDepth)
	writeGauge(&b, metricNamespace+"max_queue_depth",
		"Configured max_queue_depth ceiling on parked claims. 0 means unlimited.", sample.maxQueueDepth)

	writeMetricHeader(&b, metricNamespace+"leases", "gauge",
		"Live leases by operator-facing state.")
	fmt.Fprintf(&b, "%sleases{state=\"%s\"} %d\n", metricNamespace, surfaceStateSpawning, sample.spawning)
	fmt.Fprintf(&b, "%sleases{state=\"%s\"} %d\n", metricNamespace, surfaceStateActive, sample.active)
	fmt.Fprintf(&b, "%sleases{state=\"%s\"} %d\n", metricNamespace, surfaceStateDraining, sample.draining)

	writeGauge(&b, metricNamespace+"tickets_outstanding",
		"Issued, unredeemed WebSocket tickets the broker still holds.", s.tickets.outstanding())

	return b.String()
}

// writeMetricHeader writes the HELP and TYPE lines for one metric name.
//
// The help text is escaped per the exposition format: a backslash and a newline
// are the only two sequences that can break a HELP line, and escaping them here
// means no future help string can corrupt a scrape.
func writeMetricHeader(b *strings.Builder, name, kind, help string) {
	b.WriteString("# HELP ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(escapeHelp(help))
	b.WriteByte('\n')
	b.WriteString("# TYPE ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(kind)
	b.WriteByte('\n')
}

// writeGauge writes a complete unlabelled gauge: header plus one sample.
func writeGauge(b *strings.Builder, name, help string, value int) {
	writeMetricHeader(b, name, "gauge", help)
	fmt.Fprintf(b, "%s %d\n", name, value)
}

// escapeHelp escapes the two sequences the exposition format reserves in a HELP
// line.
func escapeHelp(help string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(help)
}

// formatFloat renders a bucket bound or a sum the way the exposition format
// wants it: shortest round-trippable decimal, never scientific notation for the
// values this file produces.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
