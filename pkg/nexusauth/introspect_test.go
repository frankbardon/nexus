package nexusauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Nothing in this file is a checked-in credential. Every token and client secret
// is a literal invented in the test that uses it, and every time-dependent
// assertion moves the fake clock from jwks_test.go rather than sleeping.

const (
	testIntrospectClientID = "nexus-broker"
	testIntrospectSecret   = "not-a-real-secret"
)

// fakeIntrospectionEndpoint is an RFC 7662 endpoint. It counts requests, which
// is what lets a test assert that a cache hit cost no round trip, that an
// outage was not cached, and that concurrent lookups of one token collapsed onto
// a single request.
type fakeIntrospectionEndpoint struct {
	server *httptest.Server

	mu       sync.Mutex
	requests int
	response map[string]any
	body     string // raw body override, for the malformed-response cases
	status   int
	block    chan struct{} // when non-nil the handler waits on it, standing in for a hung IdP

	lastForm  url.Values
	lastUser  string
	lastPass  string
	lastBasic bool
}

func newFakeIntrospectionEndpoint(t *testing.T) *fakeIntrospectionEndpoint {
	t.Helper()
	e := &fakeIntrospectionEndpoint{
		response: map[string]any{"active": true, "sub": "alice"},
	}
	e.server = httptest.NewServer(http.HandlerFunc(e.serve))
	t.Cleanup(e.server.Close)
	return e
}

func (e *fakeIntrospectionEndpoint) serve(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	user, pass, basic := r.BasicAuth()

	e.mu.Lock()
	e.requests++
	e.lastForm = r.PostForm
	e.lastUser, e.lastPass, e.lastBasic = user, pass, basic
	status, body, block := e.status, e.body, e.block
	doc := make(map[string]any, len(e.response))
	for k, v := range e.response {
		doc[k] = v
	}
	e.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if body != "" {
		_, _ = w.Write([]byte(body))
		return
	}
	_ = json.NewEncoder(w).Encode(doc)
}

func (e *fakeIntrospectionEndpoint) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.requests
}

func (e *fakeIntrospectionEndpoint) setResponse(doc map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.response = doc
}

func (e *fakeIntrospectionEndpoint) setRaw(status int, body string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status, e.body = status, body
}

func (e *fakeIntrospectionEndpoint) form() url.Values {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastForm
}

func (e *fakeIntrospectionEndpoint) basicAuth() (string, string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastUser, e.lastPass, e.lastBasic
}

// introspectValidator builds a validator against e, with the fake clock and any
// per-test overrides applied.
func introspectValidator(t *testing.T, e *fakeIntrospectionEndpoint, clock *fakeClock, mutate func(*introspectOptions)) *IntrospectValidator {
	t.Helper()
	opts := introspectOptions{
		IntrospectionURL: e.server.URL,
		ClientID:         testIntrospectClientID,
		ClientSecret:     testIntrospectSecret,
		Mapping:          ClaimMapping{PrincipalClaim: "sub"},
		httpClient:       e.server.Client(),
	}
	if clock != nil {
		opts.now = clock.now
	}
	if mutate != nil {
		mutate(&opts)
	}
	v, err := newIntrospectValidator(opts)
	if err != nil {
		t.Fatalf("newIntrospectValidator: %v", err)
	}
	return v
}

// bearerRequest builds a request presenting token.
func bearerRequest(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/claim", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

// TestIntrospectValidatorVerdicts is the table over what the endpoint can say.
// Every row that is not an outright active:true is a denial: the point of the
// table is that there is no path from a strange response to an allow.
func TestIntrospectValidatorVerdicts(t *testing.T) {
	tests := []struct {
		name string
		// status/body override the JSON response entirely when body is set.
		status   int
		body     string
		response map[string]any
		mapping  ClaimMapping

		wantPrincipal string
		wantTenant    string
		wantScopes    []string
		wantKind      Kind
		wantReason    string
	}{
		{
			name:          "active token",
			response:      map[string]any{"active": true, "sub": "alice"},
			wantPrincipal: "alice",
		},
		{
			name: "active token with tenant and space-delimited scopes",
			response: map[string]any{
				"active": true, "sub": "alice", "org": "acme", "scope": "broker.claim broker.release",
			},
			mapping:       ClaimMapping{PrincipalClaim: "sub", TenantClaim: "org", ScopesClaim: "scope"},
			wantPrincipal: "alice",
			wantTenant:    "acme",
			wantScopes:    []string{"broker.claim", "broker.release"},
		},
		{
			name: "active token with array scopes",
			response: map[string]any{
				"active": true, "sub": "alice", "scope": []any{"broker.claim", "broker.release"},
			},
			mapping:       ClaimMapping{PrincipalClaim: "sub", ScopesClaim: "scope"},
			wantPrincipal: "alice",
			wantScopes:    []string{"broker.claim", "broker.release"},
		},
		{
			name:       "inactive token",
			response:   map[string]any{"active": false},
			wantKind:   KindInvalidCredential,
			wantReason: "not active",
		},
		{
			name:       "inactive token carrying claims anyway",
			response:   map[string]any{"active": false, "sub": "alice"},
			wantKind:   KindInvalidCredential,
			wantReason: "not active",
		},
		{
			name:       "no active member",
			response:   map[string]any{"sub": "alice"},
			wantKind:   KindInvalidCredential,
			wantReason: `no "active" member`,
		},
		{
			name:       "active is not a boolean",
			response:   map[string]any{"active": "true", "sub": "alice"},
			wantKind:   KindInvalidCredential,
			wantReason: "not a boolean",
		},
		{
			name:       "malformed body",
			body:       `{"active": tru`,
			wantKind:   KindUnavailable,
			wantReason: "not a JSON object",
		},
		{
			name:       "body is a JSON array",
			body:       `[{"active": true}]`,
			wantKind:   KindUnavailable,
			wantReason: "not a JSON object",
		},
		{
			name:       "body is JSON null",
			body:       `null`,
			wantKind:   KindUnavailable,
			wantReason: "not a JSON object",
		},
		{
			name:       "server error",
			status:     http.StatusInternalServerError,
			wantKind:   KindUnavailable,
			wantReason: "answered 500",
		},
		{
			name:       "endpoint refuses our client credentials",
			status:     http.StatusUnauthorized,
			wantKind:   KindUnavailable,
			wantReason: "answered 401",
		},
		{
			name:       "principal claim absent",
			response:   map[string]any{"active": true, "uid": "alice"},
			wantKind:   KindInvalidCredential,
			wantReason: `has no "sub" claim`,
		},
		{
			name:       "principal claim empty",
			response:   map[string]any{"active": true, "sub": ""},
			wantKind:   KindInvalidCredential,
			wantReason: `claim "sub" is empty`,
		},
		{
			name:       "principal claim is not a scalar",
			response:   map[string]any{"active": true, "sub": map[string]any{"id": "alice"}},
			wantKind:   KindInvalidCredential,
			wantReason: "not a usable principal id",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newFakeIntrospectionEndpoint(t)
			if tc.response != nil {
				e.setResponse(tc.response)
			}
			if tc.status != 0 || tc.body != "" {
				e.setRaw(tc.status, tc.body)
			}
			mapping := tc.mapping
			if mapping.PrincipalClaim == "" {
				mapping = ClaimMapping{PrincipalClaim: "sub"}
			}
			v := introspectValidator(t, e, newFakeClock(), func(o *introspectOptions) {
				o.Mapping = mapping
			})

			p, err := v.Validate(context.Background(), bearerRequest("opaque-token"))

			if tc.wantKind == "" {
				if err != nil {
					t.Fatalf("Validate: unexpected error: %v", err)
				}
				if p.ID != tc.wantPrincipal {
					t.Errorf("principal id = %q, want %q", p.ID, tc.wantPrincipal)
				}
				if p.Tenant != tc.wantTenant {
					t.Errorf("tenant = %q, want %q", p.Tenant, tc.wantTenant)
				}
				if strings.Join(p.Scopes, " ") != strings.Join(tc.wantScopes, " ") {
					t.Errorf("scopes = %v, want %v", p.Scopes, tc.wantScopes)
				}
				if p.Claims == nil {
					t.Error("claims should carry the full introspection response for audit")
				}
				return
			}

			if err == nil {
				t.Fatalf("Validate: want a denial, got principal %+v", p)
			}
			if p.ID != "" {
				t.Errorf("a denial must not produce a principal, got %q", p.ID)
			}
			if got := KindOf(err); got != tc.wantKind {
				t.Errorf("kind = %q, want %q (err: %v)", got, tc.wantKind, err)
			}
			if !strings.Contains(err.Error(), tc.wantReason) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantReason)
			}
		})
	}
}

// TestIntrospectValidatorNoCredential covers the request that presents nothing:
// it must be distinguishable from a rejection, and must not cost a round trip.
func TestIntrospectValidatorNoCredential(t *testing.T) {
	e := newFakeIntrospectionEndpoint(t)
	v := introspectValidator(t, e, newFakeClock(), nil)

	_, err := v.Validate(context.Background(), httptest.NewRequest(http.MethodPost, "/claim", nil))
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("want ErrNoCredential, got %v", err)
	}
	if e.count() != 0 {
		t.Errorf("a request with no bearer token must not reach the endpoint, got %d requests", e.count())
	}
}

// TestIntrospectValidatorTimeout asserts a hung identity provider cannot hang
// the caller: the validator gives up on its own timeout and denies.
func TestIntrospectValidatorTimeout(t *testing.T) {
	e := newFakeIntrospectionEndpoint(t)
	release := make(chan struct{})
	e.mu.Lock()
	e.block = release
	e.mu.Unlock()
	t.Cleanup(func() { close(release) })

	v := introspectValidator(t, e, newFakeClock(), func(o *introspectOptions) {
		o.HTTPTimeout = 50 * time.Millisecond
		// A client with no timeout of its own, so the assertion is that the
		// validator's own context bound is what stops the wait.
		o.httpClient = &http.Client{Transport: e.server.Client().Transport}
	})

	start := time.Now()
	_, err := v.Validate(context.Background(), bearerRequest("opaque-token"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a hung endpoint must be a denial, not an allow")
	}
	if got := KindOf(err); got != KindUnavailable {
		t.Errorf("kind = %q, want %q (err: %v)", got, KindUnavailable, err)
	}
	// Generous relative to the 50ms bound: the point is that it returned at all,
	// not that it returned in a precise time.
	if elapsed > 5*time.Second {
		t.Errorf("Validate took %s; the timeout did not bound it", elapsed)
	}
}

// TestIntrospectValidatorCacheHitCostsNoRoundTrip is the core caching claim.
//
// It is non-vacuous: the endpoint counts every request it serves, so deleting
// the cache lookup in introspectionCache.resolve (or the store in publish) makes
// the second Validate call hit the server and the count assertion fail.
func TestIntrospectValidatorCacheHitCostsNoRoundTrip(t *testing.T) {
	e := newFakeIntrospectionEndpoint(t)
	e.setResponse(map[string]any{"active": true, "sub": "alice", "org": "acme"})
	v := introspectValidator(t, e, newFakeClock(), func(o *introspectOptions) {
		o.Mapping = ClaimMapping{PrincipalClaim: "sub", TenantClaim: "org"}
	})

	for i := 0; i < 5; i++ {
		p, err := v.Validate(context.Background(), bearerRequest("opaque-token"))
		if err != nil {
			t.Fatalf("Validate #%d: %v", i, err)
		}
		if p.ID != "alice" || p.Tenant != "acme" {
			t.Fatalf("Validate #%d: principal = %+v, want alice/acme", i, p)
		}
	}
	if got := e.count(); got != 1 {
		t.Errorf("endpoint served %d requests, want 1: the cache is not answering repeats", got)
	}

	// A *different* token is a different key and must still cost a round trip —
	// otherwise the "cache hit" above could be the cache answering everything.
	e.setResponse(map[string]any{"active": true, "sub": "bob"})
	p, err := v.Validate(context.Background(), bearerRequest("another-token"))
	if err != nil {
		t.Fatalf("Validate with a second token: %v", err)
	}
	if p.ID != "bob" {
		t.Errorf("principal id = %q, want bob", p.ID)
	}
	if got := e.count(); got != 2 {
		t.Errorf("endpoint served %d requests, want 2", got)
	}
}

// TestIntrospectValidatorCachedPrincipalIsACopy guards the cache against a
// caller mutating the Principal it was handed and poisoning every later hit.
func TestIntrospectValidatorCachedPrincipalIsACopy(t *testing.T) {
	e := newFakeIntrospectionEndpoint(t)
	e.setResponse(map[string]any{"active": true, "sub": "alice", "scope": "a b"})
	v := introspectValidator(t, e, newFakeClock(), func(o *introspectOptions) {
		o.Mapping = ClaimMapping{PrincipalClaim: "sub", ScopesClaim: "scope"}
	})

	first, err := v.Validate(context.Background(), bearerRequest("opaque-token"))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	first.Scopes[0] = "admin"
	first.Claims["sub"] = "mallory"

	second, err := v.Validate(context.Background(), bearerRequest("opaque-token"))
	if err != nil {
		t.Fatalf("Validate (cached): %v", err)
	}
	if second.Scopes[0] != "a" {
		t.Errorf("cached scopes were mutated by an earlier caller: %v", second.Scopes)
	}
	if second.Claims["sub"] != "alice" {
		t.Errorf("cached claims were mutated by an earlier caller: %v", second.Claims["sub"])
	}
}

// TestIntrospectCacheTTLBoundedByExp asserts a cache entry cannot outlive the
// token it describes, even when the configured maximum is far longer.
func TestIntrospectCacheTTLBoundedByExp(t *testing.T) {
	clock := newFakeClock()
	e := newFakeIntrospectionEndpoint(t)
	e.setResponse(map[string]any{
		"active": true,
		"sub":    "alice",
		"exp":    clock.now().Add(30 * time.Second).Unix(),
	})
	v := introspectValidator(t, e, clock, func(o *introspectOptions) {
		o.CacheTTL = 10 * time.Minute // deliberately much longer than exp
	})

	if _, err := v.Validate(context.Background(), bearerRequest("opaque-token")); err != nil {
		t.Fatalf("first Validate: %v", err)
	}

	// Still inside the token's lifetime: served from cache.
	clock.advance(20 * time.Second)
	if _, err := v.Validate(context.Background(), bearerRequest("opaque-token")); err != nil {
		t.Fatalf("second Validate: %v", err)
	}
	if got := e.count(); got != 1 {
		t.Fatalf("endpoint served %d requests before exp, want 1", got)
	}

	// Past exp: the entry must be gone even though cache_ttl has 9+ minutes left.
	clock.advance(11 * time.Second)
	if _, err := v.Validate(context.Background(), bearerRequest("opaque-token")); err != nil {
		t.Fatalf("third Validate: %v", err)
	}
	if got := e.count(); got != 2 {
		t.Errorf("endpoint served %d requests after exp, want 2: the entry outlived the token", got)
	}
}

// TestIntrospectCacheTTLFallsBackToMax covers the endpoint that reports no exp.
// The fallback must be the configured maximum, never unbounded caching.
func TestIntrospectCacheTTLFallsBackToMax(t *testing.T) {
	clock := newFakeClock()
	e := newFakeIntrospectionEndpoint(t)
	e.setResponse(map[string]any{"active": true, "sub": "alice"}) // no exp
	v := introspectValidator(t, e, clock, func(o *introspectOptions) {
		o.CacheTTL = time.Minute
	})

	if _, err := v.Validate(context.Background(), bearerRequest("opaque-token")); err != nil {
		t.Fatalf("first Validate: %v", err)
	}
	clock.advance(59 * time.Second)
	if _, err := v.Validate(context.Background(), bearerRequest("opaque-token")); err != nil {
		t.Fatalf("second Validate: %v", err)
	}
	if got := e.count(); got != 1 {
		t.Fatalf("endpoint served %d requests inside the TTL, want 1", got)
	}

	clock.advance(2 * time.Second)
	if _, err := v.Validate(context.Background(), bearerRequest("opaque-token")); err != nil {
		t.Fatalf("third Validate: %v", err)
	}
	if got := e.count(); got != 2 {
		t.Errorf("endpoint served %d requests past the TTL, want 2: caching is unbounded", got)
	}
}

// TestIntrospectCacheExpInThePast covers an endpoint contradicting itself —
// active:true alongside an exp that has already passed. The verdict is honoured
// (the issuer is the authority) but nothing is cached, so the contradiction
// cannot persist.
func TestIntrospectCacheExpInThePast(t *testing.T) {
	clock := newFakeClock()
	e := newFakeIntrospectionEndpoint(t)
	e.setResponse(map[string]any{
		"active": true,
		"sub":    "alice",
		"exp":    clock.now().Add(-time.Minute).Unix(),
	})
	v := introspectValidator(t, e, clock, nil)

	for i := 0; i < 2; i++ {
		if _, err := v.Validate(context.Background(), bearerRequest("opaque-token")); err != nil {
			t.Fatalf("Validate #%d: %v", i, err)
		}
	}
	if got := e.count(); got != 2 {
		t.Errorf("endpoint served %d requests, want 2: an already-expired verdict was cached", got)
	}
}

// TestIntrospectNegativeVerdictIsCached asserts a definitive refusal is
// remembered, so a client retrying a dead token in a loop does not become
// introspection traffic.
func TestIntrospectNegativeVerdictIsCached(t *testing.T) {
	clock := newFakeClock()
	e := newFakeIntrospectionEndpoint(t)
	e.setResponse(map[string]any{"active": false})
	v := introspectValidator(t, e, clock, func(o *introspectOptions) {
		o.NegativeCacheTTL = 30 * time.Second
	})

	for i := 0; i < 3; i++ {
		_, err := v.Validate(context.Background(), bearerRequest("dead-token"))
		if !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("Validate #%d: want ErrInvalidCredential, got %v", i, err)
		}
	}
	if got := e.count(); got != 1 {
		t.Errorf("endpoint served %d requests, want 1: refusals are not cached", got)
	}

	clock.advance(31 * time.Second)
	if _, err := v.Validate(context.Background(), bearerRequest("dead-token")); err == nil {
		t.Fatal("want a denial")
	}
	if got := e.count(); got != 2 {
		t.Errorf("endpoint served %d requests past the negative TTL, want 2", got)
	}
}

// TestIntrospectUnavailableIsNotCached is the counterpart: an outage must not be
// remembered, or the cache would extend it past its actual end.
func TestIntrospectUnavailableIsNotCached(t *testing.T) {
	e := newFakeIntrospectionEndpoint(t)
	e.setRaw(http.StatusServiceUnavailable, "")
	v := introspectValidator(t, e, newFakeClock(), nil)

	for i := 0; i < 3; i++ {
		_, err := v.Validate(context.Background(), bearerRequest("opaque-token"))
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Validate #%d: want ErrUnavailable, got %v", i, err)
		}
	}
	if got := e.count(); got != 3 {
		t.Fatalf("endpoint served %d requests, want 3: an outage was cached", got)
	}

	// Recovery must be immediate, with no TTL to wait out.
	e.setRaw(0, "")
	p, err := v.Validate(context.Background(), bearerRequest("opaque-token"))
	if err != nil {
		t.Fatalf("Validate after recovery: %v", err)
	}
	if p.ID != "alice" {
		t.Errorf("principal id = %q, want alice", p.ID)
	}
}

// TestIntrospectCacheKeyIsAHashNotTheToken inspects the cache's own keys. A
// memory dump or a debug print of them must not hand over live credentials.
func TestIntrospectCacheKeyIsAHashNotTheToken(t *testing.T) {
	const token = "a-live-bearer-token"
	e := newFakeIntrospectionEndpoint(t)
	v := introspectValidator(t, e, newFakeClock(), nil)

	if _, err := v.Validate(context.Background(), bearerRequest(token)); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	v.cache.mu.Lock()
	defer v.cache.mu.Unlock()
	if len(v.cache.entries) != 1 {
		t.Fatalf("cache holds %d entries, want 1", len(v.cache.entries))
	}
	want := hashToken(token)
	for k := range v.cache.entries {
		if k != want {
			t.Errorf("cache key is not the SHA-256 of the token")
		}
		if strings.Contains(string(k[:]), token) {
			t.Errorf("cache key contains the token verbatim")
		}
	}
}

// TestIntrospectSingleFlightAndConcurrency runs the validator hard from many
// goroutines. Under -race this is the concurrency-safety assertion; the request
// count is the single-flight assertion (a burst for one token must not multiply
// into a burst of round trips).
func TestIntrospectSingleFlightAndConcurrency(t *testing.T) {
	e := newFakeIntrospectionEndpoint(t)
	gate := make(chan struct{})
	e.mu.Lock()
	e.block = gate
	e.mu.Unlock()

	v := introspectValidator(t, e, newFakeClock(), nil)

	const goroutines = 32
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	ids := make([]string, goroutines)
	ready := make(chan struct{}, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready <- struct{}{}
			p, err := v.Validate(context.Background(), bearerRequest("opaque-token"))
			ids[i], errs[i] = p.ID, err
		}(i)
	}
	for i := 0; i < goroutines; i++ {
		<-ready
	}
	// Let the one in-flight request complete; every other goroutine either waits
	// on it or reads the cache it fills.
	close(gate)
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if ids[i] != "alice" {
			t.Fatalf("goroutine %d: principal id = %q, want alice", i, ids[i])
		}
	}
	if got := e.count(); got != 1 {
		t.Errorf("endpoint served %d requests for one token, want 1: concurrent lookups did not collapse", got)
	}

	// Now hammer distinct tokens concurrently, which exercises the map under
	// contention with no single-flight collapsing to hide races.
	e.mu.Lock()
	e.block = nil
	e.mu.Unlock()
	var wg2 sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			for j := 0; j < 4; j++ {
				_, _ = v.Validate(context.Background(), bearerRequest(fmt.Sprintf("token-%d-%d", i, j)))
			}
		}(i)
	}
	wg2.Wait()
}

// TestIntrospectWaiterHonoursItsOwnContext asserts that a caller waiting on
// someone else's in-flight lookup can still give up when its own request is
// cancelled — one slow lookup must not pin an unrelated request.
func TestIntrospectWaiterHonoursItsOwnContext(t *testing.T) {
	e := newFakeIntrospectionEndpoint(t)
	gate := make(chan struct{})
	e.mu.Lock()
	e.block = gate
	e.mu.Unlock()
	t.Cleanup(func() { close(gate) })

	v := introspectValidator(t, e, newFakeClock(), nil)

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_, _ = v.Validate(context.Background(), bearerRequest("opaque-token"))
	}()

	// Wait until the leader has registered itself as in-flight.
	waitFor(t, func() bool {
		v.cache.mu.Lock()
		defer v.cache.mu.Unlock()
		return len(v.cache.inflight) == 1
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := v.Validate(ctx, bearerRequest("opaque-token"))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable for an abandoned wait, got %v", err)
	}
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

// TestIntrospectionCacheIsBounded asserts the entry map cannot grow without
// bound: the keys come from caller-supplied tokens, so an unbounded map would be
// a memory-growth lever for an unauthenticated caller.
func TestIntrospectionCacheIsBounded(t *testing.T) {
	const max = 16
	clock := newFakeClock()
	c := newIntrospectionCache(time.Minute, time.Minute, max, clock.now)

	fetch := func(_ context.Context, token string) (Principal, time.Time, error) {
		return Principal{ID: token}, time.Time{}, nil
	}
	for i := 0; i < max*10; i++ {
		if _, err := c.resolve(context.Background(), fmt.Sprintf("token-%d", i), fetch); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		c.mu.Lock()
		size := len(c.entries)
		c.mu.Unlock()
		if size > max {
			t.Fatalf("cache grew to %d entries, bound is %d", size, max)
		}
	}

	// Entries that merely expired must also be reclaimed, not just evicted under
	// pressure.
	clock.advance(2 * time.Minute)
	for i := 0; i < max; i++ {
		if _, err := c.resolve(context.Background(), fmt.Sprintf("fresh-%d", i), fetch); err != nil {
			t.Fatalf("resolve fresh %d: %v", i, err)
		}
	}
	c.mu.Lock()
	size := len(c.entries)
	c.mu.Unlock()
	if size > max {
		t.Fatalf("cache grew to %d entries after expiry, bound is %d", size, max)
	}
}

// TestIntrospectRequestShape asserts the RFC 7662 request: a form POST carrying
// the token, with the broker's own credentials presented the configured way.
func TestIntrospectRequestShape(t *testing.T) {
	t.Run("basic auth by default", func(t *testing.T) {
		e := newFakeIntrospectionEndpoint(t)
		v := introspectValidator(t, e, newFakeClock(), nil)
		if _, err := v.Validate(context.Background(), bearerRequest("opaque-token")); err != nil {
			t.Fatalf("Validate: %v", err)
		}

		form := e.form()
		if got := form.Get("token"); got != "opaque-token" {
			t.Errorf("form token = %q, want the presented token", got)
		}
		if got := form.Get("token_type_hint"); got != "access_token" {
			t.Errorf("token_type_hint = %q, want access_token", got)
		}
		if form.Get("client_secret") != "" {
			t.Error("the client secret must not be in the request body when using basic auth")
		}
		user, pass, ok := e.basicAuth()
		if !ok {
			t.Fatal("no Authorization: Basic header was sent")
		}
		// RFC 6749 §2.3.1: the id and secret are form-urlencoded before base64.
		if user != url.QueryEscape(testIntrospectClientID) || pass != url.QueryEscape(testIntrospectSecret) {
			t.Errorf("basic credentials = %q/%q, want the form-urlencoded client id and secret", user, pass)
		}
	})

	t.Run("client_secret_post", func(t *testing.T) {
		e := newFakeIntrospectionEndpoint(t)
		v := introspectValidator(t, e, newFakeClock(), func(o *introspectOptions) {
			o.ClientAuth = ClientAuthPost
		})
		if _, err := v.Validate(context.Background(), bearerRequest("opaque-token")); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		form := e.form()
		if form.Get("client_id") != testIntrospectClientID {
			t.Errorf("client_id = %q, want %q", form.Get("client_id"), testIntrospectClientID)
		}
		if form.Get("client_secret") != testIntrospectSecret {
			t.Errorf("client_secret was not sent in the body")
		}
		if _, _, ok := e.basicAuth(); ok {
			t.Error("client_secret_post must not also send a Basic header")
		}
	})

	t.Run("token_type_hint can be suppressed", func(t *testing.T) {
		e := newFakeIntrospectionEndpoint(t)
		v := introspectValidator(t, e, newFakeClock(), func(o *introspectOptions) {
			o.TokenTypeHint, o.TokenTypeHintSet = "", true
		})
		if _, err := v.Validate(context.Background(), bearerRequest("opaque-token")); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if _, present := e.form()["token_type_hint"]; present {
			t.Error("an explicitly empty token_type_hint must not be sent")
		}
	})
}

// TestIntrospectSecretIsNotInErrors asserts a denial an operator might paste
// into an issue does not carry the broker's client secret.
func TestIntrospectSecretIsNotInErrors(t *testing.T) {
	e := newFakeIntrospectionEndpoint(t)
	e.setRaw(http.StatusInternalServerError, "")
	v := introspectValidator(t, e, newFakeClock(), nil)

	_, err := v.Validate(context.Background(), bearerRequest("opaque-token"))
	if err == nil {
		t.Fatal("want a denial")
	}
	if strings.Contains(err.Error(), testIntrospectSecret) {
		t.Errorf("denial leaks the client secret: %v", err)
	}
	if strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte(testIntrospectSecret))) {
		t.Errorf("denial leaks the encoded client secret: %v", err)
	}
}

// TestNewIntrospectValidatorRejects covers the construction guards. Each one
// exists so a misconfiguration fails the boot rather than the first claim.
func TestNewIntrospectValidatorRejects(t *testing.T) {
	base := func() introspectOptions {
		return introspectOptions{
			IntrospectionURL: "https://id.example/introspect",
			ClientID:         testIntrospectClientID,
			Mapping:          ClaimMapping{PrincipalClaim: "sub"},
		}
	}
	tests := []struct {
		name   string
		mutate func(*introspectOptions)
		want   string
	}{
		{"no url", func(o *introspectOptions) { o.IntrospectionURL = "" }, "introspection_url is required"},
		{"relative url", func(o *introspectOptions) { o.IntrospectionURL = "/introspect" }, "want an absolute URL"},
		{"plain http", func(o *introspectOptions) { o.IntrospectionURL = "http://id.example/introspect" }, "http is only allowed for a loopback host"},
		{"userinfo", func(o *introspectOptions) { o.IntrospectionURL = "https://u:p@id.example/introspect" }, "userinfo is not supported"},
		{"bad scheme", func(o *introspectOptions) { o.IntrospectionURL = "ftp://id.example/introspect" }, "scheme must be https"},
		{"no principal claim", func(o *introspectOptions) { o.Mapping = ClaimMapping{} }, "principal_claim is required"},
		{"no client id", func(o *introspectOptions) { o.ClientID = "" }, "client_id is required"},
		{"bad client auth", func(o *introspectOptions) { o.ClientAuth = "mtls" }, `client_auth must be "basic" or "post"`},
		{"negative cache ttl", func(o *introspectOptions) { o.CacheTTL = -time.Second }, "cache_ttl must not be negative"},
		{"over-long cache ttl", func(o *introspectOptions) { o.CacheTTL = time.Hour }, "cache_ttl must not exceed 15m0s"},
		{"over-long negative cache ttl", func(o *introspectOptions) { o.NegativeCacheTTL = time.Hour }, "negative_cache_ttl must not exceed 15m0s"},
		{"negative http timeout", func(o *introspectOptions) { o.HTTPTimeout = -time.Second }, "http_timeout must not be negative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := base()
			tc.mutate(&opts)
			_, err := newIntrospectValidator(opts)
			if err == nil {
				t.Fatal("want a construction error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}

	t.Run("loopback http is allowed", func(t *testing.T) {
		opts := base()
		opts.IntrospectionURL = "http://127.0.0.1:9000/introspect"
		if _, err := newIntrospectValidator(opts); err != nil {
			t.Fatalf("loopback http should be accepted: %v", err)
		}
	})
}

// TestBuildIntrospectValidatorFromConfig covers the YAML surface, including the
// secret-sourcing convention.
func TestBuildIntrospectValidatorFromConfig(t *testing.T) {
	valid := func() map[string]any {
		return map[string]any{
			"type":               ValidatorTypeIntrospect,
			"introspection_url":  "https://id.example/introspect",
			"client_id":          testIntrospectClientID,
			"client_secret":      testIntrospectSecret,
			"client_auth":        "post",
			"token_type_hint":    "access_token",
			"cache_ttl":          "2m",
			"negative_cache_ttl": "20s",
			"http_timeout":       "3s",
			"principal_claim":    "sub",
			"tenant_claim":       "org",
			"scopes_claim":       "scope",
		}
	}

	t.Run("full entry builds", func(t *testing.T) {
		chain, err := ChainFromMap(map[string]any{"validators": []any{valid()}})
		if err != nil {
			t.Fatalf("ChainFromMap: %v", err)
		}
		if names := chain.Names(); len(names) != 1 || names[0] != ValidatorTypeIntrospect {
			t.Errorf("chain names = %v, want [introspect]", names)
		}
	})

	t.Run("secret from env", func(t *testing.T) {
		const envName = "NEXUSAUTH_TEST_INTROSPECT_SECRET"
		t.Setenv(envName, "  "+testIntrospectSecret+"\n")
		entry := valid()
		delete(entry, "client_secret")
		entry["client_secret_env"] = envName

		vc, err := parseValidatorEntry(entry)
		if err != nil {
			t.Fatalf("parseValidatorEntry: %v", err)
		}
		got, err := resolveSecret(vc.Options, keyClientSecret, keyClientSecretEnv)
		if err != nil {
			t.Fatalf("resolveSecret: %v", err)
		}
		if got != testIntrospectSecret {
			t.Errorf("resolved secret = %q, want the trimmed env value", got)
		}
		if _, err := buildIntrospectValidator(vc); err != nil {
			t.Fatalf("buildIntrospectValidator: %v", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(map[string]any)
		env    map[string]string
		want   string
	}{
		{
			name:   "unknown key",
			mutate: func(m map[string]any) { m["introspect_url"] = "https://id.example/i" },
			want:   `unknown key(s) "introspect_url"`,
		},
		{
			name:   "missing introspection_url",
			mutate: func(m map[string]any) { delete(m, "introspection_url") },
			want:   "introspection_url is required",
		},
		{
			name:   "missing client_id",
			mutate: func(m map[string]any) { delete(m, "client_id") },
			want:   "client_id is required",
		},
		{
			name:   "missing principal_claim",
			mutate: func(m map[string]any) { delete(m, "principal_claim") },
			want:   "principal_claim is required",
		},
		{
			name: "both secret sources",
			mutate: func(m map[string]any) {
				m["client_secret_env"] = "NEXUSAUTH_TEST_UNUSED"
			},
			want: "client_secret and client_secret_env are mutually exclusive",
		},
		{
			name: "env var unset",
			mutate: func(m map[string]any) {
				delete(m, "client_secret")
				m["client_secret_env"] = "NEXUSAUTH_TEST_MISSING_SECRET"
			},
			want: `names environment variable "NEXUSAUTH_TEST_MISSING_SECRET", which is unset or empty`,
		},
		{
			name: "env var empty",
			mutate: func(m map[string]any) {
				delete(m, "client_secret")
				m["client_secret_env"] = "NEXUSAUTH_TEST_EMPTY_SECRET"
			},
			env:  map[string]string{"NEXUSAUTH_TEST_EMPTY_SECRET": "   "},
			want: `names environment variable "NEXUSAUTH_TEST_EMPTY_SECRET", which is unset or empty`,
		},
		{
			name:   "bare number duration",
			mutate: func(m map[string]any) { m["cache_ttl"] = 120 },
			want:   `cache_ttl: want a duration string such as "10m"`,
		},
		{
			name:   "non-string client_id",
			mutate: func(m map[string]any) { m["client_id"] = 42 },
			want:   "client_id: want a string",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			entry := valid()
			tc.mutate(entry)
			_, err := ChainFromMap(map[string]any{"validators": []any{entry}})
			if err == nil {
				t.Fatal("want a config error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}

// TestIntrospectInChain asserts the chain treats an unavailable verdict as the
// aggregate kind even when a cheaper validator flatly rejected the credential.
// The alternative — reporting "credential rejected" — tells every client to
// re-authenticate against the identity provider that is already failing.
func TestIntrospectInChain(t *testing.T) {
	e := newFakeIntrospectionEndpoint(t)
	e.setRaw(http.StatusBadGateway, "")

	static, err := NewStaticValidator([]StaticToken{{Token: "ci-token", Principal: Principal{ID: "ci"}}})
	if err != nil {
		t.Fatalf("NewStaticValidator: %v", err)
	}
	chain := NewChain(
		NamedValidator{Name: "static", Validator: static},
		NamedValidator{Name: "introspect", Validator: introspectValidator(t, e, newFakeClock(), nil)},
	)

	_, err = chain.Validate(context.Background(), bearerRequest("an-idp-token"))
	if err == nil {
		t.Fatal("want a denial")
	}
	if got := KindOf(err); got != KindUnavailable {
		t.Errorf("aggregate kind = %q, want %q (err: %v)", got, KindUnavailable, err)
	}

	// The static token still wins outright, and never reaches the endpoint.
	before := e.count()
	p, err := chain.Validate(context.Background(), bearerRequest("ci-token"))
	if err != nil {
		t.Fatalf("static validation: %v", err)
	}
	if p.ID != "ci" {
		t.Errorf("principal id = %q, want ci", p.ID)
	}
	if e.count() != before {
		t.Errorf("a static match must short-circuit before the introspection round trip")
	}
}
