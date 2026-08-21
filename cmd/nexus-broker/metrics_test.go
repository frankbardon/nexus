package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// newMetricsTestServer wires GET /metrics over an httptest server with the same
// topology run() builds: the route is registered THROUGH the guard, and the
// handler is given the same guard plus the loaded admin scope. authYAML empty
// means no `auth:` block at all.
//
// It returns the server, the shared registry (so a test can seed leases), the
// counter set (so a test can drive a counter without a live spawn) and the
// ticket store.
func newMetricsTestServer(t *testing.T, maxConcurrent int, authYAML string) (*httptest.Server, *Registry, *brokerMetrics, *ticketStore) {
	t.Helper()
	cfg := mustLoadConfig(t, authYAML)
	if authYAML != "" && !cfg.AuthChain.Enabled() {
		t.Fatalf("newMetricsTestServer: YAML produced a DISABLED chain; the test would prove nothing:\n%s", authYAML)
	}
	logger := testLogger()
	metrics := newBrokerMetrics()
	reg := NewRegistry(logger, maxConcurrent)
	reg.useMetrics(metrics)
	guard := newAuthGuard(logger, cfg.AuthChain)
	tickets := newTicketStore(logger, guard.enabled())
	reg.useTicketStore(tickets)
	ms := NewMetricsServer(logger, reg, metrics, tickets, guard, cfg.AdminScope)
	mux := http.NewServeMux()
	ms.Register(guard.Guard(mux))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, reg, metrics, tickets
}

// markLeaseDraining latches a lease's teardown flag directly, so the state
// breakdown can be exercised without running a real release (which would also
// remove the lease and leave nothing to count).
func markLeaseDraining(t *testing.T, reg *Registry, id string) {
	t.Helper()
	reg.mu.Lock()
	defer reg.mu.Unlock()
	l, ok := reg.leases[id]
	if !ok {
		t.Fatalf("markLeaseDraining: lease %s is not in the registry", id)
	}
	l.releasing = true
}

// scrape performs GET /metrics with an optional bearer token and returns the
// status, the Content-Type and the body.
func scrape(t *testing.T, base, token string) (int, string, string) {
	t.Helper()
	resp := doAuthed(t, http.MethodGet, base+"/metrics", token, "")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics body: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

// mustScrape scrapes and fails on any non-200.
func mustScrape(t *testing.T, base, token string) string {
	t.Helper()
	status, ctype, body := scrape(t, base, token)
	if status != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200; body=%s", status, body)
	}
	if ctype != metricsContentType {
		t.Fatalf("GET /metrics Content-Type = %q, want %q", ctype, metricsContentType)
	}
	return body
}

// ---------------------------------------------------------------------------
// A parser for the Prometheus text exposition format
// ---------------------------------------------------------------------------

// promSample is one parsed sample line: the metric name, its label set rendered
// back to a canonical string, and the value.
type promSample struct {
	name   string
	labels string
	value  float64
}

// promExposition is a parsed scrape: the declared HELP and TYPE per metric name,
// and every sample.
type promExposition struct {
	help    map[string]string
	types   map[string]string
	samples []promSample
}

// series returns the value of one series, and whether it was present.
func (e promExposition) series(name, labels string) (float64, bool) {
	for _, s := range e.samples {
		if s.name == name && s.labels == labels {
			return s.value, true
		}
	}
	return 0, false
}

// mustSeries returns the value of one series, failing if it is absent. The
// absence is worth failing on rather than reading as zero: a declared label value
// that stops being rendered is exactly the regression that silently breaks an
// operator's alert.
func (e promExposition) mustSeries(t *testing.T, name, labels string) float64 {
	t.Helper()
	v, ok := e.series(name, labels)
	if !ok {
		t.Fatalf("series %s%s is absent from the exposition", name, labels)
	}
	return v
}

// sampleLine matches a sample: a metric name, an optional label set, whitespace
// and a value. It is deliberately strict — anchored, with an explicit label
// grammar — so a malformed line fails the parse rather than being skipped.
var sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})? ([^ ]+)$`)

// labelPair matches one name="value" pair inside a label set.
var labelPair = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)="([^"]*)"$`)

// parsePrometheusText parses an exposition and fails the test on anything that
// is not valid text format.
//
// This is the assertion the story asks for: a handler that returned a 200 and a
// blob of JSON would satisfy "the endpoint answers", so the test parses the
// grammar instead — every metric carries a HELP and a TYPE line before its
// samples, every sample line matches the sample grammar, every label set is
// well-formed, every value is a number, and no series is emitted twice.
func parsePrometheusText(t *testing.T, body string) promExposition {
	t.Helper()
	exp := promExposition{help: map[string]string{}, types: map[string]string{}}
	seen := map[string]bool{}

	for i, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		lineNo := i + 1
		if line == "" {
			t.Fatalf("line %d: blank line in the exposition", lineNo)
		}
		if strings.HasPrefix(line, "#") {
			fields := strings.SplitN(line, " ", 4)
			if len(fields) < 3 {
				t.Fatalf("line %d: malformed comment line %q", lineNo, line)
			}
			switch fields[1] {
			case "HELP":
				name, help := fields[2], ""
				if len(fields) == 4 {
					help = fields[3]
				}
				if help == "" {
					t.Errorf("line %d: metric %s declares an empty HELP", lineNo, name)
				}
				if _, dup := exp.help[name]; dup {
					t.Errorf("line %d: metric %s declares HELP twice", lineNo, name)
				}
				exp.help[name] = help
			case "TYPE":
				if len(fields) != 4 {
					t.Fatalf("line %d: malformed TYPE line %q", lineNo, line)
				}
				name, kind := fields[2], fields[3]
				switch kind {
				case "counter", "gauge", "histogram", "summary", "untyped":
				default:
					t.Errorf("line %d: metric %s declares unknown type %q", lineNo, name, kind)
				}
				if _, dup := exp.types[name]; dup {
					t.Errorf("line %d: metric %s declares TYPE twice", lineNo, name)
				}
				exp.types[name] = kind
			default:
				t.Fatalf("line %d: unknown comment directive %q", lineNo, fields[1])
			}
			continue
		}

		m := sampleLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("line %d: %q does not parse as a Prometheus sample", lineNo, line)
		}
		name, rawLabels, rawValue := m[1], m[2], m[3]

		value, err := strconv.ParseFloat(rawValue, 64)
		if err != nil {
			t.Fatalf("line %d: value %q of %s is not a number: %v", lineNo, rawValue, name, err)
		}

		labels := canonicalLabels(t, lineNo, rawLabels)

		// A sample must belong to a metric that already declared its TYPE. The
		// histogram suffixes are the documented exception: _bucket, _sum and
		// _count all belong to the base name's single TYPE line.
		base := name
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if trimmed, ok := strings.CutSuffix(name, suffix); ok && exp.types[trimmed] == "histogram" {
				base = trimmed
				break
			}
		}
		if _, declared := exp.types[base]; !declared {
			t.Errorf("line %d: sample %s has no preceding # TYPE line", lineNo, name)
		}
		if _, declared := exp.help[base]; !declared {
			t.Errorf("line %d: sample %s has no preceding # HELP line", lineNo, name)
		}

		key := name + labels
		if seen[key] {
			t.Errorf("line %d: series %s is emitted twice", lineNo, key)
		}
		seen[key] = true

		exp.samples = append(exp.samples, promSample{name: name, labels: labels, value: value})
	}

	if len(exp.samples) == 0 {
		t.Fatal("the exposition carries no samples at all, so every assertion below would be vacuous")
	}
	return exp
}

// canonicalLabels validates a raw `{a="1",b="2"}` label set and re-renders it in
// sorted order, so a test can name a series without depending on the emission
// order of its labels.
func canonicalLabels(t *testing.T, lineNo int, raw string) string {
	t.Helper()
	if raw == "" {
		return ""
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(raw, "{"), "}")
	if inner == "" {
		t.Fatalf("line %d: empty label set", lineNo)
	}
	parts := strings.Split(inner, ",")
	pairs := make([]string, 0, len(parts))
	for _, part := range parts {
		if !labelPair.MatchString(part) {
			t.Fatalf("line %d: malformed label pair %q", lineNo, part)
		}
		pairs = append(pairs, part)
	}
	sort.Strings(pairs)
	return "{" + strings.Join(pairs, ",") + "}"
}

// ---------------------------------------------------------------------------
// Format
// ---------------------------------------------------------------------------

// TestMetricsHTTP_ServesValidPrometheusText is the format contract: the endpoint
// answers text format, not merely 200.
//
// Every metric is required to be namespaced too, because the names are the
// operator surface this story publishes — an unprefixed one would collide with
// whatever else a deployment scrapes on the same target.
func TestMetricsHTTP_ServesValidPrometheusText(t *testing.T) {
	ts, _, _, _ := newMetricsTestServer(t, 4, "")

	exp := parsePrometheusText(t, mustScrape(t, ts.URL, ""))

	if len(exp.types) == 0 {
		t.Fatal("the exposition declares no metric types at all")
	}
	for name := range exp.types {
		if !strings.HasPrefix(name, metricNamespace) {
			t.Errorf("metric %s is not namespaced %s", name, metricNamespace)
		}
		if _, ok := exp.help[name]; !ok {
			t.Errorf("metric %s declares a TYPE but no HELP", name)
		}
	}
	for name := range exp.help {
		if _, ok := exp.types[name]; !ok {
			t.Errorf("metric %s declares a HELP but no TYPE", name)
		}
	}

	// The metric names this story publishes as a stable surface. If one is
	// renamed, an operator's dashboard breaks silently — so the rename has to
	// break this test first.
	for _, want := range []string{
		metricNamespace + "claims_total",
		metricNamespace + "claim_duration_seconds",
		metricNamespace + "spawn_failures_total",
		metricNamespace + "frames_dropped_total",
		metricNamespace + "replay_gaps_total",
		metricNamespace + "client_evictions_total",
		metricNamespace + "config_reloads_total",
		metricNamespace + "restored_leases_total",
		metricNamespace + "slots_in_use",
		metricNamespace + "max_concurrent",
		metricNamespace + "queue_depth",
		metricNamespace + "max_queue_depth",
		metricNamespace + "leases",
		metricNamespace + "tickets_outstanding",
	} {
		if _, ok := exp.types[want]; !ok {
			t.Errorf("the exposition is missing the documented metric %s", want)
		}
	}
}

// TestMetricsHTTP_HistogramIsWellFormed pins the one composite metric: a
// histogram must carry a cumulative bucket per bound, a +Inf bucket equal to the
// count, a _sum and a _count.
func TestMetricsHTTP_HistogramIsWellFormed(t *testing.T) {
	ts, _, metrics, _ := newMetricsTestServer(t, 4, "")

	metrics.claimAccepted(300 * time.Millisecond)
	metrics.claimAccepted(2 * time.Second)
	metrics.claimAccepted(90 * time.Second) // past every bound: +Inf only

	exp := parsePrometheusText(t, mustScrape(t, ts.URL, ""))
	name := metricNamespace + "claim_duration_seconds"

	if got := exp.types[name]; got != "histogram" {
		t.Fatalf("%s type = %q, want histogram", name, got)
	}
	if got := exp.mustSeries(t, name+"_count", ""); got != 3 {
		t.Errorf("%s_count = %v, want 3", name, got)
	}
	if got := exp.mustSeries(t, name+"_sum", ""); got < 92.2 || got > 92.4 {
		t.Errorf("%s_sum = %v, want ~92.3", name, got)
	}
	if got := exp.mustSeries(t, name+`_bucket`, `{le="+Inf"}`); got != 3 {
		t.Errorf("%s_bucket{le=\"+Inf\"} = %v, want 3 (must equal _count)", name, got)
	}

	// Buckets are CUMULATIVE and therefore monotonically non-decreasing.
	var prev float64
	for _, bound := range claimDurationBuckets {
		label := fmt.Sprintf(`{le="%s"}`, formatFloat(bound))
		got := exp.mustSeries(t, name+"_bucket", label)
		if got < prev {
			t.Errorf("%s_bucket%s = %v, which is below the previous bucket %v — buckets must be cumulative",
				name, label, got, prev)
		}
		prev = got
	}
	// 0.3s falls in le="0.5"; 2s in le="2.5"; 90s in neither.
	if got := exp.mustSeries(t, name+"_bucket", `{le="0.5"}`); got != 1 {
		t.Errorf("%s_bucket{le=\"0.5\"} = %v, want 1", name, got)
	}
	if got := exp.mustSeries(t, name+"_bucket", `{le="2.5"}`); got != 2 {
		t.Errorf("%s_bucket{le=\"2.5\"} = %v, want 2", name, got)
	}
	if got := exp.mustSeries(t, name+"_bucket", `{le="60"}`); got != 2 {
		t.Errorf("%s_bucket{le=\"60\"} = %v, want 2 — the 90s observation belongs to +Inf only", name, got)
	}
}

// TestMetricsHTTP_DeclaredLabelValuesRenderFromTheFirstScrape proves the
// zero-series property: every declared outcome/reason is present at 0 before
// anything has happened.
//
// It is what lets an alert be written against a series that already exists,
// rather than one that only appears the first time the broker fails.
func TestMetricsHTTP_DeclaredLabelValuesRenderFromTheFirstScrape(t *testing.T) {
	ts, _, _, _ := newMetricsTestServer(t, 4, "")
	exp := parsePrometheusText(t, mustScrape(t, ts.URL, ""))

	families := []struct {
		name   string
		label  string
		values []string
	}{
		{metricNamespace + "claims_total", "outcome", claimOutcomes},
		{metricNamespace + "spawn_failures_total", "reason", spawnFailureReasons},
		{metricNamespace + "frames_dropped_total", "reason", frameDropReasons},
		{metricNamespace + "replay_gaps_total", "reason", replayGapReasons},
		{metricNamespace + "config_reloads_total", "outcome", reloadOutcomes},
		{metricNamespace + "restored_leases_total", "outcome", restoredOutcomes},
	}
	for _, f := range families {
		for _, v := range f.values {
			label := fmt.Sprintf(`{%s="%s"}`, f.label, v)
			if got := exp.mustSeries(t, f.name, label); got != 0 {
				t.Errorf("%s%s = %v on a fresh broker, want 0", f.name, label, got)
			}
		}
	}
}

// TestMetricsHTTP_CardinalityIsBounded is the guard against the failure mode a
// metrics endpoint most easily introduces: a label carrying a lease id, a
// principal or a session id turns one metric into an unbounded series set.
//
// It seeds real leases owned by real principals and asserts that neither value
// appears anywhere in the exposition.
func TestMetricsHTTP_CardinalityIsBounded(t *testing.T) {
	ts, reg, _, _ := newMetricsTestServer(t, 8, "")
	ids, _, _ := seedTwoTenantLeases(t, reg)
	reg.MarkSessionID(ids[0], "session-should-not-be-a-label")

	body := mustScrape(t, ts.URL, "")
	for _, forbidden := range append([]string{ownerPrincipal, otherPrincipal, "session-should-not-be-a-label"}, ids...) {
		if strings.Contains(body, forbidden) {
			t.Errorf("the exposition contains %q, which is an unbounded label value:\n%s", forbidden, body)
		}
	}

	// Not vacuous: the leases really are there, counted in the bounded gauge.
	exp := parsePrometheusText(t, body)
	if got := exp.mustSeries(t, metricNamespace+"slots_in_use", ""); got != 4 {
		t.Fatalf("slots_in_use = %v, want 4 — the assertions above would be vacuous", got)
	}
}

// ---------------------------------------------------------------------------
// Authorization
// ---------------------------------------------------------------------------

// TestMetricsHTTP_AuthDisabledServesAnyone is the backward-compatibility posture:
// a broker with no `auth:` block serves this route to an unauthenticated caller,
// exactly as it serves every other route.
func TestMetricsHTTP_AuthDisabledServesAnyone(t *testing.T) {
	ts, _, _, _ := newMetricsTestServer(t, 4, "")

	status, ctype, body := scrape(t, ts.URL, "")
	if status != http.StatusOK {
		t.Fatalf("unauthenticated scrape of an open broker = %d, want 200; body=%s", status, body)
	}
	if ctype != metricsContentType {
		t.Errorf("Content-Type = %q, want %q", ctype, metricsContentType)
	}
	parsePrometheusText(t, body)
}

// TestMetricsHTTP_AuthenticatedNonAdminIsRefused is the operator-only rule: a
// caller whose credential is perfectly valid, but which does not carry
// `auth.admin_scope`, is refused — because the whole endpoint is whole-registry
// aggregates, the same disclosure GET /leases reserves for an operator.
func TestMetricsHTTP_AuthenticatedNonAdminIsRefused(t *testing.T) {
	ts, reg, _, tickets := newMetricsTestServer(t, 8, scopedLeasesAuthYAML)
	ids, _, _ := seedTwoTenantLeases(t, reg)
	if _, err := tickets.mint(ids[0], ownerPrincipal); err != nil {
		t.Fatalf("mint ticket: %v", err)
	}

	status, _, body := scrape(t, ts.URL, ownerToken)
	if status != http.StatusForbidden {
		t.Fatalf("non-admin scrape = %d, want 403; body=%s", status, body)
	}
	if got, want := strings.TrimSpace(body), `{"error":"insufficient scope"}`; got != want {
		t.Errorf("non-admin body = %s, want %s", got, want)
	}
	// The refusal must disclose nothing: no counter, no gauge, no lease state.
	if strings.Contains(body, metricNamespace) {
		t.Errorf("the refusal leaked metric data: %s", body)
	}

	// The very same server serves an operator, so the refusal is authorization
	// and not a broken handler.
	exp := parsePrometheusText(t, mustScrape(t, ts.URL, adminToken))
	if got := exp.mustSeries(t, metricNamespace+"slots_in_use", ""); got != 4 {
		t.Fatalf("operator slots_in_use = %v, want 4 — the refusal above would be vacuous", got)
	}
	// The ticket store is live on an authenticated broker, so this gauge is
	// exercised with a real issued credential rather than an inert one.
	if got := exp.mustSeries(t, metricNamespace+"tickets_outstanding", ""); got != 1 {
		t.Errorf("tickets_outstanding = %v, want 1", got)
	}
}

// TestMetricsHTTP_NoCredentialIsRefusedByTheGuard proves the route sits behind
// the SAME guard as POST /claim: a caller presenting nothing gets the guard's
// 401, not the handler's 403.
func TestMetricsHTTP_NoCredentialIsRefusedByTheGuard(t *testing.T) {
	ts, _, _, _ := newMetricsTestServer(t, 4, scopedLeasesAuthYAML)

	status, _, body := scrape(t, ts.URL, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("credential-less scrape of an authenticated broker = %d, want 401; body=%s", status, body)
	}
	if strings.Contains(body, metricNamespace) {
		t.Errorf("the refusal leaked metric data: %s", body)
	}
}

// TestMetricsHTTP_AuthOnWithNoAdminScopeRefusesEveryone pins the safe reading of
// "the operator configured authentication but no admin scope": nobody is an
// operator, so nobody may scrape. It is the same rule GET /leases applies when
// deciding whether to disclose its aggregates.
func TestMetricsHTTP_AuthOnWithNoAdminScopeRefusesEveryone(t *testing.T) {
	ts, _, _, _ := newMetricsTestServer(t, 4, twoPrincipalAuthYAML)

	for _, token := range []string{ownerToken, otherToken} {
		status, _, body := scrape(t, ts.URL, token)
		if status != http.StatusForbidden {
			t.Errorf("scrape with %s on a broker with no admin_scope = %d, want 403; body=%s",
				token, status, body)
		}
	}
}

// ---------------------------------------------------------------------------
// Content
// ---------------------------------------------------------------------------

// TestMetricsHTTP_GaugesTrackLiveRegistryState proves the gauges are read from
// live state at scrape time rather than accumulated: the same endpoint reports
// different numbers before and after leases come and go.
//
// The configured ceilings are asserted BESIDE the observed numbers, which is the
// gap E4-S4 left: a queue depth with no bound beside it cannot be read.
func TestMetricsHTTP_GaugesTrackLiveRegistryState(t *testing.T) {
	ts, reg, _, tickets := newMetricsTestServer(t, 4, "")
	reg.useQueueDepth(7)

	exp := parsePrometheusText(t, mustScrape(t, ts.URL, ""))
	if got := exp.mustSeries(t, metricNamespace+"slots_in_use", ""); got != 0 {
		t.Errorf("slots_in_use on an idle broker = %v, want 0", got)
	}
	if got := exp.mustSeries(t, metricNamespace+"max_concurrent", ""); got != 4 {
		t.Errorf("max_concurrent = %v, want 4", got)
	}
	if got := exp.mustSeries(t, metricNamespace+"max_queue_depth", ""); got != 7 {
		t.Errorf("max_queue_depth = %v, want 7", got)
	}

	ids, _, _ := seedTwoTenantLeases(t, reg) // fills the cap exactly

	// One lease is latched for teardown so the state breakdown is not all one
	// value.
	markLeaseDraining(t, reg, ids[0])

	// A parked waiter, so queue_depth is non-zero on the same scrape.
	queued := make(chan error, 1)
	go func() {
		_, err := reg.NewLeaseQueued(context.Background(), 5*time.Second, nexusauth.Principal{ID: ownerPrincipal})
		queued <- err
	}()
	waitForQueueLen(t, reg, 1)

	if _, err := tickets.mint(ids[1], ownerPrincipal); err != nil {
		t.Fatalf("mint ticket: %v", err)
	}

	exp = parsePrometheusText(t, mustScrape(t, ts.URL, ""))
	for _, tc := range []struct {
		name   string
		labels string
		want   float64
	}{
		{metricNamespace + "slots_in_use", "", 4},
		{metricNamespace + "max_concurrent", "", 4},
		{metricNamespace + "queue_depth", "", 1},
		{metricNamespace + "max_queue_depth", "", 7},
		{metricNamespace + "leases", `{state="` + surfaceStateSpawning + `"}`, 3},
		{metricNamespace + "leases", `{state="` + surfaceStateDraining + `"}`, 1},
		{metricNamespace + "leases", `{state="` + surfaceStateActive + `"}`, 0},
		// Tickets are inert with auth off, so nothing was issued.
		{metricNamespace + "tickets_outstanding", "", 0},
	} {
		if got := exp.mustSeries(t, tc.name, tc.labels); got != tc.want {
			t.Errorf("%s%s = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}

	reg.Remove(ids[0])
	if err := <-queued; err != nil {
		t.Fatalf("queued claim err: %v", err)
	}
}

// TestMetricsHTTP_CountersRecordWhatHappened walks each counter family through
// one increment and reads it back off the wire, so the wiring between the
// reporting call and the rendered series is proven end to end.
func TestMetricsHTTP_CountersRecordWhatHappened(t *testing.T) {
	ts, _, m, _ := newMetricsTestServer(t, 4, "")

	m.claimOutcome(claimOutcomeNoCapacity)
	m.claimOutcome(claimOutcomeNoCapacity)
	m.claimOutcome(claimOutcomeQueueTimeout)
	m.claimAccepted(1500 * time.Millisecond)
	m.spawnFailed(spawnFailureExitedEarly)
	m.frameDropped(frameDropClientBufferFull)
	m.replayGap(gapReasonEvicted)
	m.clientEvicted()
	m.reloadOutcome(reloadOutcomeApplied)
	m.reloadOutcome(reloadOutcomeRejected)
	m.leasesRestored(3)
	m.restoredSettled(2, 1)

	exp := parsePrometheusText(t, mustScrape(t, ts.URL, ""))
	for _, tc := range []struct {
		name   string
		labels string
		want   float64
	}{
		{metricNamespace + "claims_total", `{outcome="no_capacity"}`, 2},
		{metricNamespace + "claims_total", `{outcome="queue_timeout"}`, 1},
		{metricNamespace + "claims_total", `{outcome="accepted"}`, 1},
		{metricNamespace + "claims_total", `{outcome="spawn_failed"}`, 0},
		{metricNamespace + "spawn_failures_total", `{reason="exited_before_ready"}`, 1},
		{metricNamespace + "frames_dropped_total", `{reason="client_buffer_full"}`, 1},
		{metricNamespace + "frames_dropped_total", `{reason="undecodable"}`, 0},
		{metricNamespace + "replay_gaps_total", `{reason="` + gapReasonEvicted + `"}`, 1},
		{metricNamespace + "client_evictions_total", "", 1},
		{metricNamespace + "config_reloads_total", `{outcome="applied"}`, 1},
		{metricNamespace + "config_reloads_total", `{outcome="rejected"}`, 1},
		{metricNamespace + "restored_leases_total", `{outcome="restored"}`, 3},
		{metricNamespace + "restored_leases_total", `{outcome="reattached"}`, 2},
		{metricNamespace + "restored_leases_total", `{outcome="reaped"}`, 1},
		{metricNamespace + "claim_duration_seconds_count", "", 1},
	} {
		if got := exp.mustSeries(t, tc.name, tc.labels); got != tc.want {
			t.Errorf("%s%s = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}
}

// TestBrokerMetrics_NilReceiverIsInert proves the nil-safety every existing test
// depends on: a Registry (and therefore a Gateway, a ClaimServer, a reloader)
// wired without metrics must report into nothing rather than panic.
func TestBrokerMetrics_NilReceiverIsInert(t *testing.T) {
	var m *brokerMetrics
	m.claimOutcome(claimOutcomeAccepted)
	m.claimAccepted(time.Second)
	m.spawnFailed(spawnFailureExec)
	m.frameDropped(frameDropUndecodable)
	m.replayGap(gapReasonEvicted)
	m.clientEvicted()
	m.reloadOutcome(reloadOutcomeApplied)
	m.leasesRestored(2)
	m.restoredSettled(1, 1)

	var reg *Registry
	if reg.Metrics() != nil {
		t.Error("a nil Registry reported a non-nil counter set")
	}
	if got := reg.metricsSample(); got != (metricsSample{}) {
		t.Errorf("a nil Registry sampled %+v, want the zero sample", got)
	}
}

// TestCounterFamily_UndeclaredValueIsANoOp pins the cardinality bound at its
// source: a label value that is not in the declared set increments nothing and
// grows no series, so no call site can widen a label set at runtime.
func TestCounterFamily_UndeclaredValueIsANoOp(t *testing.T) {
	f := newCounterFamily("nexus_broker_test_total", "help", "outcome", []string{"a", "b"})
	f.inc("a")
	f.inc("c") // undeclared
	f.add("c", 9)

	var b strings.Builder
	f.render(&b)
	got := b.String()
	if strings.Contains(got, `outcome="c"`) {
		t.Errorf("an undeclared label value grew a series:\n%s", got)
	}
	if !strings.Contains(got, `nexus_broker_test_total{outcome="a"} 1`) {
		t.Errorf("the declared value did not record:\n%s", got)
	}
	if !strings.Contains(got, `nexus_broker_test_total{outcome="b"} 0`) {
		t.Errorf("an untouched declared value is missing:\n%s", got)
	}
}

// TestEscapeHelp pins the two sequences the exposition format reserves in a HELP
// line, so no future help string can corrupt a scrape.
func TestEscapeHelp(t *testing.T) {
	cases := map[string]string{
		"plain text":       "plain text",
		"a \\ backslash":   `a \\ backslash`,
		"two\nlines":       `two\nlines`,
		"both \\ and \nnl": `both \\ and \nnl`,
	}
	for in, want := range cases {
		if got := escapeHelp(in); got != want {
			t.Errorf("escapeHelp(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The counters are wired to the real paths
// ---------------------------------------------------------------------------

// TestClaimMetrics_RefusalsAreCountedDistinctly proves the instrumentation is
// wired into the LIVE claim path, and that the three refusals sharing HTTP 503
// land in three different series.
//
// That separation is the whole reason the outcome label exists: a dashboard
// grouping by status code sees one number climbing and cannot tell "raise
// max_concurrent" from "raise queue_wait_timeout" from "raise max_queue_depth".
func TestClaimMetrics_RefusalsAreCountedDistinctly(t *testing.T) {
	cases := []struct {
		name        string
		apply       func(*Config)
		park        int
		wantOutcome string
	}{
		{
			name:        "waiting disabled",
			apply:       func(c *Config) { c.QueueWaitTimeout = 0; c.MaxQueueDepth = 4 },
			wantOutcome: claimOutcomeNoCapacity,
		},
		{
			name:        "waited and gave up",
			apply:       func(c *Config) { c.QueueWaitTimeout = 50 * time.Millisecond; c.MaxQueueDepth = 4 },
			wantOutcome: claimOutcomeQueueTimeout,
		},
		{
			name:        "queue already full",
			apply:       func(c *Config) { c.QueueWaitTimeout = 5 * time.Second; c.MaxQueueDepth = 1 },
			park:        1,
			wantOutcome: claimOutcomeQueueFull,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := admissionConfig(t, "", tc.apply)
			metrics := newBrokerMetrics()
			ts, reg, _ := newAdmissionClaimServer(t, cfg, 1)
			reg.useMetrics(metrics)

			if _, err := reg.NewLease(anonymousOwner()); err != nil {
				t.Fatalf("fill capacity: %v", err)
			}
			parked := make([]chan error, 0, tc.park)
			for i := 0; i < tc.park; i++ {
				parked = append(parked, parkWaiter(t, reg, anonymousOwner(), i+1))
			}

			if status, _ := claimRefusal(t, ts, ""); status != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", status)
			}

			exp := parsePrometheusText(t, renderMetricsFor(reg, metrics))
			for _, outcome := range claimOutcomes {
				want := float64(0)
				if outcome == tc.wantOutcome {
					want = 1
				}
				label := `{outcome="` + outcome + `"}`
				if got := exp.mustSeries(t, metricNamespace+"claims_total", label); got != want {
					t.Errorf("claims_total%s = %v, want %v", label, got, want)
				}
			}
			// A refusal is not a spawn, so nothing may have been counted there.
			for _, reason := range spawnFailureReasons {
				label := `{reason="` + reason + `"}`
				if got := exp.mustSeries(t, metricNamespace+"spawn_failures_total", label); got != 0 {
					t.Errorf("spawn_failures_total%s = %v after a pure admission refusal, want 0", label, got)
				}
			}

			for _, ch := range parked {
				reg.Remove(oneLiveLeaseID(t, reg))
				<-ch
			}
		})
	}
}

// TestClaimMetrics_MalformedBodyCountsAsRejected proves the two refusals that
// never reach the spawn spine are still counted: a claim the broker threw out at
// the door is still a claim, and an outcome breakdown that silently omitted it
// would not add up to the request rate.
func TestClaimMetrics_MalformedBodyCountsAsRejected(t *testing.T) {
	cfg := admissionConfig(t, "", nil)
	metrics := newBrokerMetrics()
	ts, reg, _ := newAdmissionClaimServer(t, cfg, 4)
	reg.useMetrics(metrics)

	for _, body := range []string{`not json at all`, `{"config":""}`} {
		resp := doAuthed(t, http.MethodPost, ts.URL+"/claim", "", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST /claim %q = %d, want 400", body, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}

	exp := parsePrometheusText(t, renderMetricsFor(reg, metrics))
	if got := exp.mustSeries(t, metricNamespace+"claims_total", `{outcome="`+claimOutcomeRejected+`"}`); got != 2 {
		t.Errorf("claims_total{outcome=rejected} = %v, want 2", got)
	}
	if got := exp.mustSeries(t, metricNamespace+"claim_duration_seconds_count", ""); got != 0 {
		t.Errorf("claim_duration_seconds_count = %v after two refusals, want 0 — a refusal's "+
			"latency measures the refusal, not the spawn", got)
	}
}

// renderMetricsFor renders the exposition for a registry/counter pair without
// going through HTTP, for tests whose server is a claim server rather than a
// metrics one.
func renderMetricsFor(reg *Registry, m *brokerMetrics) string {
	return NewMetricsServer(testLogger(), reg, m, nil, nil, "").render()
}
