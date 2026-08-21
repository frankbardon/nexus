package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/a2a"
)

// ---- the durable index ----

// TestA2AContextIndexRoundTripsBindings pins the basic contract: what is
// recorded is what a later process reads back.
func TestA2AContextIndexRoundTripsBindings(t *testing.T) {
	dir := t.TempDir()
	idx := mustOpenContextIndex(t, dir)
	idx.record("alice", "support", "ctx-1", "session-1")
	idx.record("", "research", "ctx-2", "session-2")
	if err := idx.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := mustOpenContextIndex(t, dir)
	if got, ok := reopened.lookup("alice", "support", "ctx-1"); !ok || got != "session-1" {
		t.Errorf("lookup(alice/support/ctx-1) = %q,%v; want session-1,true", got, ok)
	}
	if got, ok := reopened.lookup("", "research", "ctx-2"); !ok || got != "session-2" {
		t.Errorf("lookup(anonymous/research/ctx-2) = %q,%v; want session-2,true", got, ok)
	}
}

// TestA2AContextIndexKeysOnOwnerAndProfile is the security property: a contextId
// may be chosen by the client, so the same one under a different principal — or
// a different profile — must be a different conversation, not a shortcut into
// someone else's session.
func TestA2AContextIndexKeysOnOwnerAndProfile(t *testing.T) {
	idx := mustOpenContextIndex(t, t.TempDir())
	idx.record("alice", "support", "shared", "alice-session")

	if got, ok := idx.lookup("bob", "support", "shared"); ok {
		t.Errorf("bob resolved alice's context to %q; contexts must be principal-scoped", got)
	}
	if got, ok := idx.lookup("alice", "research", "shared"); ok {
		t.Errorf("the research profile resolved the support profile's context to %q", got)
	}
	if got, ok := idx.lookup("alice", "support", "shared"); !ok || got != "alice-session" {
		t.Errorf("alice lost her own binding: %q,%v", got, ok)
	}
}

// TestA2AContextIndexIgnoresIncompleteBindings pins that a record which answers
// nothing is never stored: an empty context or session would occupy one of the
// capped slots while being indistinguishable from "not recorded".
func TestA2AContextIndexIgnoresIncompleteBindings(t *testing.T) {
	idx := mustOpenContextIndex(t, t.TempDir())
	idx.record("alice", "support", "", "session-1")
	idx.record("alice", "support", "ctx-1", "")
	idx.record("alice", "", "ctx-2", "session-2")

	if _, ok := idx.lookup("alice", "support", "ctx-1"); ok {
		t.Error("a binding with no session id was stored")
	}
	if _, ok := idx.lookup("alice", "", "ctx-2"); ok {
		t.Error("a binding with no profile was stored")
	}
}

// TestA2AContextIndexSupersedesOnRebind pins that a conversation moved to a new
// session (an operator started it over) reads back as the NEW session, not both.
func TestA2AContextIndexSupersedesOnRebind(t *testing.T) {
	dir := t.TempDir()
	idx := mustOpenContextIndex(t, dir)
	idx.record("", "support", "ctx-1", "session-old")
	idx.record("", "support", "ctx-1", "session-new")
	if err := idx.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := mustOpenContextIndex(t, dir)
	if got, _ := reopened.lookup("", "support", "ctx-1"); got != "session-new" {
		t.Errorf("lookup = %q, want session-new", got)
	}
	// The rewrite-on-open pass deduplicates, so the file holds one line per
	// context rather than one per recording.
	if lines := countIndexLines(t, filepath.Join(dir, a2aContextIndexName)); lines != 1 {
		t.Errorf("index holds %d records for one context, want 1 after the open rewrite", lines)
	}
}

// TestA2AContextIndexPrunesOldestBindings pins the cap and its eviction order:
// the file is bounded, and what goes is the least recently recorded — degrading
// to "unknown", never to a wrong session.
func TestA2AContextIndexPrunesOldestBindings(t *testing.T) {
	dir := t.TempDir()
	idx := mustOpenContextIndex(t, dir)
	// A deterministic clock so recency is exercised without sleeps.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tick := 0
	idx.now = func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Second)
	}

	total := maxA2AContextBindings + 5
	for i := range total {
		idx.record("", "support", fmt.Sprintf("ctx-%05d", i), fmt.Sprintf("session-%05d", i))
	}
	idx.mu.Lock()
	err := idx.rewriteLocked()
	idx.mu.Unlock()
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	if _, ok := idx.lookup("", "support", "ctx-00000"); ok {
		t.Error("the oldest binding survived the cap")
	}
	if _, ok := idx.lookup("", "support", fmt.Sprintf("ctx-%05d", total-1)); !ok {
		t.Error("the newest binding was evicted")
	}
	if lines := countIndexLines(t, filepath.Join(dir, a2aContextIndexName)); lines != maxA2AContextBindings {
		t.Errorf("index holds %d records, want the cap %d", lines, maxA2AContextBindings)
	}
}

// TestA2AContextIndexSkipsATornTrailingRecord pins the crash tolerance: a broker
// killed mid-write must cost one binding, not the whole file and not the boot.
func TestA2AContextIndexSkipsATornTrailingRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, a2aContextIndexName)
	good, err := json.Marshal(a2aContextRecord{
		Profile: "support", ContextID: "ctx-1", SessionID: "session-1", At: time.Now(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// A complete record, then a record cut off mid-write.
	content := string(good) + "\n" + `{"profile":"support","context_id":"ctx-2","sess`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}

	idx := mustOpenContextIndex(t, dir)
	if got, ok := idx.lookup("", "support", "ctx-1"); !ok || got != "session-1" {
		t.Errorf("the intact record was lost: %q,%v", got, ok)
	}
	if _, ok := idx.lookup("", "support", "ctx-2"); ok {
		t.Error("a torn record was loaded")
	}
}

// TestA2ANilContextIndexIsUsable pins that a broker with no state_dir needs no
// branch anywhere: every method is nil-receiver safe and reports "unknown".
func TestA2ANilContextIndexIsUsable(t *testing.T) {
	var idx *a2aContextIndex
	idx.record("alice", "support", "ctx-1", "session-1")
	if _, ok := idx.lookup("alice", "support", "ctx-1"); ok {
		t.Error("a nil index answered a lookup")
	}
	if err := idx.Close(); err != nil {
		t.Errorf("closing a nil index: %v", err)
	}
}

// TestOpenA2AContextIndexWithoutStateDirIsNil pins that no file is created for a
// broker that configured no state_dir.
func TestOpenA2AContextIndexWithoutStateDirIsNil(t *testing.T) {
	idx, err := openA2AContextIndex(testLogger(), Config{})
	if err != nil {
		t.Fatalf("openA2AContextIndex: %v", err)
	}
	if idx != nil {
		t.Error("an index was opened for a broker with no state_dir")
	}
}

func countIndexLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

// ---- a classified spawn failure, seen by a client ----

// refusingLeaseProvider fails every acquisition with a classified outcome.
type refusingLeaseProvider struct{ err error }

func (p refusingLeaseProvider) Acquire(context.Context, a2aLeaseRequest) (a2aInstance, error) {
	return nil, p.err
}

// TestA2ASpawnFailureAnswersATerminalTask is the "never a hang" criterion seen
// from where it matters: the client sends a message, and gets back a TASK in a
// terminal state — in the vocabulary it already speaks — rather than a protocol
// error about broker machinery, and rather than an open stream.
func TestA2ASpawnFailureAnswersATerminalTask(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		state a2a.TaskState
	}{
		{"rejected", a2aRejectedSpawn("this broker refused to start an agent instance", nil), a2a.TaskStateRejected},
		{"failed", a2aFailedSpawn("an agent instance could not be started", nil), a2a.TaskStateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := a2aTestConfig(t, "")
			server, err := NewA2AServer(testLogger(), cfg)
			if err != nil {
				t.Fatalf("NewA2AServer: %v", err)
			}
			server.useLeaseProvider(refusingLeaseProvider{err: tc.err})
			ts, _ := newBrokerTestServer(t, cfg, server.Register)

			body := `{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":` +
				`{"messageId":"m1","role":"ROLE_USER","contextId":"ctx-1","parts":[{"text":"hello"}]}}}`
			resp := postJSONWithin(t, ts.URL+agentJSONRPCPath("support"), body, 5*time.Second)

			var envelope struct {
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(resp, &envelope); err != nil {
				t.Fatalf("response is not a JSON-RPC envelope: %v (%s)", err, resp)
			}
			if envelope.Error != nil {
				t.Fatalf("a classified spawn failure was answered with a protocol error: %s", envelope.Error)
			}
			var result a2a.SendMessageResponse
			if err := json.Unmarshal(envelope.Result, &result); err != nil {
				t.Fatalf("result is not a SendMessageResponse: %v (%s)", err, envelope.Result)
			}
			if result.Task == nil {
				t.Fatalf("no task in the result: %s", envelope.Result)
			}
			if result.Task.Status.State != tc.state {
				t.Errorf("state = %s, want %s", result.Task.Status.State, tc.state)
			}
			if result.Task.Status.Message == nil {
				t.Fatal("the terminal status carries no message explaining what happened")
			}
			text, _ := result.Task.Status.Message.Parts[0].TextValue()
			if !strings.Contains(text, "agent instance") {
				t.Errorf("status message %q does not explain the failure", text)
			}
			// And nothing about leases leaks into what the client is told.
			if strings.Contains(strings.ToLower(text), "lease") {
				t.Errorf("status message %q names a lease; an A2A client must never learn leases exist", text)
			}
		})
	}
}

// TestA2ASpawnFailureClosesAStream is the streaming half: the SSE response opens
// on an already-terminal snapshot and closes, rather than hanging.
func TestA2ASpawnFailureClosesAStream(t *testing.T) {
	cfg := a2aTestConfig(t, "")
	server, err := NewA2AServer(testLogger(), cfg)
	if err != nil {
		t.Fatalf("NewA2AServer: %v", err)
	}
	server.useLeaseProvider(refusingLeaseProvider{
		err: a2aFailedSpawn("an agent instance could not be started", nil),
	})
	ts, _ := newBrokerTestServer(t, cfg, server.Register)

	body := `{"jsonrpc":"2.0","id":2,"method":"SendStreamingMessage","params":{"message":` +
		`{"messageId":"m1","role":"ROLE_USER","contextId":"ctx-1","parts":[{"text":"hello"}]}}}`
	resp := postSSEWithin(t, ts.URL+agentJSONRPCPath("support"), body, 5*time.Second)
	if len(resp) == 0 {
		t.Fatal("the stream carried nothing")
	}
	if !strings.Contains(resp, a2a.TaskStateFailed.String()) {
		t.Errorf("the stream never reported a terminal state:\n%s", resp)
	}
}

// postJSONWithin posts a JSON-RPC body and returns the response, FAILING the
// test if the broker takes longer than within to answer.
//
// The timeout is the assertion, not a convenience: "never a hang" is the
// property under test, and a test that simply blocks forever reports a hang as
// a suite timeout with no attribution.
func postJSONWithin(t *testing.T, url, body string, within time.Duration) []byte {
	t.Helper()
	client := &http.Client{Timeout: within}
	resp, err := client.Post(url, a2a.ContentTypeJSON, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return raw
}

// postSSEWithin posts a streaming request and returns the whole stream once the
// server closes it, failing the test if it does not close inside within.
func postSSEWithin(t *testing.T, url, body string, within time.Duration) string {
	t.Helper()
	client := &http.Client{Timeout: within}
	resp, err := client.Post(url, a2a.ContentTypeJSON, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return string(raw)
}
