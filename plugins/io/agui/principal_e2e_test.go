package agui

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
)

// This file is E2-S2: end-to-end coverage of the E2-S1 identity bind/clear
// through the REAL AG-UI transport (actual HTTP handler + SSE stream over a
// real engine bus and a real SessionWorkspace), as opposed to
// contract_test.go's bus-level TestContract_* assertions, which drive
// startRun/resumeRun directly without a socket in between.

// newSessionTestPlugin boots the plugin exactly like newTestPlugin
// (roundtrip_test.go) but additionally wires a real *engine.SessionWorkspace,
// so bindSessionContext's SetReservedLabel/SetLabel calls land in
// SessionMeta.Labels and announce session.tag.set/deleted on the bus — the
// thing newTestPlugin's session-less plugin cannot exercise. cfg may be nil;
// "bind" is always overwritten with a fresh loopback port.
func newSessionTestPlugin(t *testing.T, cfg map[string]any) (*Plugin, engine.EventBus, *engine.SessionWorkspace, string) {
	t.Helper()
	if cfg == nil {
		cfg = map[string]any{}
	}
	bus := engine.NewEventBus()
	addr := freeAddr(t)
	cfg["bind"] = addr

	session, err := engine.NewSessionWorkspace(filepath.Join(t.TempDir(), "sessions"), bus)
	if err != nil {
		t.Fatalf("new session workspace: %v", err)
	}

	p := New().(*Plugin)
	ctx := engine.PluginContext{
		Config:  cfg,
		Bus:     bus,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Session: session,
	}
	if err := p.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := p.Ready(); err != nil {
		t.Fatalf("ready: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(t.Context()) })

	url := "http://" + addr + agentPath
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodOptions, url, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			return p, bus, session, url
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("server did not come up")
	return nil, nil, nil, ""
}

// staticAuthConfig builds an `auth:` block with one static validator mapping
// each token to its principal, the same shape TestAuthBlockStaticValidator
// (auth_test.go) already proves is wired end to end.
func staticAuthConfig(tokenToPrincipal map[string]string) map[string]any {
	var tokens []any
	for token, principal := range tokenToPrincipal {
		tokens = append(tokens, map[string]any{"token": token, "principal": principal})
	}
	return map[string]any{
		"auth": map[string]any{
			"validators": []any{map[string]any{
				"type":   "static",
				"tokens": tokens,
			}},
		},
	}
}

// TestE2E_RunAgentInputBindsPrincipalBeforeFirstAgentEventAndClearsOnEnd is
// the core E2-S2 acceptance test: a real POST RunAgentInput, authenticated as
// a real principal through the real auth chain, must announce
// session.tag.set{Key: "_principal_id"} before the run's first agent-turn
// event, and session.tag.deleted{Key: "_principal_id"} once the run ends.
func TestE2E_RunAgentInputBindsPrincipalBeforeFirstAgentEventAndClearsOnEnd(t *testing.T) {
	p, bus, session, url := newSessionTestPlugin(t, staticAuthConfig(map[string]string{
		"tok-e2e": "principal-e2e",
	}))

	var mu sync.Mutex
	var order []string
	var deletedKeys []string
	setValues := map[string]string{}

	bus.Subscribe("session.tag.set", func(e engine.Event[any]) {
		if s, ok := e.Payload.(events.SessionTagSet); ok {
			mu.Lock()
			order = append(order, "tag.set:"+s.Key)
			if _, exists := setValues[s.Key]; !exists {
				setValues[s.Key] = s.Value
			}
			mu.Unlock()
		}
	})
	bus.Subscribe("session.tag.deleted", func(e engine.Event[any]) {
		if d, ok := e.Payload.(events.SessionTagDeleted); ok {
			mu.Lock()
			deletedKeys = append(deletedKeys, d.Key)
			mu.Unlock()
		}
	})
	bus.Subscribe("agent.turn.start", func(engine.Event[any]) {
		mu.Lock()
		order = append(order, "turn.start")
		mu.Unlock()
	})

	inputSeen := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case inputSeen <- in:
			default:
			}
		}
	})

	go func() {
		<-inputSeen
		turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-e2e"}
		_ = bus.Emit("agent.turn.start", turn)
		_ = bus.Emit("io.output", events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "hi", Role: "assistant"})
		_ = bus.Emit("agent.turn.end", turn)
	}()

	body := `{"threadId":"thread-e2e","runId":"run-e2e","messages":[{"id":"m1","role":"user","content":"hello"}]}`
	resp := post(t, url, "tok-e2e", "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	evs, err := agui.NewSSEReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatalf("read sse: %v", err)
	}
	if len(evs) == 0 || evs[len(evs)-1].EventType() != agui.EventRunFinished {
		t.Fatalf("last event = %v, want RunFinished", eventTypes(evs))
	}

	// handleRunAgent's endRun runs in a defer as the handler returns; the SSE
	// read completing races that defer, so wait for the slot to actually free
	// before asserting the post-run clear.
	waitFor(t, func() bool { return p.currentRun() == nil })

	mu.Lock()
	defer mu.Unlock()

	if setValues[reservedPrincipalIDKey] != "principal-e2e" {
		t.Errorf("_principal_id session.tag.set value = %q, want principal-e2e", setValues[reservedPrincipalIDKey])
	}

	setIdx, turnIdx := -1, -1
	for i, name := range order {
		if name == "tag.set:"+reservedPrincipalIDKey && setIdx == -1 {
			setIdx = i
		}
		if name == "turn.start" && turnIdx == -1 {
			turnIdx = i
		}
	}
	if setIdx == -1 {
		t.Fatalf("session.tag.set never fired for %q; order=%v", reservedPrincipalIDKey, order)
	}
	if turnIdx == -1 {
		t.Fatalf("agent.turn.start never observed; order=%v", order)
	}
	if setIdx >= turnIdx {
		t.Fatalf("session.tag.set(%d) did not precede agent.turn.start(%d); order=%v", setIdx, turnIdx, order)
	}

	if !slices.Contains(deletedKeys, reservedPrincipalIDKey) {
		t.Errorf("session.tag.deleted never fired for %q after run end", reservedPrincipalIDKey)
	}

	meta, err := session.SessionMetadata()
	if err != nil {
		t.Fatalf("session metadata: %v", err)
	}
	if _, ok := meta.Labels[reservedPrincipalIDKey]; ok {
		t.Errorf("_principal_id still present in session metadata after run end: %v", meta.Labels)
	}
}

// TestE2E_ResumeRebindsToNewPrincipalNotOriginal is modeled directly on
// resume_test.go's TestResumeCycle (interrupt -> resume across two runs on
// the same thread), but with the two runs authenticated as two DIFFERENT
// principals (swapping the bearer token between requests). It asserts the
// resume's bind reflects the NEW principal, never the one bound at the
// original, now-interrupted run.
func TestE2E_ResumeRebindsToNewPrincipalNotOriginal(t *testing.T) {
	p, bus, session, url := newSessionTestPlugin(t, staticAuthConfig(map[string]string{
		"tok-alice": "alice",
		"tok-bob":   "bob",
	}))

	var mu sync.Mutex
	var principalBindOrder []string

	bus.Subscribe("session.tag.set", func(e engine.Event[any]) {
		if s, ok := e.Payload.(events.SessionTagSet); ok && s.Key == reservedPrincipalIDKey {
			mu.Lock()
			principalBindOrder = append(principalBindOrder, s.Value)
			mu.Unlock()
		}
	})

	responded := make(chan events.HITLResponse, 1)
	bus.Subscribe("hitl.responded", func(e engine.Event[any]) {
		if r, ok := e.Payload.(events.HITLResponse); ok {
			select {
			case responded <- r:
			default:
			}
		}
	})
	inputSeen := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case inputSeen <- in:
			default:
			}
		}
	})

	// boundDuringResume captures Labels["_principal_id"] the instant
	// hitl.responded unblocks this goroutine — resumeRun's bindSessionContext
	// runs synchronously BEFORE it emits hitl.responded (see resume.go), and
	// the run cannot finish (and endRun cannot clear the label) until
	// agent.turn.end fires below, so this read is guaranteed to observe the
	// resume's own bind, not the cleared post-run state.
	boundDuringResume := make(chan string, 1)

	go func() {
		<-inputSeen
		turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-resume"}
		_ = bus.Emit("agent.turn.start", turn)
		_ = bus.Emit("hitl.requested", events.HITLRequest{
			SchemaVersion: events.HITLRequestVersion,
			ID:            "req-resume",
			SessionID:     "thread-resume",
			TurnID:        "turn-resume",
			Mode:          events.HITLModeChoices,
			Prompt:        "Approve deploy?",
			Choices: []events.HITLChoice{
				{ID: "allow", Label: "Allow", Kind: events.ChoiceAllow},
			},
		})
		<-responded
		if meta, err := session.SessionMetadata(); err == nil {
			boundDuringResume <- meta.Labels[reservedPrincipalIDKey]
		} else {
			boundDuringResume <- ""
		}
		_ = bus.Emit("io.output", events.AgentOutput{SchemaVersion: events.AgentOutputVersion, Content: "deploying", Role: "assistant"})
		_ = bus.Emit("agent.turn.end", turn)
	}()

	// --- Run 1, authenticated as alice: POST, read to the interrupt. ---
	body1 := `{"threadId":"thread-resume","runId":"run-1","messages":[{"id":"m1","role":"user","content":"deploy"}]}`
	resp1 := post(t, url, "tok-alice", "", body1)
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("run1 status = %d, want 200", resp1.StatusCode)
	}
	evs1, err := agui.NewSSEReader(resp1.Body).ReadAll()
	if err != nil {
		t.Fatalf("read run1 sse: %v", err)
	}
	var fin1 *agui.RunFinishedEvent
	for _, e := range evs1 {
		if fe, ok := e.(*agui.RunFinishedEvent); ok {
			fin1 = fe
		}
	}
	if fin1 == nil {
		t.Fatalf("run1 produced no RunFinished (events=%v)", eventTypes(evs1))
	}
	if fin1.Outcome != agui.OutcomeInterrupt {
		t.Fatalf("run1 outcome = %q, want interrupt", fin1.Outcome)
	}
	var ip agui.Interrupt
	if err := json.Unmarshal(fin1.Result, &ip); err != nil {
		t.Fatalf("decode run1 interrupt: %v", err)
	}
	if ip.InterruptID == "" {
		t.Fatal("run1 interrupt missing interruptId")
	}

	waitFor(t, func() bool { return p.currentRun() == nil })

	// Run 1's own endRun (deferred in handleRunAgent) fires as soon as its HTTP
	// handler returns, whether or not the outcome was an interrupt — clearing
	// _principal_id exactly as TestContract_EndRunClearsPrincipalIDLabel
	// asserts at the bus level. The bind is proven by principalBindOrder
	// (captured live via session.tag.set) below, not by inspecting Labels
	// between the two runs.
	meta, err := session.SessionMetadata()
	if err != nil {
		t.Fatalf("session metadata after run1: %v", err)
	}
	if _, ok := meta.Labels[reservedPrincipalIDKey]; ok {
		t.Errorf("_principal_id still present after run1's endRun: %v", meta.Labels)
	}

	// --- Run 2, authenticated as bob: resume the SAME thread's interrupt. ---
	body2 := `{"threadId":"thread-resume","runId":"run-2","resume":[{"interruptId":"` + ip.InterruptID +
		`","status":"resolved","payload":{"choiceId":"allow"}}]}`
	resp2 := post(t, url, "tok-bob", "", body2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("run2 status = %d, want 200", resp2.StatusCode)
	}
	evs2, err := agui.NewSSEReader(resp2.Body).ReadAll()
	if err != nil {
		t.Fatalf("read run2 sse: %v", err)
	}
	last := evs2[len(evs2)-1]
	fe2, ok := last.(*agui.RunFinishedEvent)
	if !ok {
		t.Fatalf("run2 last event = %s, want RunFinished", last.EventType())
	}
	if fe2.Outcome == agui.OutcomeInterrupt {
		t.Fatal("run2 should complete normally, not re-interrupt")
	}

	waitFor(t, func() bool { return p.currentRun() == nil })

	select {
	case bound := <-boundDuringResume:
		if bound != "bob" {
			t.Fatalf("_principal_id while run2 was in flight = %q, want bob (new principal, independent of run1's alice)", bound)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the resume's own bound-principal snapshot")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(principalBindOrder) < 2 {
		t.Fatalf("session.tag.set(_principal_id) fired %d times, want at least 2 (one per run); values=%v", len(principalBindOrder), principalBindOrder)
	}
	if principalBindOrder[0] != "alice" {
		t.Errorf("first _principal_id bind = %q, want alice", principalBindOrder[0])
	}
	if last := principalBindOrder[len(principalBindOrder)-1]; last != "bob" {
		t.Errorf("last _principal_id bind = %q, want bob (the resume's own resolved principal)", last)
	}
}

// TestE2E_ContextItemsProduceMatchingTagSetAnnouncements asserts a real
// RunAgentInput.Context payload produces one session.tag.set per item, keyed
// by the item's Description with the item's Value, over the real transport.
func TestE2E_ContextItemsProduceMatchingTagSetAnnouncements(t *testing.T) {
	_, bus, _, url := newSessionTestPlugin(t, nil)

	var mu sync.Mutex
	setValues := map[string]string{}
	bus.Subscribe("session.tag.set", func(e engine.Event[any]) {
		if s, ok := e.Payload.(events.SessionTagSet); ok {
			mu.Lock()
			setValues[s.Key] = s.Value
			mu.Unlock()
		}
	})

	inputSeen := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case inputSeen <- in:
			default:
			}
		}
	})
	go func() {
		<-inputSeen
		turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-ctx"}
		_ = bus.Emit("agent.turn.start", turn)
		_ = bus.Emit("agent.turn.end", turn)
	}()

	body := `{"threadId":"thread-ctx","runId":"run-ctx","messages":[{"id":"m1","role":"user","content":"hi"}],` +
		`"context":[{"description":"dataset","value":"prod"},{"description":"region","value":"us-east"}]}`
	resp := post(t, url, "", "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if _, err := agui.NewSSEReader(resp.Body).ReadAll(); err != nil {
		t.Fatalf("read sse: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if setValues["dataset"] != "prod" {
		t.Errorf("session.tag.set[dataset] = %q, want prod; saw %v", setValues["dataset"], setValues)
	}
	if setValues["region"] != "us-east" {
		t.Errorf("session.tag.set[region] = %q, want us-east; saw %v", setValues["region"], setValues)
	}
}

// TestE2E_UserInputPayloadUnchangedByPrincipalBinding is the regression guard
// against reintroducing the superseded "extend UserInput with PrincipalID"
// design. It has two halves:
//
//  1. A structural check independent of any test-constructed input: the
//     events.UserInput field set is pinned, and no principal-shaped field
//     exists on it at all.
//  2. A real end-to-end capture: a POST authenticated as a real principal
//     still emits an io.input payload with that exact shape — the principal
//     never rides UserInput, it rides the session tag store instead.
//
// Headers joined the pinned list when the X-Nexus-* convention landed, and it
// is worth saying why that is not the design this test rejects. Identity is a
// SERVER-RESOLVED fact: the auth chain decides it, nothing downstream may
// contradict it, and putting it on a payload any plugin can rewrite before the
// next handler sees it would make it forgeable. Headers is the opposite — an
// unverified statement the CALLER made about its own request, useful to a
// handler precisely as a hint. It rides the payload for the same reason
// Content does, and it is bound to the reserved label namespace too so a
// plugin outside the io.input path can still read it.
func TestE2E_UserInputPayloadUnchangedByPrincipalBinding(t *testing.T) {
	wantFields := []string{"SchemaVersion", "Content", "Files", "SessionID", "PreloadMessages", "Headers"}
	typ := reflect.TypeFor[events.UserInput]()
	if typ.NumField() != len(wantFields) {
		t.Fatalf("events.UserInput has %d fields, want %d (%v) — a field was added or removed", typ.NumField(), len(wantFields), wantFields)
	}
	for i, name := range wantFields {
		if got := typ.Field(i).Name; got != name {
			t.Errorf("events.UserInput field[%d] = %q, want %q", i, got, name)
		}
	}
	for field := range typ.Fields() {
		if strings.Contains(strings.ToLower(field.Name), "principal") {
			t.Errorf("events.UserInput gained a principal-shaped field %q; identity must ride the session tag store (session.tag.set), not UserInput", field.Name)
		}
	}

	_, bus, _, url := newSessionTestPlugin(t, staticAuthConfig(map[string]string{
		"tok-shape": "principal-shape",
	}))

	captured := make(chan events.UserInput, 1)
	trigger := make(chan events.UserInput, 1)
	bus.Subscribe("io.input", func(e engine.Event[any]) {
		if in, ok := e.Payload.(events.UserInput); ok {
			select {
			case captured <- in:
			default:
			}
			select {
			case trigger <- in:
			default:
			}
		}
	})

	go func() {
		<-trigger
		turn := events.TurnInfo{SchemaVersion: events.TurnInfoVersion, TurnID: "turn-shape"}
		_ = bus.Emit("agent.turn.start", turn)
		_ = bus.Emit("agent.turn.end", turn)
	}()

	body := `{"threadId":"thread-shape","runId":"run-shape","messages":[{"id":"m1","role":"user","content":"hi"}]}`
	resp := post(t, url, "tok-shape", "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if _, err := agui.NewSSEReader(resp.Body).ReadAll(); err != nil {
		t.Fatalf("read sse: %v", err)
	}

	var got events.UserInput
	select {
	case got = <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for io.input")
	}

	want := events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       "hi",
		SessionID:     "thread-shape",
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("io.input payload = %s, want %s (byte-for-byte unchanged shape)", gotJSON, wantJSON)
	}
	if strings.Contains(strings.ToLower(string(gotJSON)), "principal") {
		t.Errorf("io.input payload leaked the principal identity: %s", gotJSON)
	}
}
