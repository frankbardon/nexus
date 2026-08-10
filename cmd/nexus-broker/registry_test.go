package main

import (
	"context"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/nexusauth"
)

// testOwner is a fully populated Principal — every reference-typed field
// non-nil — so the copy-in/copy-out tests below have something to alias if the
// registry ever stops cloning.
func testOwner() nexusauth.Principal {
	return nexusauth.Principal{
		ID:     "ci-runner",
		Tenant: "acme",
		Scopes: []string{"broker.claim", "broker.release"},
		Claims: map[string]any{"sub": "ci-runner", "iss": "https://idp.example"},
	}
}

// TestLeaseOwner_StampedAtCreation covers the base case for both constructors:
// the owner handed to NewLease is the owner LeaseOwner reports back, in full.
func TestLeaseOwner_StampedAtCreation(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)

	id, err := reg.NewLease(testOwner())
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}

	got, ok := reg.LeaseOwner(id)
	if !ok {
		t.Fatal("LeaseOwner reported the lease unknown")
	}
	if got.ID != "ci-runner" {
		t.Errorf("owner ID = %q, want ci-runner", got.ID)
	}
	if got.Tenant != "acme" {
		t.Errorf("owner Tenant = %q, want acme", got.Tenant)
	}
	if !got.HasScope("broker.claim") || !got.HasScope("broker.release") {
		t.Errorf("owner Scopes = %v, want both granted scopes carried (E2-S3 needs them)", got.Scopes)
	}
	if got.Claims["iss"] != "https://idp.example" {
		t.Errorf("owner Claims = %v, want the raw claim set carried for audit", got.Claims)
	}
}

// TestLeaseOwnerQueued_StampedAtCreation covers the claim path's actual entry
// point. NewLeaseQueued reserves the capacity slot before the lease exists, so
// the owner has to be threaded past that acquire without disturbing it — this
// asserts both halves: one slot held, and the owner stamped.
func TestLeaseOwnerQueued_StampedAtCreation(t *testing.T) {
	reg := NewRegistry(testLogger(), 2)

	id, err := reg.NewLeaseQueued(context.Background(), 5*time.Second, testOwner())
	if err != nil {
		t.Fatalf("NewLeaseQueued: %v", err)
	}
	if got := reg.SlotsInUse(); got != 1 {
		t.Errorf("SlotsInUse = %d, want 1 (ownership must not change slot accounting)", got)
	}
	got, ok := reg.LeaseOwner(id)
	if !ok {
		t.Fatal("LeaseOwner reported the lease unknown")
	}
	if got.ID != "ci-runner" {
		t.Errorf("owner ID = %q, want ci-runner", got.ID)
	}
}

// TestLeaseOwner_AnonymousWhenAuthDisabled pins the auth-off contract: a lease
// claimed with no authenticated principal has a well-defined zero owner, and
// reading it is an ordinary read — no absent value for any caller to trip over.
func TestLeaseOwner_AnonymousWhenAuthDisabled(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)

	id, err := reg.NewLease(anonymousOwner())
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}

	got, ok := reg.LeaseOwner(id)
	if !ok {
		t.Fatal("LeaseOwner reported the lease unknown")
	}
	if got.ID != "" || got.Tenant != "" || got.Scopes != nil || got.Claims != nil {
		t.Errorf("anonymous owner = %+v, want the zero Principal", got)
	}
	// The property the enforcement stories rely on: with auth disabled every
	// lease is owned by the same anonymous identity, so an id-equality check
	// admits everyone exactly as the broker behaved before auth existed.
	if got.ID != anonymousOwner().ID {
		t.Error("anonymous owners must compare equal by ID")
	}
}

// TestLeaseOwner_UnknownLease pins the distinction a caller must not collapse:
// an unknown lease reports ok=false, not an anonymously-owned lease.
func TestLeaseOwner_UnknownLease(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)

	got, ok := reg.LeaseOwner("no-such-lease")
	if ok {
		t.Error("LeaseOwner on an unknown id must report ok=false")
	}
	if got.ID != "" {
		t.Errorf("owner for an unknown lease = %+v, want the zero Principal", got)
	}
}

// TestLeaseOwner_SurvivesLifecycleTransitions is the story's core guarantee: the
// owner is orthogonal to the lifecycle enum, so no transition may clear it. It
// walks pending → registered → active (plus the activity/session stamps that
// happen along the way) and re-reads the owner at every step.
func TestLeaseOwner_SurvivesLifecycleTransitions(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)

	id, err := reg.NewLease(testOwner())
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}

	assertOwner := func(stage string, wantState leaseState) {
		t.Helper()
		reg.mu.Lock()
		gotState := reg.leases[id].state
		reg.mu.Unlock()
		if gotState != wantState {
			t.Fatalf("%s: lease state = %q, want %q", stage, gotState, wantState)
		}
		got, ok := reg.LeaseOwner(id)
		if !ok {
			t.Fatalf("%s: LeaseOwner reported the lease unknown", stage)
		}
		if got.ID != "ci-runner" {
			t.Fatalf("%s: owner ID = %q, want ci-runner (no transition may clear the owner)", stage, got.ID)
		}
	}

	assertOwner("pending", leaseStatePending)

	instance := newWSConn(nil)
	if err := reg.AttachInstance(id, instance, "", false); err != nil {
		t.Fatalf("AttachInstance: %v", err)
	}
	assertOwner("registered", leaseStateRegistered)

	client := newWSConn(nil)
	if err := reg.AttachClient(id, client); err != nil {
		t.Fatalf("AttachClient: %v", err)
	}
	assertOwner("active", leaseStateActive)

	reg.markActivity(id)
	reg.MarkReady(id)
	reg.MarkSessionID(id, "engine-sess-1")
	assertOwner("after ready/session/activity stamps", leaseStateActive)

	// Detaching the client drops back to registered; the owner is still the
	// claimant, which is what lets a reconnect be checked against it.
	reg.DetachClient(id, client)
	assertOwner("client detached", leaseStateRegistered)

	// The releasing latch is not a lifecycle state but must not clear the owner
	// either: a teardown-in-progress lease is still owned while it drains.
	reg.mu.Lock()
	reg.leases[id].releasing = true
	reg.mu.Unlock()
	assertOwner("draining", leaseStateRegistered)
}

// TestLeaseOwner_NoInternalStateEscapes holds the accessor to Snapshot's
// discipline: values out, no internal slice or map escaping. Both directions
// matter — a caller must not be able to rewrite a lease's owner by mutating the
// Principal it passed in, nor the one it got back.
func TestLeaseOwner_NoInternalStateEscapes(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)

	in := testOwner()
	id, err := reg.NewLease(in)
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}

	// Mutating the Principal handed to NewLease must not reach the lease.
	in.Scopes[0] = "broker.admin"
	in.Claims["iss"] = "https://attacker.example"

	got, ok := reg.LeaseOwner(id)
	if !ok {
		t.Fatal("LeaseOwner reported the lease unknown")
	}
	if got.Scopes[0] != "broker.claim" {
		t.Errorf("owner Scopes[0] = %q, want broker.claim: the caller's slice reached the lease", got.Scopes[0])
	}
	if got.Claims["iss"] != "https://idp.example" {
		t.Errorf("owner Claims[iss] = %v, want the stamped value: the caller's map reached the lease", got.Claims["iss"])
	}

	// Mutating the returned Principal must not reach the lease either.
	got.Scopes[0] = "broker.admin"
	got.Claims["iss"] = "https://attacker.example"

	again, _ := reg.LeaseOwner(id)
	if again.Scopes[0] != "broker.claim" {
		t.Errorf("owner Scopes[0] = %q after mutating a returned copy, want broker.claim", again.Scopes[0])
	}
	if again.Claims["iss"] != "https://idp.example" {
		t.Errorf("owner Claims[iss] = %v after mutating a returned copy, want the stamped value", again.Claims["iss"])
	}
}

// TestLeaseOwner_NilReferenceFieldsAreSafe covers the shape a static token
// produces (an ID and nothing else): cloning must leave nil fields nil rather
// than materialising empty ones, so a persisted or logged owner does not gain
// fields the credential never carried.
func TestLeaseOwner_NilReferenceFieldsAreSafe(t *testing.T) {
	reg := NewRegistry(testLogger(), 0)

	id, err := reg.NewLease(nexusauth.Principal{ID: "static-caller"})
	if err != nil {
		t.Fatalf("NewLease: %v", err)
	}

	got, ok := reg.LeaseOwner(id)
	if !ok {
		t.Fatal("LeaseOwner reported the lease unknown")
	}
	if got.ID != "static-caller" {
		t.Errorf("owner ID = %q, want static-caller", got.ID)
	}
	if got.Scopes != nil || got.Claims != nil {
		t.Errorf("owner = %+v, want nil Scopes and Claims preserved", got)
	}
	if got.HasScope("broker.claim") {
		t.Error("a principal with no scopes must not report holding one")
	}
}
