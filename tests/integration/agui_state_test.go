//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/agui/aguiclient"
)

// agui_state_test.go proves the bidirectional AG-UI shared-state feature
// (E3-S1/S2/S3) end to end through the pure-Go conformance client, with mocked
// LLM responses (no API key). The transport mirrors the session's scene store as
// an AG-UI shared-state document keyed by scene_id: a StateSnapshot rides the run
// right after RunStarted and every scene mutation emits an ordered StateDelta
// (RFC 6902 JSON Patch). The client reconstructs the final document by applying
// the snapshot + deltas in order.

// reconstructState replays the run's StateSnapshot followed by every StateDelta
// (in stream order) via pkg/agui.ApplyPatch and returns the resulting document.
// It fails the test when the run carried no StateSnapshot, since the snapshot is
// the required base of the shared-state stream.
func reconstructState(t *testing.T, res aguiclient.Result) json.RawMessage {
	t.Helper()
	snapEv := res.First(agui.EventStateSnapshot)
	if snapEv == nil {
		t.Fatalf("stream carried no StateSnapshot: %v", res.Types())
	}
	snap, ok := snapEv.(*agui.StateSnapshotEvent)
	if !ok {
		t.Fatalf("StateSnapshot event has unexpected type %T", snapEv)
	}

	doc := json.RawMessage(snap.Snapshot)
	if len(doc) == 0 {
		doc = json.RawMessage("{}")
	}
	for _, e := range res.Events {
		delta, ok := e.(*agui.StateDeltaEvent)
		if !ok {
			continue
		}
		out, err := agui.ApplyPatch(doc, delta.Delta)
		if err != nil {
			t.Fatalf("apply StateDelta %v: %v", delta.Delta, err)
		}
		doc = out
	}
	return doc
}

// assertJSONEqual fails when got does not decode to the same JSON value as want.
func assertJSONEqual(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var gv, wv any
	if err := json.Unmarshal(got, &gv); err != nil {
		t.Fatalf("decode got %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &wv); err != nil {
		t.Fatalf("decode want %s: %v", want, err)
	}
	gb, _ := json.Marshal(gv)
	wb, _ := json.Marshal(wv)
	if string(gb) != string(wb) {
		t.Fatalf("state mismatch:\n got  %s\n want %s", gb, wb)
	}
}

// TestAGUIState_OutboundSnapshotDeltas drives a run whose mock agent mutates the
// scene store (scene_create then scene_patch). The client must receive an initial
// StateSnapshot followed by ordered StateDeltas, and applying snapshot + deltas
// must reconstruct the final shared-state document.
func TestAGUIState_OutboundSnapshotDeltas(t *testing.T) {
	bootEngine(t, "configs/test-agui-state.yaml")
	waitForListener(t, aguiBindAddr)

	c := aguiclient.New("http://" + aguiBindAddr + "/agui")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := c.Run(ctx, aguiclient.UserMessage("thread-state", "run-state", "Build the board."))
	if err != nil {
		t.Fatalf("agui run: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	types := res.Types()
	if types[0] != agui.EventRunStarted {
		t.Fatalf("event[0] = %s, want RunStarted", types[0])
	}

	// The initial StateSnapshot must ride the run right after RunStarted, and the
	// two scene mutations must each surface a StateDelta in order.
	assertOrderedSubsequence(t, types,
		agui.EventRunStarted,
		agui.EventStateSnapshot,
		agui.EventStateDelta,
		agui.EventStateDelta,
		agui.EventRunFinished,
	)

	// The snapshot precedes every delta (a client cannot apply a delta without its
	// base document).
	snapIdx, firstDeltaIdx := -1, -1
	for i, tp := range types {
		if tp == agui.EventStateSnapshot && snapIdx < 0 {
			snapIdx = i
		}
		if tp == agui.EventStateDelta && firstDeltaIdx < 0 {
			firstDeltaIdx = i
		}
	}
	if snapIdx < 0 || firstDeltaIdx < 0 || snapIdx > firstDeltaIdx {
		t.Fatalf("StateSnapshot (idx %d) must precede first StateDelta (idx %d): %v",
			snapIdx, firstDeltaIdx, types)
	}

	// The initial snapshot must be the empty document — no scene exists yet at run
	// start; state is built entirely from the ordered deltas.
	snap := res.First(agui.EventStateSnapshot).(*agui.StateSnapshotEvent)
	assertJSONEqual(t, snap.Snapshot, "{}")

	if got := res.Count(agui.EventStateDelta); got != 2 {
		t.Fatalf("StateDelta count = %d, want 2 (create + patch)", got)
	}

	// Snapshot + deltas reconstructs the final document: the created board with the
	// patched status shallow-merged in.
	final := reconstructState(t, res)
	assertJSONEqual(t, final,
		`{"board":{"title":"Sprint","tasks":["design"],"status":"active"}}`)
}

// TestAGUIState_InboundSeedObserved sends a client-authored state document on
// RunAgentInput.state. The seed must (a) be reflected in the initial StateSnapshot
// and (b) be observable by the agent within the run — the mock agent's scene_get
// ToolCallResult must carry the seeded content, proving the store adopted it.
func TestAGUIState_InboundSeedObserved(t *testing.T) {
	bootEngine(t, "configs/test-agui-state-inbound.yaml")
	waitForListener(t, aguiBindAddr)

	c := aguiclient.New("http://" + aguiBindAddr + "/agui")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	input := aguiclient.UserMessage("thread-inbound", "run-inbound", "Read the board.")
	input.State = json.RawMessage(`{"board":{"title":"Seeded Board","phase":"kickoff"}}`)

	res, err := c.Run(ctx, input)
	if err != nil {
		t.Fatalf("agui run: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	// (a) The initial StateSnapshot reflects the client seed.
	snapEv := res.First(agui.EventStateSnapshot)
	if snapEv == nil {
		t.Fatalf("no StateSnapshot in stream: %v", res.Types())
	}
	snap := snapEv.(*agui.StateSnapshotEvent)
	assertJSONEqual(t, snap.Snapshot,
		`{"board":{"title":"Seeded Board","phase":"kickoff"}}`)

	// (b) The agent observed the seed: its scene_get result carries the seeded
	// content. The seed is applied before io.input, so scene_get finds the scene.
	var toolContent string
	for _, e := range res.Events {
		if tr, ok := e.(*agui.ToolCallResultEvent); ok {
			toolContent += tr.Content
		}
	}
	if toolContent == "" {
		t.Fatalf("no ToolCallResult content captured: %v", res.Types())
	}
	if !contains(toolContent, "Seeded Board") {
		t.Fatalf("scene_get result did not carry the client seed; got %q", toolContent)
	}

	// The seed itself produces no StateDelta (the mirror was pre-seeded to the same
	// value the scene store echoes back), and scene_get does not mutate state.
	if got := res.Count(agui.EventStateDelta); got != 0 {
		t.Fatalf("StateDelta count = %d, want 0 (seed + read-only run emits none)", got)
	}

	// Snapshot + (no) deltas still reconstructs the seeded document.
	assertJSONEqual(t, reconstructState(t, res),
		`{"board":{"title":"Seeded Board","phase":"kickoff"}}`)
}

// TestAGUIState_ConflictAgentWins proves the documented
// "client-state-seeds-then-agent-wins" conflict semantics. The client seeds
// scene "board", then the agent patches the SAME scene_id. Last-writer wins on
// the overlapping key while the scene store's shallow-merge preserves the client
// key the agent did not touch — and the mutation flows back as a StateDelta.
func TestAGUIState_ConflictAgentWins(t *testing.T) {
	bootEngine(t, "configs/test-agui-state-conflict.yaml")
	waitForListener(t, aguiBindAddr)

	c := aguiclient.New("http://" + aguiBindAddr + "/agui")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	input := aguiclient.UserMessage("thread-conflict", "run-conflict", "Rename the board.")
	input.State = json.RawMessage(`{"board":{"title":"Client","owner":"alice"}}`)

	res, err := c.Run(ctx, input)
	if err != nil {
		t.Fatalf("agui run: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	// The initial snapshot carries the client seed (applied before io.input).
	snap := res.First(agui.EventStateSnapshot).(*agui.StateSnapshotEvent)
	assertJSONEqual(t, snap.Snapshot, `{"board":{"title":"Client","owner":"alice"}}`)

	// The agent's patch on the same scene_id emits exactly one StateDelta (the seed
	// itself is silent).
	if got := res.Count(agui.EventStateDelta); got != 1 {
		t.Fatalf("StateDelta count = %d, want 1 (agent patch only)", got)
	}

	// Agent wins on the overlapping "title" key; the client's "owner" is preserved
	// by shallow-merge. Snapshot + delta reconstructs the merged document.
	final := reconstructState(t, res)
	assertJSONEqual(t, final, `{"board":{"title":"Agent","owner":"alice"}}`)
}
