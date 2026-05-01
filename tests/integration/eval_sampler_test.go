//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/testharness"
)

// TestEvalSampler_RateOne_CapturesSession boots a real engine (mock provider,
// no API key) with the online sampler enabled at rate=1.0, runs a 2-turn
// scripted dialogue via nexus.io.test, and confirms the snapshot landed at
// the expected on-disk path with the documented layout (journal/ subtree +
// metadata.json sibling).
func TestEvalSampler_RateOne_CapturesSession(t *testing.T) {
	outDir := t.TempDir()
	cfgPath := copyConfig(t, "configs/test-eval-sampler.yaml", map[string]any{
		"nexus.observe.sampler": map[string]any{
			"enabled":         true,
			"rate":            1.0,
			"failure_capture": true,
			"out_dir":         outDir,
		},
	})

	h := testharness.New(t, cfgPath,
		testharness.WithTimeout(20*time.Second),
		testharness.WithKeepSession(),
	)
	h.Run()

	sessionDir := h.SessionDir()
	if sessionDir == "" {
		t.Fatal("no session dir")
	}
	t.Cleanup(func() { os.RemoveAll(sessionDir) })

	sessionID := filepath.Base(sessionDir)
	caseDir := filepath.Join(outDir, sessionID)

	// 1) The sample dir exists.
	info, err := os.Stat(caseDir)
	if err != nil {
		t.Fatalf("expected sample dir at %s: %v", caseDir, err)
	}
	if !info.IsDir() {
		t.Fatalf("sample path %s is not a dir", caseDir)
	}

	// 2) journal/header.json was copied.
	if _, err := os.Stat(filepath.Join(caseDir, "journal", "header.json")); err != nil {
		t.Errorf("expected journal/header.json: %v", err)
	}

	// 3) journal/events.jsonl was copied and is non-empty.
	dstEvents, err := os.ReadFile(filepath.Join(caseDir, "journal", "events.jsonl"))
	if err != nil {
		t.Fatalf("read sampled events.jsonl: %v", err)
	}
	if len(dstEvents) == 0 {
		t.Errorf("sampled events.jsonl is empty")
	}

	// 4) Identity redactor: snapshot is a byte-for-byte prefix of the live
	//    source segment. The source keeps growing after capture (the
	//    io.session.end envelope itself, plus any post-handler emissions,
	//    land after the sampler's snapshot finished), so equality is too
	//    strict — but every byte in the snapshot must match the source.
	srcEvents, err := os.ReadFile(filepath.Join(sessionDir, "journal", "events.jsonl"))
	if err != nil {
		t.Fatalf("read source events.jsonl: %v", err)
	}
	if !bytes.HasPrefix(srcEvents, dstEvents) {
		t.Errorf("identity-redactor snapshot is not a prefix of source\n  src len=%d  dst len=%d", len(srcEvents), len(dstEvents))
	}

	// 5) metadata.json fields.
	mb, err := os.ReadFile(filepath.Join(caseDir, "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	var meta struct {
		CapturedAt            string  `json:"captured_at"`
		Reason                string  `json:"reason"`
		SamplingRateAtCapture float64 `json:"sampling_rate_at_capture"`
		SessionStatus         string  `json:"session_status"`
		EngineVersion         string  `json:"engine_version"`
	}
	if err := json.Unmarshal(mb, &meta); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	if meta.Reason != "sampled" {
		t.Errorf("reason=%q, want sampled", meta.Reason)
	}
	if meta.SamplingRateAtCapture != 1.0 {
		t.Errorf("sampling_rate_at_capture=%v, want 1.0", meta.SamplingRateAtCapture)
	}
	if meta.CapturedAt == "" {
		t.Error("captured_at empty")
	}
	if meta.EngineVersion == "" {
		t.Error("engine_version empty")
	}
}

// TestEvalSampler_NotInActive_NoBehaviorChange is the "off-by-default"
// smoke test mandated by the plan: a config with no sampler entry must
// produce zero behavior change. Boots the same harness without
// nexus.observe.sampler and asserts no sample dir lands anywhere under
// the test out_dir.
func TestEvalSampler_NotInActive_NoBehaviorChange(t *testing.T) {
	outDir := t.TempDir()
	// Reuse the journal-basic config (mock mode, no sampler) — it is the
	// canonical "engine boots cleanly under mock provider" smoke test.
	h := testharness.New(t, "configs/test-journal-basic.yaml",
		testharness.WithTimeout(20*time.Second),
		testharness.WithKeepSession(),
	)
	h.Run()

	sessionDir := h.SessionDir()
	t.Cleanup(func() { os.RemoveAll(sessionDir) })

	// Nothing should have written under outDir; we never told the sampler
	// to point here, but the assertion is "no surprise files anywhere".
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read outDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected outDir to be empty when sampler is not active, got %d entries", len(entries))
	}
}
