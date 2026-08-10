package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// newLeasesTestServer wires the GET /leases endpoint over an httptest server
// with authentication OFF, returning the server and the shared registry so a
// test can seed leases. Auth-off is the pre-scoping baseline: every caller is an
// operator, so these tests assert the response shape the endpoint has always had.
func newLeasesTestServer(t *testing.T, maxConcurrent int) (*httptest.Server, *Registry) {
	t.Helper()
	return newLeasesTestServerWithAuth(t, maxConcurrent, "")
}

// newLeasesTestServerWithAuth is newLeasesTestServer for a broker whose `auth:`
// block comes from authYAML (empty means no block at all, i.e. auth disabled).
//
// It mirrors run()'s topology: the listing is registered THROUGH the guard, and
// the handler is given the same guard plus the loaded admin scope. The config
// goes through the real loader so a test cannot prove scoping against a chain or
// an admin scope no operator could actually configure.
func newLeasesTestServerWithAuth(t *testing.T, maxConcurrent int, authYAML string) (*httptest.Server, *Registry) {
	t.Helper()
	cfg := mustLoadConfig(t, authYAML)
	if authYAML != "" && !cfg.AuthChain.Enabled() {
		t.Fatalf("newLeasesTestServerWithAuth: YAML produced a DISABLED chain; the test would prove nothing:\n%s", authYAML)
	}
	logger := testLogger()
	reg := NewRegistry(logger, maxConcurrent)
	guard := newAuthGuard(logger, cfg.AuthChain)
	ls := NewLeasesServer(logger, reg, guard, cfg.AdminScope)
	mux := http.NewServeMux()
	ls.Register(guard.Guard(mux))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, reg
}

// getLeases performs an unauthenticated GET /leases and decodes the snapshot,
// failing on any non-200 status.
func getLeases(t *testing.T, base string) RegistrySnapshot {
	t.Helper()
	return decodeLeases(t, getLeasesBody(t, base, ""))
}

// getLeasesAs performs GET /leases with a bearer token and decodes the snapshot.
func getLeasesAs(t *testing.T, base, token string) RegistrySnapshot {
	t.Helper()
	return decodeLeases(t, getLeasesBody(t, base, token))
}

// getLeasesBody performs GET /leases with an optional bearer token and returns
// the RAW response body, failing on any non-200 status.
//
// The raw bytes matter: the caller-scoped shape is defined by which keys are
// ABSENT, and a decode into RegistrySnapshot turns an omitted aggregate into a
// zero — the exact distinction under test.
func getLeasesBody(t *testing.T, base, token string) []byte {
	t.Helper()
	resp := doAuthed(t, http.MethodGet, base+"/leases", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /leases status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read leases body: %v", err)
	}
	return body
}

// decodeLeases decodes a GET /leases body into a snapshot.
func decodeLeases(t *testing.T, body []byte) RegistrySnapshot {
	t.Helper()
	var snap RegistrySnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("decode snapshot %q: %v", body, err)
	}
	return snap
}

// leaseIDsIn lists the lease ids in a snapshot, in response order.
func leaseIDsIn(snap RegistrySnapshot) []string {
	ids := make([]string, 0, len(snap.Leases))
	for _, l := range snap.Leases {
		ids = append(ids, l.ID)
	}
	return ids
}

// TestSurfaceState_MapsInternalLifecycle proves the operator-facing state
// projection: a not-yet-registered lease reads spawning, a registered/active
// lease reads active, and a latched teardown reads draining regardless of the
// internal state it was in.
func TestSurfaceState_MapsInternalLifecycle(t *testing.T) {
	cases := []struct {
		name      string
		state     leaseState
		releasing bool
		want      string
	}{
		{"pending", leaseStatePending, false, surfaceStateSpawning},
		{"registered", leaseStateRegistered, false, surfaceStateActive},
		{"active", leaseStateActive, false, surfaceStateActive},
		{"draining overrides pending", leaseStatePending, true, surfaceStateDraining},
		{"draining overrides active", leaseStateActive, true, surfaceStateDraining},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &lease{state: tc.state, releasing: tc.releasing}
			if got := l.surfaceState(); got != tc.want {
				t.Fatalf("surfaceState() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLeasesHTTP_NeverDisclosesSpawnSecret pins the non-disclosure rule on the
// projection most likely to break it: LeaseSnapshot is a field-by-field copy of
// a lease, so an added field is one careless line away from being listed.
//
// The assertion is on the RAW bytes of both the operator and the caller-scoped
// listings, not on LeaseSnapshot's fields, so a secret that reached the response
// under any key name — or inside another value — would fail it.
func TestLeasesHTTP_NeverDisclosesSpawnSecret(t *testing.T) {
	const secret = "5c9e1f70b2a34d68af0c1e2d3b4a5968"

	ts, reg := newLeasesTestServer(t, 4)
	id, _ := seedLease(t, reg, newFakeProcess(4243))
	reg.SetSpawnSecret(id, secret)

	body := getLeasesBody(t, ts.URL, "")
	if !bytes.Contains(body, []byte(id)) {
		t.Fatalf("the lease is not in the listing at all, so the assertion below is vacuous: %s", body)
	}
	if bytes.Contains(body, []byte(secret)) {
		t.Errorf("GET /leases disclosed the lease's spawn secret: %s", body)
	}

	// The same, one layer down: a caller that encodes the snapshot itself (an
	// internal caller, a future surface) must not find it either.
	snapJSON, err := json.Marshal(reg.Snapshot())
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if bytes.Contains(snapJSON, []byte(secret)) {
		t.Errorf("RegistrySnapshot carries the spawn secret: %s", snapJSON)
	}
}

// TestLeasesHTTP_ListsClaimedLeaseThenGoneAfterRelease proves the core surface:
// a seeded (claimed) lease appears with its id, pid, session id, and an active
// state plus the capacity aggregates; after release it disappears and the
// aggregates reflect the freed slot.
func TestLeasesHTTP_ListsClaimedLeaseThenGoneAfterRelease(t *testing.T) {
	ts, reg := newLeasesTestServer(t, 4)

	proc := newFakeProcess(4242)
	id, _ := seedLease(t, reg, proc) // NewLease + AttachInstance + SetProcess
	reg.MarkSessionID(id, "sess-abc")
	close(proc.exited) // let release's graceful path complete without a kill

	snap := getLeases(t, ts.URL)
	if snap.MaxConcurrent != 4 {
		t.Errorf("max_concurrent = %d, want 4", snap.MaxConcurrent)
	}
	if snap.SlotsInUse != 1 {
		t.Errorf("slots_in_use = %d, want 1", snap.SlotsInUse)
	}
	if snap.QueueDepth != 0 {
		t.Errorf("queue_depth = %d, want 0", snap.QueueDepth)
	}
	if len(snap.Leases) != 1 {
		t.Fatalf("leases len = %d, want 1", len(snap.Leases))
	}
	got := snap.Leases[0]
	if got.ID != id {
		t.Errorf("lease_id = %q, want %q", got.ID, id)
	}
	if got.SessionID != "sess-abc" {
		t.Errorf("session_id = %q, want %q", got.SessionID, "sess-abc")
	}
	if got.PID != 4242 {
		t.Errorf("pid = %d, want 4242", got.PID)
	}
	// An instance has registered but no client is attached: surface state is
	// active.
	if got.State != surfaceStateActive {
		t.Errorf("state = %q, want %q", got.State, surfaceStateActive)
	}
	if got.LastActivity.IsZero() || got.CreatedAt.IsZero() {
		t.Errorf("timestamps not populated: %+v", got)
	}

	// Release the lease; it must vanish from the surface and free its slot.
	if err := reg.releaseLease(id, "manual release", time.Second); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}
	after := getLeases(t, ts.URL)
	if len(after.Leases) != 0 {
		t.Fatalf("leases len after release = %d, want 0", len(after.Leases))
	}
	if after.SlotsInUse != 0 {
		t.Errorf("slots_in_use after release = %d, want 0", after.SlotsInUse)
	}
}

// TestLeasesHTTP_QueueDepthReflectsWaiters proves the aggregate queue_depth
// surfaces parked waiters: with cap=1 the only slot is held and a second,
// queued claim parks; GET /leases then reports queue_depth=1 while still
// listing exactly the one live lease.
func TestLeasesHTTP_QueueDepthReflectsWaiters(t *testing.T) {
	ts, reg := newLeasesTestServer(t, 1)

	holder, err := reg.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("occupy slot: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, qerr := reg.NewLeaseQueued(context.Background(), 5*time.Second, anonymousOwner())
		errCh <- qerr
	}()
	waitForQueueLen(t, reg, 1)

	snap := getLeases(t, ts.URL)
	if snap.QueueDepth != 1 {
		t.Errorf("queue_depth = %d, want 1", snap.QueueDepth)
	}
	if snap.SlotsInUse != 1 {
		t.Errorf("slots_in_use = %d, want 1", snap.SlotsInUse)
	}
	if len(snap.Leases) != 1 {
		t.Fatalf("leases len = %d, want 1 (only the holder is live)", len(snap.Leases))
	}
	if snap.Leases[0].ID != holder {
		t.Errorf("listed lease = %q, want holder %q", snap.Leases[0].ID, holder)
	}

	// Free the slot so the queued claim proceeds and the goroutine exits cleanly.
	reg.Remove(holder)
	if err := <-errCh; err != nil {
		t.Fatalf("queued claim err: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Per-caller scoping (E2-S3)
// ---------------------------------------------------------------------------

// Credentials the scoping fixture binds, on top of auth_test.go's
// ownerToken/otherToken pair (reused so "two principals" means the same two
// identities the ownership tests use).
const (
	adminToken     = "admin-token"
	adminPrincipal = "admin-principal"

	// testAdminScope is deliberately NOT defaultAdminScope: the endpoint must
	// honour the CONFIGURED scope, and a fixture built on the default value would
	// still pass if `auth.admin_scope` were ignored entirely.
	testAdminScope = "test.broker.operator"

	// A fourth valid credential that owns nothing, for the empty-listing case.
	noLeaseToken     = "no-lease-token"
	noLeasePrincipal = "no-lease-principal"
)

// scopedLeasesAuthYAML configures the four identities the scoping tests need:
// two lease-owning principals, one operator (holding the configured admin scope
// among others, so the check is a scope MEMBERSHIP test and not an equality test
// on the whole list), and one valid caller that owns nothing.
const scopedLeasesAuthYAML = `
auth:
  admin_scope: "` + testAdminScope + `"
  validators:
    - type: static
      tokens:
        - token: "` + ownerToken + `"
          principal: "` + ownerPrincipal + `"
        - token: "` + otherToken + `"
          principal: "` + otherPrincipal + `"
        - token: "` + adminToken + `"
          principal: "` + adminPrincipal + `"
          scopes: "unrelated.scope ` + testAdminScope + `"
        - token: "` + noLeaseToken + `"
          principal: "` + noLeasePrincipal + `"
`

// seedTwoTenantLeases seeds four leases ALTERNATING between the two owning
// principals, and returns every id in creation order plus the per-owner subsets
// (also in creation order).
//
// The alternation is the point: neither owner's leases are contiguous in the
// registry, so a per-caller listing that comes back in creation order is ordered
// because the snapshot sorted it — not because one tenant's leases happened to be
// adjacent.
func seedTwoTenantLeases(t *testing.T, reg *Registry) (all, ownerIDs, otherIDs []string) {
	t.Helper()
	for i := 0; i < 4; i++ {
		principal := ownerPrincipal
		if i%2 == 1 {
			principal = otherPrincipal
		}
		id, err := reg.NewLease(nexusauth.Principal{ID: principal})
		if err != nil {
			t.Fatalf("NewLease %d for %s: %v", i, principal, err)
		}
		all = append(all, id)
		if i%2 == 1 {
			otherIDs = append(otherIDs, id)
		} else {
			ownerIDs = append(ownerIDs, id)
		}
		time.Sleep(time.Millisecond) // distinct createdAt stamps
	}
	return all, ownerIDs, otherIDs
}

// TestLeasesHTTP_ScopedToCallersOwnLeases is the core of E2-S3: two principals
// with leases interleaved in the same registry each see EXACTLY their own, in
// creation order.
//
// reflect.DeepEqual on the id slice asserts three things at once — the caller's
// leases are all present, the other principal's are all absent, and the
// deterministic created_at ordering survives filtering.
func TestLeasesHTTP_ScopedToCallersOwnLeases(t *testing.T) {
	ts, reg := newLeasesTestServerWithAuth(t, 8, scopedLeasesAuthYAML)
	all, ownerIDs, otherIDs := seedTwoTenantLeases(t, reg)

	for _, tc := range []struct {
		name  string
		token string
		want  []string
	}{
		{"owner principal", ownerToken, ownerIDs},
		{"other principal", otherToken, otherIDs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := getLeasesAs(t, ts.URL, tc.token)
			if got := leaseIDsIn(snap); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("leases = %v, want exactly its own %v (in creation order)", got, tc.want)
			}
			// Ordering, restated on the timestamps themselves so the assertion does
			// not depend on the seeding order being the sorted order.
			for i := 1; i < len(snap.Leases); i++ {
				if snap.Leases[i].CreatedAt.Before(snap.Leases[i-1].CreatedAt) {
					t.Errorf("lease[%d] created_at %v precedes lease[%d] %v — not sorted",
						i, snap.Leases[i].CreatedAt, i-1, snap.Leases[i-1].CreatedAt)
				}
			}
		})
	}

	// The registry really did hold both tenants' leases throughout, so the
	// assertions above mean "filtered", not "the registry was empty".
	if got := leaseIDsIn(reg.Snapshot()); !reflect.DeepEqual(got, all) {
		t.Fatalf("registry holds %v, want all %v — the fixture, not the filter, is broken", got, all)
	}
}

// TestLeasesHTTP_NonAdminAggregatesOmitted proves the capacity aggregates are
// absent — not zeroed — for a caller-scoped listing, so a client cannot infer
// another tenant's load from them.
//
// Omitted rather than zeroed is the deliberate choice: a zero is a claim about
// the broker that a client unaware of its own privilege level would believe,
// whereas a missing key is unambiguously "not disclosed".
//
// The aggregates are made genuinely NON-ZERO first (cap filled, plus a parked
// waiter) and the operator view is asserted to report them on the same server, so
// this proves suppression rather than "there was nothing to report".
func TestLeasesHTTP_NonAdminAggregatesOmitted(t *testing.T) {
	ts, reg := newLeasesTestServerWithAuth(t, 4, scopedLeasesAuthYAML)
	seedTwoTenantLeases(t, reg) // exactly fills cap=4

	// Park a claim so queue_depth is non-zero too.
	queued := make(chan error, 1)
	go func() {
		_, err := reg.NewLeaseQueued(context.Background(), 5*time.Second, nexusauth.Principal{ID: ownerPrincipal})
		queued <- err
	}()
	waitForQueueLen(t, reg, 1)

	body := getLeasesBody(t, ts.URL, ownerToken)
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatalf("decode caller-scoped body %q: %v", body, err)
	}
	for _, aggregate := range []string{"max_concurrent", "slots_in_use", "queue_depth"} {
		if raw, present := keys[aggregate]; present {
			t.Errorf("caller-scoped response discloses %q = %s", aggregate, raw)
		}
	}
	if len(keys) != 1 {
		t.Errorf("caller-scoped response has %d top-level keys (%v), want only \"leases\"", len(keys), keys)
	}

	// The very same server discloses real, non-zero aggregates to an operator —
	// so the absences above are suppression, not an idle broker.
	admin := getLeasesAs(t, ts.URL, adminToken)
	if admin.MaxConcurrent != 4 || admin.SlotsInUse != 4 || admin.QueueDepth != 1 {
		t.Fatalf("operator aggregates = {max %d, in-use %d, queued %d}, want {4, 4, 1} — "+
			"the suppression assertions above would be vacuous",
			admin.MaxConcurrent, admin.SlotsInUse, admin.QueueDepth)
	}

	// Free a slot so the queued goroutine finishes cleanly.
	reg.Remove(admin.Leases[0].ID)
	if err := <-queued; err != nil {
		t.Fatalf("queued claim err: %v", err)
	}
}

// TestLeasesHTTP_AdminScopeSeesEveryLeaseAndAggregates proves the operator view:
// a caller holding the configured admin scope gets every lease from both
// principals plus the aggregates.
//
// The response bytes are compared against an encoding of the UNRESTRICTED
// snapshot, which is what the endpoint returned before scoping existed — so this
// pins "byte-identical to today's response", key order included, rather than
// merely "the fields are present".
func TestLeasesHTTP_AdminScopeSeesEveryLeaseAndAggregates(t *testing.T) {
	ts, reg := newLeasesTestServerWithAuth(t, 6, scopedLeasesAuthYAML)
	all, _, _ := seedTwoTenantLeases(t, reg)

	body := bytes.TrimSpace(getLeasesBody(t, ts.URL, adminToken))
	want, err := json.Marshal(reg.Snapshot())
	if err != nil {
		t.Fatalf("marshal unrestricted snapshot: %v", err)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("operator response differs from the unrestricted shape:\n got %s\nwant %s", body, want)
	}

	snap := decodeLeases(t, body)
	if got := leaseIDsIn(snap); !reflect.DeepEqual(got, all) {
		t.Errorf("operator leases = %v, want every lease %v", got, all)
	}
	if snap.MaxConcurrent != 6 || snap.SlotsInUse != 4 || snap.QueueDepth != 0 {
		t.Errorf("operator aggregates = {max %d, in-use %d, queued %d}, want {6, 4, 0}",
			snap.MaxConcurrent, snap.SlotsInUse, snap.QueueDepth)
	}
}

// TestLeasesHTTP_AuthDisabledShowsEveryLeaseAndAggregates is the
// backward-compatibility guarantee: with no `auth:` block, an unauthenticated
// GET /leases returns exactly what it always returned — every lease and every
// aggregate.
//
// The seeded leases are owned by two NAMED principals, neither of them the
// anonymous caller. So this is not vacuous: were the auth-disabled branch
// missing, the owner filter would compare the anonymous caller's empty id against
// those owners and the response would come back empty.
func TestLeasesHTTP_AuthDisabledShowsEveryLeaseAndAggregates(t *testing.T) {
	ts, reg := newLeasesTestServer(t, 5) // no auth block at all
	all, _, _ := seedTwoTenantLeases(t, reg)

	body := bytes.TrimSpace(getLeasesBody(t, ts.URL, "")) // no credential presented
	want, err := json.Marshal(reg.Snapshot())
	if err != nil {
		t.Fatalf("marshal unrestricted snapshot: %v", err)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("auth-disabled response differs from the unrestricted shape:\n got %s\nwant %s", body, want)
	}

	snap := decodeLeases(t, body)
	if got := leaseIDsIn(snap); !reflect.DeepEqual(got, all) {
		t.Errorf("auth-disabled leases = %v, want every lease %v", got, all)
	}
	if snap.MaxConcurrent != 5 || snap.SlotsInUse != 4 {
		t.Errorf("auth-disabled aggregates = {max %d, in-use %d}, want {5, 4}",
			snap.MaxConcurrent, snap.SlotsInUse)
	}
}

// TestLeasesHTTP_CallerWithNoLeasesGetsEmptyList pins the empty case: a valid
// caller that owns nothing gets 200 and an empty JSON array — not 404, not an
// error, and not `null` (which several clients decode as "field missing").
func TestLeasesHTTP_CallerWithNoLeasesGetsEmptyList(t *testing.T) {
	ts, reg := newLeasesTestServerWithAuth(t, 8, scopedLeasesAuthYAML)
	seedTwoTenantLeases(t, reg) // other principals' leases exist and stay hidden

	// getLeasesBody fails the test on any non-200, so reaching the compare below
	// is itself the "not 404, not an error" assertion.
	body := getLeasesBody(t, ts.URL, noLeaseToken)
	if got, want := string(bytes.TrimSpace(body)), `{"leases":[]}`; got != want {
		t.Fatalf("lease-less caller response = %s, want %s", got, want)
	}
}

// TestLeasesHTTP_EmptyAdminScopeGrantsNoOperatorView pins the meaning of
// `admin_scope: ""`: with auth on and no admin scope configured, NOBODY gets the
// operator view — the endpoint is caller-scoped for every credential, including
// one that would otherwise have qualified.
func TestLeasesHTTP_EmptyAdminScopeGrantsNoOperatorView(t *testing.T) {
	noAdminYAML := strings.Replace(scopedLeasesAuthYAML,
		`admin_scope: "`+testAdminScope+`"`, `admin_scope: ""`, 1)
	if noAdminYAML == scopedLeasesAuthYAML {
		t.Fatal("fixture rewrite did not apply; the test would prove nothing")
	}
	ts, reg := newLeasesTestServerWithAuth(t, 8, noAdminYAML)
	seedTwoTenantLeases(t, reg)

	// The admin credential owns no lease, so a caller-scoped listing is empty and
	// carries no aggregates.
	if got, want := string(bytes.TrimSpace(getLeasesBody(t, ts.URL, adminToken))), `{"leases":[]}`; got != want {
		t.Fatalf("response with admin_scope disabled = %s, want %s", got, want)
	}
}

// TestLeasesHTTP_SortedByCreation proves the surface is deterministically
// ordered by creation time (then id), so operators and tests see a stable list.
func TestLeasesHTTP_SortedByCreation(t *testing.T) {
	ts, reg := newLeasesTestServer(t, 0)

	var ids []string
	for i := 0; i < 5; i++ {
		id, err := reg.NewLease(anonymousOwner())
		if err != nil {
			t.Fatalf("lease %d: %v", i, err)
		}
		ids = append(ids, id)
		time.Sleep(time.Millisecond) // distinct createdAt stamps
	}

	snap := getLeases(t, ts.URL)
	if len(snap.Leases) != len(ids) {
		t.Fatalf("leases len = %d, want %d", len(snap.Leases), len(ids))
	}
	for i, want := range ids {
		if snap.Leases[i].ID != want {
			t.Fatalf("lease[%d] = %q, want %q (creation order)", i, snap.Leases[i].ID, want)
		}
	}
}
