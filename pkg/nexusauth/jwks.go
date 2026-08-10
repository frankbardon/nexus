package nexusauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Tunables for the JWKS key cache. They are package constants rather than config
// keys because they bound resource use rather than express policy: an operator
// tunes freshness with cache_ttl, not the size of the document we are willing to
// read.
const (
	// maxJWKSBodyBytes caps the key-set document we will read. A JWKS with a
	// handful of keys is a couple of kilobytes; the cap stops a compromised or
	// misconfigured endpoint from turning a key fetch into unbounded allocation.
	maxJWKSBodyBytes = 1 << 20 // 1 MiB

	// minRSAModulusBits is the smallest RSA modulus we will build a verification
	// key from. Anything under 2048 bits is not a key a current issuer should be
	// signing with, and accepting one would let a weak key gate lease ownership.
	minRSAModulusBits = 2048

	// maxJWSHeaderBytes caps the base64url header segment we will decode when
	// reading a token's `alg` before verification. The header is a small JSON
	// object; the cap keeps the pre-verification path from decoding an
	// attacker-sized blob.
	maxJWSHeaderBytes = 4096
)

// keyEntry is one published verification key plus what the issuer said about it.
type keyEntry struct {
	key crypto.PublicKey

	// alg is the JWK's optional `alg` parameter (RFC 7517 §4.4) — the single
	// algorithm the issuer intends this key to be used with. Empty when the
	// issuer declared none. When it is present it is enforced: a key the issuer
	// published for RS256 must not verify an ES256 token, even if both algorithms
	// are in the operator's allowlist.
	alg string
}

// keySet is one immutable snapshot of an issuer's published signature keys.
// It is replaced wholesale on refresh and never mutated in place, which is what
// makes a cache read a plain map lookup under a short-lived lock.
type keySet struct {
	// byKID maps a `kid` onto its entry. Keys published without a `kid` are
	// absent here and only reachable through the single-key fallback in lookup.
	byKID map[string]keyEntry

	// all holds every usable key in document order, so a token with no `kid` can
	// be verified when — and only when — the issuer publishes exactly one key.
	all []keyEntry
}

// lookup resolves the verification key for kid.
//
// A token with no `kid` is answered only when the key set has exactly one usable
// key. Trying every key instead would make verification cost scale with the size
// of the key set on a path an unauthenticated caller controls, and it would make
// a rotation window silently accept a key the token never named.
func (s *keySet) lookup(kid string) (keyEntry, bool) {
	if s == nil {
		return keyEntry{}, false
	}
	if kid != "" {
		k, ok := s.byKID[kid]
		return k, ok
	}
	if len(s.all) == 1 {
		return s.all[0], true
	}
	return keyEntry{}, false
}

// keyCache holds an issuer's key set in memory and decides when to go back to
// the network for it. It is the reason a validated request costs no round trip:
// verification sits on POST /claim and on the WebSocket connect path, so a
// per-request JWKS fetch would put the identity provider's latency (and
// availability) directly in front of every session.
//
// The policy in one place:
//
//   - A `kid` we already hold is answered from memory, always. If the snapshot is
//     older than ttl a refresh runs *behind* the request rather than in front of
//     it, so a TTL expiry never shows up as request latency.
//   - A `kid` we do not hold triggers one synchronous fetch, because that is the
//     only way key rotation can work without a restart.
//   - That fetch is rate limited two ways: per-`kid` (an unknown `kid` is
//     remembered as unknown for negTTL) and globally (a fetch that failed to
//     resolve the `kid` it was made for blocks further on-demand fetches for
//     negTTL). Without the global bound, a flood of tokens bearing *distinct*
//     random `kid`s would amplify one-to-one into JWKS traffic.
//   - A fetch failure is never an allow. A key we hold keeps verifying — that is
//     the deliberate availability trade, and it is why the cache exists — but an
//     endpoint we cannot reach cannot vouch for a key we do not have.
//
// It is safe for concurrent use; one instance serves every request.
type keyCache struct {
	url     string
	client  *http.Client
	timeout time.Duration
	ttl     time.Duration
	negTTL  time.Duration
	now     func() time.Time

	mu  sync.Mutex
	set *keySet
	// fetchedAt is when set was retrieved, for the staleness test.
	fetchedAt time.Time
	// unknownKIDs is the negative cache: kid → when we last confirmed we could
	// not resolve it.
	unknownKIDs map[string]time.Time
	// lastUnresolved is when an on-demand fetch last failed to produce the key it
	// was made for, whether because the endpoint was unreachable or because the
	// key genuinely is not published. It bounds on-demand fetch traffic
	// regardless of how many distinct kids a caller invents.
	lastUnresolved time.Time
	// lastFetchErr is the error from the most recent fetch attempt, so a request
	// that merely waited on someone else's in-flight fetch can report why it got
	// nothing.
	lastFetchErr error
	// inflight is non-nil while a fetch is running and is closed when it
	// finishes, collapsing concurrent misses onto one request.
	inflight chan struct{}
}

// newKeyCache builds a cache over a JWKS URL. The caller has already validated
// url, the durations, and the client.
func newKeyCache(url string, client *http.Client, timeout, ttl, negTTL time.Duration, now func() time.Time) *keyCache {
	return &keyCache{
		url:         url,
		client:      client,
		timeout:     timeout,
		ttl:         ttl,
		negTTL:      negTTL,
		now:         now,
		unknownKIDs: make(map[string]time.Time),
	}
}

// key resolves the verification key for the kid a token named, fetching the
// issuer's key set only when it has to. The returned error is always a
// classified *Error of kind KindInvalidCredential — see the note on Kind in
// errors.go: this package has no "temporarily unavailable" verdict, and failing
// closed is the only safe reading of "we could not establish who you are".
func (c *keyCache) key(ctx context.Context, kid string) (keyEntry, error) {
	// Hot path: a key we already hold. No network, and a stale snapshot is
	// refreshed behind the request.
	if k, stale, ok := c.cached(kid); ok {
		if stale {
			c.refreshInBackground()
		}
		return k, nil
	}

	// The kid is unknown. Either the issuer rotated since our last fetch, or the
	// token is forged. One refresh answers both — subject to the rate limits that
	// stop the forged case becoming a request amplifier.
	if reason, blocked := c.refuseWithoutFetch(kid); blocked {
		return keyEntry{}, NewError(KindInvalidCredential, reason, nil)
	}
	fetchErr := c.fetchOnce(ctx)
	if k, _, ok := c.cached(kid); ok {
		return k, nil
	}
	c.recordUnresolved(kid)
	if fetchErr != nil {
		return keyEntry{}, NewError(KindInvalidCredential,
			"the issuer's key set could not be fetched and the token's key id is not cached", fetchErr)
	}
	return keyEntry{}, NewError(KindInvalidCredential, unresolvedKIDReason(kid), nil)
}

// cached reports the key for kid from the current snapshot, and whether that
// snapshot is past its TTL.
func (c *keyCache) cached(kid string) (entry keyEntry, stale, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, found := c.set.lookup(kid)
	if !found {
		return keyEntry{}, false, false
	}
	return k, c.now().Sub(c.fetchedAt) > c.ttl, true
}

// refuseWithoutFetch reports whether an unknown kid must be denied without going
// to the network, and the operator-facing reason why.
func (c *keyCache) refuseWithoutFetch(kid string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if kid != "" {
		if at, seen := c.unknownKIDs[kid]; seen && now.Sub(at) < c.negTTL {
			return unresolvedKIDReason(kid), true
		}
	}
	if !c.lastUnresolved.IsZero() && now.Sub(c.lastUnresolved) < c.negTTL {
		// Deliberately not "unknown kid": the honest statement is that we
		// declined to re-ask the issuer this soon, which is what an operator
		// needs to see if a genuine rotation is being delayed.
		return "the token's key id is not cached and the issuer's key set was queried too recently to ask again", true
	}
	return "", false
}

// recordUnresolved remembers that we could not resolve kid, arming both the
// per-kid negative cache and the global on-demand fetch bound.
func (c *keyCache) recordUnresolved(kid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.lastUnresolved = now
	if kid == "" {
		// Nothing to key a negative entry on; the global bound carries this case.
		return
	}
	// Bound the negative cache so a flood of distinct invented kids cannot grow
	// it without limit. Dropping the whole map is fine: it is an optimization,
	// and the global bound still throttles fetches while it refills.
	if len(c.unknownKIDs) >= maxNegativeCacheEntries {
		c.unknownKIDs = make(map[string]time.Time, maxNegativeCacheEntries)
	}
	c.unknownKIDs[kid] = now
}

// maxNegativeCacheEntries bounds the per-kid negative cache. It is generous
// relative to any real key set: the entries exist to absorb a repeat of the same
// bad token, not to be a complete record.
const maxNegativeCacheEntries = 1024

// refreshInBackground kicks off a refresh without blocking the caller, which is
// how a TTL expiry stays off the request path. It is a no-op while a fetch is
// already in flight.
func (c *keyCache) refreshInBackground() {
	c.mu.Lock()
	busy := c.inflight != nil
	c.mu.Unlock()
	if busy {
		return
	}
	go func() {
		// A detached refresh cannot borrow the request's context — that is
		// cancelled the moment the response is written — so it gets its own
		// bound. An identity provider that hangs must not leak a goroutine.
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()
		_ = c.fetchOnce(ctx)
	}()
}

// fetchOnce retrieves and installs a fresh key set, collapsing concurrent calls
// onto a single HTTP request. A caller that arrives while a fetch is in flight
// waits for it and then re-reads the cache rather than issuing a second request.
func (c *keyCache) fetchOnce(ctx context.Context) error {
	c.mu.Lock()
	if done := c.inflight; done != nil {
		c.mu.Unlock()
		select {
		case <-done:
			c.mu.Lock()
			err := c.lastFetchErr
			c.mu.Unlock()
			return err
		case <-ctx.Done():
			return fmt.Errorf("waiting for an in-flight JWKS fetch: %w", ctx.Err())
		}
	}
	done := make(chan struct{})
	c.inflight = done
	c.mu.Unlock()

	set, err := c.fetch(ctx)

	c.mu.Lock()
	c.lastFetchErr = err
	if err == nil {
		c.set = set
		c.fetchedAt = c.now()
		// A fresh key set invalidates every "we could not resolve this" verdict:
		// the kid we just rejected may be in the document we just retrieved.
		c.unknownKIDs = make(map[string]time.Time)
		c.lastUnresolved = time.Time{}
	}
	c.inflight = nil
	close(done)
	c.mu.Unlock()
	return err
}

// fetch performs one JWKS HTTP request and parses the result. Every step is
// bounded — the client carries a timeout and the body read is capped — because
// this call sits behind POST /claim: a hanging identity provider must not hang a
// claim.
func (c *keyCache) fetch(ctx context.Context) (*keySet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("building JWKS request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching JWKS from %s: %w", c.url, err)
	}
	defer func() {
		// Drain within the cap so the connection can be reused, then close.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxJWKSBodyBytes))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching JWKS from %s: unexpected status %s", c.url, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading JWKS from %s: %w", c.url, err)
	}
	if len(body) > maxJWKSBodyBytes {
		return nil, fmt.Errorf("JWKS from %s is larger than %d bytes", c.url, maxJWKSBodyBytes)
	}

	set, err := parseJWKS(body)
	if err != nil {
		return nil, fmt.Errorf("parsing JWKS from %s: %w", c.url, err)
	}
	return set, nil
}

// unresolvedKIDReason phrases a kid we could not resolve, quoting and truncating
// the value because it comes from an unauthenticated token header and ends up in
// an operator's log line.
func unresolvedKIDReason(kid string) string {
	if kid == "" {
		return "token names no key id and the issuer publishes more than one key"
	}
	return fmt.Sprintf("token key id %q is not published by the issuer", truncateForLog(kid))
}

// truncateForLog bounds an attacker-supplied string before it reaches a log
// record or an error message.
func truncateForLog(s string) string {
	const limit = 64
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// jwksDocument is the JWK Set wire shape (RFC 7517 §5).
type jwksDocument struct {
	Keys []jwksKey `json:"keys"`
}

// jwksKey is the subset of JWK fields needed to build a signature-verification
// key. The JWKS parse is hand-rolled on purpose: the module graph has no JOSE
// library, and reconstructing a public key from its base64url parameters is a
// few dozen lines against stdlib crypto.
type jwksKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`

	// RSA parameters.
	N string `json:"n"`
	E string `json:"e"`

	// EC parameters.
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// parseJWKS turns a JWK Set document into a keySet.
//
// An individual key we cannot use is skipped rather than fatal: issuers publish
// encryption keys and key types we do not support alongside the one they sign
// with, and failing the whole fetch over an unrelated entry would take a
// deployment down on a change that does not concern it. A document with *no*
// usable key is an error, and it names why each entry was skipped — that is the
// case an operator has to be able to diagnose.
func parseJWKS(body []byte) (*keySet, error) {
	var doc jwksDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decoding key set: %w", err)
	}

	set := &keySet{byKID: make(map[string]keyEntry, len(doc.Keys))}
	skipped := make([]string, 0, len(doc.Keys))
	for i, jk := range doc.Keys {
		// `use` is optional. When present, anything other than "sig" is an
		// encryption key and must never verify a signature.
		if jk.Use != "" && jk.Use != "sig" {
			skipped = append(skipped, fmt.Sprintf("keys[%d]: use is %q, not \"sig\"", i, jk.Use))
			continue
		}
		key, err := jk.publicKey()
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("keys[%d]: %v", i, err))
			continue
		}
		pub := keyEntry{key: key, alg: jk.Alg}
		if jk.Kid != "" {
			// First entry wins for a duplicated kid. A key set with two keys under
			// one id is malformed, and last-one-wins would make verification
			// depend on document order.
			if _, dup := set.byKID[jk.Kid]; dup {
				skipped = append(skipped, fmt.Sprintf("keys[%d]: duplicate kid %q", i, truncateForLog(jk.Kid)))
				continue
			}
			set.byKID[jk.Kid] = pub
		}
		set.all = append(set.all, pub)
	}
	if len(set.all) == 0 {
		if len(skipped) == 0 {
			return nil, fmt.Errorf("key set contains no keys")
		}
		return nil, fmt.Errorf("key set contains no usable signature keys (%s)", strings.Join(skipped, "; "))
	}
	return set, nil
}

// publicKey reconstructs the verification key for one JWK entry.
func (k jwksKey) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		return k.rsaPublicKey()
	case "EC":
		return k.ecdsaPublicKey()
	case "":
		return nil, fmt.Errorf("kty is required")
	default:
		return nil, fmt.Errorf("unsupported key type %q", truncateForLog(k.Kty))
	}
}

// rsaPublicKey builds an *rsa.PublicKey from the base64url modulus and exponent
// (RFC 7518 §6.3.1).
func (k jwksKey) rsaPublicKey() (*rsa.PublicKey, error) {
	nb, err := decodeBase64URL(k.N)
	if err != nil {
		return nil, fmt.Errorf("modulus: %w", err)
	}
	eb, err := decodeBase64URL(k.E)
	if err != nil {
		return nil, fmt.Errorf("exponent: %w", err)
	}

	n := new(big.Int).SetBytes(nb)
	if n.BitLen() < minRSAModulusBits {
		return nil, fmt.Errorf("modulus is %d bits, the minimum is %d", n.BitLen(), minRSAModulusBits)
	}

	// rsa.PublicKey.E is an int. A value that does not fit is not something a
	// real issuer published, and truncating it would build a key that verifies
	// nothing while looking valid.
	e := new(big.Int).SetBytes(eb)
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() > math.MaxInt32 {
		return nil, fmt.Errorf("public exponent is out of range")
	}
	if e.Bit(0) == 0 {
		return nil, fmt.Errorf("public exponent must be odd")
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

// ecdsaPublicKey builds an *ecdsa.PublicKey from the curve name and the
// base64url affine coordinates (RFC 7518 §6.2.1).
func (k jwksKey) ecdsaPublicKey() (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	switch k.Crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	case "":
		return nil, fmt.Errorf("crv is required for an EC key")
	default:
		return nil, fmt.Errorf("unsupported curve %q", truncateForLog(k.Crv))
	}

	xb, err := decodeBase64URL(k.X)
	if err != nil {
		return nil, fmt.Errorf("x coordinate: %w", err)
	}
	yb, err := decodeBase64URL(k.Y)
	if err != nil {
		return nil, fmt.Errorf("y coordinate: %w", err)
	}
	// RFC 7518 fixes the octet length of each coordinate at the curve's field
	// size; a short or long value is a malformed key, not one to left-pad into
	// shape.
	size := (curve.Params().BitSize + 7) / 8
	if len(xb) != size || len(yb) != size {
		return nil, fmt.Errorf("%s coordinates must be %d bytes each, got %d and %d", k.Crv, size, len(xb), len(yb))
	}

	// Assemble the uncompressed SEC 1 point and let crypto/ecdsa parse it:
	// ParseUncompressedPublicKey rejects a point that is not on the curve, which
	// is the check that stops a crafted key set being used as an invalid-curve
	// oracle. Building the key by hand would skip it.
	point := make([]byte, 0, 1+2*size)
	point = append(point, 4)
	point = append(point, xb...)
	point = append(point, yb...)
	pub, err := ecdsa.ParseUncompressedPublicKey(curve, point)
	if err != nil {
		return nil, fmt.Errorf("invalid %s point: %w", k.Crv, err)
	}
	return pub, nil
}

// decodeBase64URL decodes a JWK parameter. RFC 7517 §3 mandates unpadded
// base64url, but padding is tolerated rather than rejected: an issuer that pads
// is technically wrong and entirely harmless, and refusing its key set would
// take a deployment down over cosmetics.
func decodeBase64URL(s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("value is required")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return nil, fmt.Errorf("not base64url: %w", err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("value decodes to zero bytes")
	}
	return b, nil
}
