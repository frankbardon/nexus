//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/cmd/nexus-broker/testdata/stubcore"
	"github.com/frankbardon/nexus/pkg/a2a"
)

// This file proves the A2A lifecycle end to end against REAL spawned processes:
// an A2A message boots an isolated instance, a later message on the same
// contextId reaches the same instance, and a message after the instance is gone
// re-spawns it with -recall so history is replayed. The client sends nothing but
// A2A and is never told a lease exists.
//
// The instance is the same stub the claim suite uses (testdata/stubinstance), so
// no LLM and no API key are involved. Its answer to a turn carries three facts —
// variant, session id, and whether it was started with -recall — which is what
// makes "the second spawn resumed the first one's session" assertable from the
// client's side of the wire alone.

// a2aStubProfileName is the profile the fixtures publish. It is also a path
// segment, so it obeys validateProfileName.
const a2aStubProfileName = "support"

// startA2AStubBroker boots a stub broker with one A2A profile and returns its
// address alongside the shared registry.
func startA2AStubBroker(t *testing.T, opts ...stubBrokerOption) (string, *Registry) {
	t.Helper()
	stubBin := buildStubInstance(t)

	configPath := filepath.Join(t.TempDir(), "support.yaml")
	if err := os.WriteFile(configPath, []byte("engine:\n  name: stub\n"), 0o600); err != nil {
		t.Fatalf("write profile config: %v", err)
	}

	profile := AgentProfile{
		Binary:         reservedBinaryName,
		Config:         configPath,
		ResolvedConfig: configPath,
		Card: AgentCardSpec{
			Name:        "Support Agent",
			Description: "Answers support questions.",
			Version:     "0.1.0",
			Skills: []AgentCardSkill{{
				ID:          "answer",
				Name:        "Answer",
				Description: "Answers a question.",
			}},
		},
	}
	all := append([]stubBrokerOption{
		withAgents(map[string]AgentProfile{a2aStubProfileName: profile}),
	}, opts...)
	return startStubBrokerWithRegistry(t, stubBin, all...)
}

// sendA2AMessage posts a blocking SendMessage over the JSON-RPC binding and
// returns the resulting Task.
//
// It uses a bounded HTTP client on purpose: "never a hang" is a property under
// test here, and a request with no deadline would report a hang as a suite
// timeout with no attribution.
func sendA2AMessage(t *testing.T, base, contextID, text string) a2a.Task {
	t.Helper()
	body := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"SendMessage","params":{"message":`+
			`{"messageId":%q,"role":"ROLE_USER","contextId":%q,"parts":[{"text":%q}]}}}`,
		"m-"+contextID+"-"+text, contextID, text)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post("http://"+base+agentJSONRPCPath(a2aStubProfileName), a2a.ContentTypeJSON, strings.NewReader(body))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read SendMessage response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SendMessage status = %d: %s", resp.StatusCode, raw)
	}

	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("response is not a JSON-RPC envelope: %v (%s)", err, raw)
	}
	if envelope.Error != nil {
		t.Fatalf("SendMessage was refused: %s", envelope.Error)
	}
	var result a2a.SendMessageResponse
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		t.Fatalf("result is not a SendMessageResponse: %v (%s)", err, envelope.Result)
	}
	if result.Task == nil {
		t.Fatalf("SendMessage produced no task: %s", envelope.Result)
	}
	return *result.Task
}

// answerOf extracts the turn's answer text from a completed task.
func answerOf(t *testing.T, task a2a.Task) string {
	t.Helper()
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("task state = %s, want COMPLETED (status message: %+v)", task.Status.State, task.Status.Message)
	}
	if len(task.Artifacts) != 1 {
		t.Fatalf("artifacts = %d, want the single response artifact", len(task.Artifacts))
	}
	text, ok := task.Artifacts[0].Parts[0].TextValue()
	if !ok {
		t.Fatal("the response artifact carries no text")
	}
	return text
}

// TestA2AColdSpawnReusesAndResumes is the whole story in one run:
//
//	message 1 on a new context  → an instance is spawned, fresh session
//	message 2 on the same one   → the SAME instance answers, no second spawn
//	the instance is released    → the conversation has no process
//	message 3 on the same one   → a NEW instance, -recall'd onto the SAME session
//
// The client never claims, never releases and never learns any of it happened.
func TestA2AColdSpawnReusesAndResumes(t *testing.T) {
	base, reg := startA2AStubBroker(t)
	const contextID = "conv-resume"

	// 1. Cold spawn.
	first := answerOf(t, sendA2AMessage(t, base, contextID, "hello"))
	freshSession := stubcore.NewSessionID(stubcore.VariantBase)
	if want := stubcore.TurnAnswer(stubcore.VariantBase, freshSession, false); first != want {
		t.Fatalf("first answer = %q, want %q (a fresh session, no -recall)", first, want)
	}
	leases := reg.Snapshot().Leases
	if len(leases) != 1 {
		t.Fatalf("leases after the first message = %d, want 1", len(leases))
	}
	firstLease := leases[0].ID

	// 2. Same context, same instance.
	second := answerOf(t, sendA2AMessage(t, base, contextID, "still there?"))
	if second != first {
		t.Errorf("second answer = %q, want the same instance's %q", second, first)
	}
	leases = reg.Snapshot().Leases
	if len(leases) != 1 || leases[0].ID != firstLease {
		t.Fatalf("leases after the second message = %+v, want the same single lease %s", leases, firstLease)
	}

	// 3. The instance goes away — exactly as an idle release or a crash leaves it.
	resp, err := http.Post("http://"+base+"/release/"+firstLease, "application/json", nil)
	if err != nil {
		t.Fatalf("POST /release: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release status = %d, want 200", resp.StatusCode)
	}
	if reg.Has(firstLease) {
		t.Fatal("the lease survived its release")
	}

	// 4. The next message resumes rather than failing or starting over.
	third := answerOf(t, sendA2AMessage(t, base, contextID, "are you back?"))
	if want := stubcore.TurnAnswer(stubcore.VariantBase, freshSession, true); third != want {
		t.Fatalf("third answer = %q, want %q: the re-spawn must carry -recall %s so history replays",
			third, want, freshSession)
	}
	leases = reg.Snapshot().Leases
	if len(leases) != 1 {
		t.Fatalf("leases after the resume = %d, want 1", len(leases))
	}
	if leases[0].ID == firstLease {
		t.Error("the resumed conversation reused the released lease id; it must be a new lease")
	}
}

// TestA2AIdleReleaseThenResume pins that idle release keeps working for
// A2A-created leases — they are ordinary leases — and that the conversation
// survives it.
func TestA2AIdleReleaseThenResume(t *testing.T) {
	base, reg := startA2AStubBroker(t,
		withIdleTimeout(300*time.Millisecond),
		withReleaseGrace(2*time.Second),
	)
	const contextID = "conv-idle"

	answerOf(t, sendA2AMessage(t, base, contextID, "hello"))
	if got := len(reg.Snapshot().Leases); got != 1 {
		t.Fatalf("leases = %d, want 1", got)
	}

	// The sweeper reaps it once the conversation has been quiet for the timeout.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(reg.Snapshot().Leases) > 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(reg.Snapshot().Leases); got != 0 {
		t.Fatalf("leases = %d after the idle window, want 0: idle release must apply to A2A leases", got)
	}

	// And the conversation is still resumable.
	answer := answerOf(t, sendA2AMessage(t, base, contextID, "back again"))
	want := stubcore.TurnAnswer(stubcore.VariantBase, stubcore.NewSessionID(stubcore.VariantBase), true)
	if answer != want {
		t.Errorf("answer after idle release = %q, want %q", answer, want)
	}
}

// TestA2ASeparateContextsGetSeparateInstances pins the isolation claim against
// real processes: two conversations are two OS-isolated instances.
func TestA2ASeparateContextsGetSeparateInstances(t *testing.T) {
	base, reg := startA2AStubBroker(t)

	answerOf(t, sendA2AMessage(t, base, "conv-a", "hello"))
	answerOf(t, sendA2AMessage(t, base, "conv-b", "hello"))

	leases := reg.Snapshot().Leases
	if len(leases) != 2 {
		t.Fatalf("leases = %d, want 2: separate contexts must get separate instances", len(leases))
	}
	if leases[0].PID == leases[1].PID {
		t.Error("two contexts share one process")
	}
}

// TestA2AStreamingOverTheWireDrivesARealInstance closes the card honesty
// window: the card advertises capabilities.streaming, and a client that acts on
// it gets a live SSE stream of a turn a real spawned instance ran.
func TestA2AStreamingOverTheWireDrivesARealInstance(t *testing.T) {
	base, _ := startA2AStubBroker(t)

	// The card says streaming is available...
	card := fetchAgentCard(t, base)
	if !card.Capabilities.Streaming {
		t.Fatal("the card does not advertise streaming")
	}

	// ...and it is.
	body := `{"jsonrpc":"2.0","id":7,"method":"SendStreamingMessage","params":{"message":` +
		`{"messageId":"m-stream","role":"ROLE_USER","contextId":"conv-stream","parts":[{"text":"hello"}]}}}`
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post("http://"+base+agentJSONRPCPath(a2aStubProfileName), a2a.ContentTypeJSON, strings.NewReader(body))
	if err != nil {
		t.Fatalf("SendStreamingMessage: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("SendStreamingMessage status = %d: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want an SSE stream", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	stream := string(raw)
	for _, want := range []string{
		a2a.TaskStateSubmitted.String(),
		a2a.TaskStateWorking.String(),
		a2a.TaskStateCompleted.String(),
		stubcore.TurnAnswer(stubcore.VariantBase, stubcore.NewSessionID(stubcore.VariantBase), false),
	} {
		if !strings.Contains(stream, want) {
			t.Errorf("the stream never carried %q:\n%s", want, stream)
		}
	}
}

// fetchAgentCard reads one profile's Agent Card off the broker.
func fetchAgentCard(t *testing.T, base string) a2a.AgentCard {
	t.Helper()
	resp, err := http.Get("http://" + base + agentCardPath(a2aStubProfileName))
	if err != nil {
		t.Fatalf("GET agent card: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("agent card status = %d", resp.StatusCode)
	}
	var card a2a.AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("decode agent card: %v", err)
	}
	return card
}

// TestA2ARespawnSurvivesBrokerRestart is the durability criterion against real
// processes: a broker that restarts with no memory of the conversation still
// resumes it, because the contextId → session binding is on disk.
func TestA2ARespawnSurvivesBrokerRestart(t *testing.T) {
	stateDir := t.TempDir()
	addr := freeAddr(t)
	const contextID = "conv-restart"

	stubBin := buildStubInstance(t)
	configPath := filepath.Join(t.TempDir(), "support.yaml")
	if err := os.WriteFile(configPath, []byte("engine:\n  name: stub\n"), 0o600); err != nil {
		t.Fatalf("write profile config: %v", err)
	}
	profile := AgentProfile{
		Binary:         reservedBinaryName,
		Config:         configPath,
		ResolvedConfig: configPath,
		Card: AgentCardSpec{
			Name:        "Support Agent",
			Description: "Answers support questions.",
			Version:     "0.1.0",
			Skills:      []AgentCardSkill{{ID: "answer", Name: "Answer", Description: "Answers."}},
		},
	}
	opts := []stubBrokerOption{
		withAgents(map[string]AgentProfile{a2aStubProfileName: profile}),
		withStateDir(stateDir),
		withListenAddr(addr),
		withReleaseGrace(2 * time.Second),
	}

	first := startStubBrokerHandle(t, stubBin, opts...)
	firstTask := sendA2AMessage(t, first.base, contextID, "hello")
	answerOf(t, firstTask)
	leases := first.registry.Snapshot().Leases
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(leases))
	}
	// The instance is torn down before the broker is, so the second broker has no
	// surviving process to adopt — the resume must come from the durable index
	// alone.
	if err := first.registry.releaseLease(leases[0].ID, "manual release", 2*time.Second); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}
	first.stop()

	second := startStubBrokerHandle(t, stubBin, opts...)
	answer := answerOf(t, sendA2AMessage(t, second.base, contextID, "still here?"))
	want := stubcore.TurnAnswer(stubcore.VariantBase, stubcore.NewSessionID(stubcore.VariantBase), true)
	if answer != want {
		t.Errorf("answer after a broker restart = %q, want %q: the contextId → session binding must be durable",
			answer, want)
	}

	// The TASK the first broker ran is still readable from the second one. This
	// is the second durable index earning its keep, and it is the case a client
	// actually hits: it holds a task id from before the restart and has no way to
	// know a restart happened.
	restored := getA2ATask(t, second.base, a2aStubProfileName, "", firstTask.ID)
	if restored.Status.State != a2a.TaskStateCompleted {
		t.Errorf("the pre-restart task reads back as %s, want COMPLETED: the task store must be durable",
			restored.Status.State)
	}
	if restored.ContextID != contextID {
		t.Errorf("the pre-restart task's context = %q, want %q", restored.ContextID, contextID)
	}
	if len(restored.Artifacts) != 1 {
		t.Fatalf("the pre-restart task reads back with %d artifacts, want its answer", len(restored.Artifacts))
	}
	wantAnswer := stubcore.TurnAnswer(stubcore.VariantBase, stubcore.NewSessionID(stubcore.VariantBase), false)
	if text, _ := restored.Artifacts[0].Parts[0].TextValue(); text != wantAnswer {
		t.Errorf("the pre-restart answer reads back as %q, want %q", text, wantAnswer)
	}
}

// TestA2AUnknownBinaryRejectsTheTaskOverTheWire pins the spawn-failure
// criterion at the surface a client sees: a REJECTED task, promptly, rather than
// a hang or a protocol error about broker internals.
func TestA2AUnknownBinaryRejectsTheTaskOverTheWire(t *testing.T) {
	stubBin := buildStubInstance(t)
	configPath := filepath.Join(t.TempDir(), "support.yaml")
	if err := os.WriteFile(configPath, []byte("engine:\n  name: stub\n"), 0o600); err != nil {
		t.Fatalf("write profile config: %v", err)
	}
	// A profile naming a registry entry this broker does not offer. LoadConfig
	// would refuse this at boot; a Config assembled in Go reaches the spawn, which
	// is exactly the path under test.
	profile := AgentProfile{
		Binary:         "vision",
		Config:         configPath,
		ResolvedConfig: configPath,
		Card: AgentCardSpec{
			Name:        "Support Agent",
			Description: "Answers support questions.",
			Version:     "0.1.0",
			Skills:      []AgentCardSkill{{ID: "answer", Name: "Answer", Description: "Answers."}},
		},
	}
	base, reg := startStubBrokerWithRegistry(t, stubBin,
		withAgents(map[string]AgentProfile{a2aStubProfileName: profile}))

	started := time.Now()
	task := sendA2AMessage(t, base, "conv-bad-binary", "hello")
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("the refusal took %s; a spawn failure must never hang", elapsed)
	}
	if task.Status.State != a2a.TaskStateRejected {
		t.Fatalf("state = %s, want REJECTED", task.Status.State)
	}
	if got := len(reg.Snapshot().Leases); got != 0 {
		t.Errorf("leases = %d after a refused spawn, want 0", got)
	}
}
