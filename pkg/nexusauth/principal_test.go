package nexusauth

import "testing"

func TestPrincipalHasScope(t *testing.T) {
	p := Principal{ID: "u", Scopes: []string{"leases.read", "admin"}}
	if !p.HasScope("admin") {
		t.Fatal("granted scope not found")
	}
	if p.HasScope("Admin") {
		t.Fatal("scope match must be case-sensitive")
	}
	if p.HasScope("leases") {
		t.Fatal("scope match must be exact, not a prefix")
	}
	if p.HasScope("") {
		t.Fatal("empty scope must never match")
	}
	if (Principal{ID: "u"}).HasScope("admin") {
		t.Fatal("principal with no scopes granted one")
	}
}

func TestPrincipalCloneIsDeep(t *testing.T) {
	src := Principal{
		ID:     "u",
		Scopes: []string{"admin"},
		Claims: map[string]any{"sub": "u"},
	}
	dst := src.clone()
	dst.Scopes[0] = "hacked"
	dst.Claims["sub"] = "hacked"
	if src.Scopes[0] != "admin" {
		t.Fatal("clone aliased Scopes")
	}
	if src.Claims["sub"] != "u" {
		t.Fatal("clone aliased Claims")
	}

	// A nil slice/map must stay nil rather than become an empty allocation, so
	// "carries no scopes" stays distinguishable from "carries none granted".
	bare := Principal{ID: "u"}.clone()
	if bare.Scopes != nil || bare.Claims != nil {
		t.Fatalf("clone materialized empty containers: %+v", bare)
	}
}
