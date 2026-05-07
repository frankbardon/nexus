package compat

import (
	"errors"
	"testing"
)

func TestRegistryStartsEmpty(t *testing.T) {
	if len(Migrations) != 0 {
		t.Fatalf("expected empty registry at v1; got %d entries", len(Migrations))
	}
}

func TestApplySameVersionPassesThrough(t *testing.T) {
	in := map[string]any{"foo": "bar"}
	out, err := Apply("llm.request", 1, 1, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["foo"] != "bar" {
		t.Fatalf("expected payload unchanged; got %v", out)
	}
}

func TestApplyEmptyRegistryReturnsPayloadUnchanged(t *testing.T) {
	// With no registered migrators, a zero-versioned payload (the v0==v1
	// case for journal records written before the field existed) flows
	// through Apply unchanged. The doc.go rule promises this.
	in := map[string]any{"_schema_version": 0, "content": "hello"}
	out, err := Apply("io.input", 0, 1, in)
	if err != nil {
		t.Fatalf("unexpected error on empty registry: %v", err)
	}
	if out["content"] != "hello" {
		t.Fatalf("expected payload unchanged; got %v", out)
	}
}

func TestApplyDowngradeRejected(t *testing.T) {
	_, err := Apply("llm.request", 2, 1, map[string]any{})
	if err == nil {
		t.Fatal("expected error on downgrade; got nil")
	}
}

// TestApplyChainsMigrators registers an example migration table to exercise
// the chained 1->2->3 path. The table is tear-down so the live registry
// stays empty after the test.
func TestApplyChainsMigrators(t *testing.T) {
	const evt = "test.event"
	defer func() {
		delete(Migrations, Key{EventType: evt, From: 1, To: 2})
		delete(Migrations, Key{EventType: evt, From: 2, To: 3})
	}()

	Migrations[Key{EventType: evt, From: 1, To: 2}] = func(p map[string]any) (map[string]any, error) {
		p["v2_field"] = "added"
		return p, nil
	}
	Migrations[Key{EventType: evt, From: 2, To: 3}] = func(p map[string]any) (map[string]any, error) {
		p["v3_field"] = "added"
		return p, nil
	}

	in := map[string]any{"v1_field": "original"}
	out, err := Apply(evt, 1, 3, in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["v1_field"] != "original" {
		t.Errorf("expected v1_field preserved; got %v", out["v1_field"])
	}
	if out["v2_field"] != "added" {
		t.Errorf("expected v2 step applied; got %v", out["v2_field"])
	}
	if out["v3_field"] != "added" {
		t.Errorf("expected v3 step applied; got %v", out["v3_field"])
	}
}

func TestApplyMissingIntermediateStepFailsLoud(t *testing.T) {
	const evt = "test.gappy"
	defer delete(Migrations, Key{EventType: evt, From: 1, To: 2})

	// Register only 1->2, then ask for 1->3. The missing 2->3 should
	// surface as an error rather than a silent no-op, since at least one
	// migrator exists for this event type.
	Migrations[Key{EventType: evt, From: 1, To: 2}] = func(p map[string]any) (map[string]any, error) {
		return p, nil
	}

	_, err := Apply(evt, 1, 3, map[string]any{})
	if err == nil {
		t.Fatal("expected error on missing intermediate step; got nil")
	}
}

func TestApplyMigratorErrorIsWrapped(t *testing.T) {
	const evt = "test.fail"
	defer delete(Migrations, Key{EventType: evt, From: 1, To: 2})

	sentinel := errors.New("boom")
	Migrations[Key{EventType: evt, From: 1, To: 2}] = func(p map[string]any) (map[string]any, error) {
		return nil, sentinel
	}

	_, err := Apply(evt, 1, 2, map[string]any{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel; got %v", err)
	}
}
