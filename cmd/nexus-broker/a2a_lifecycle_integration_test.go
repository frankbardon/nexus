//go:build integration

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/cmd/nexus-broker/testdata/stubcore"
	"github.com/frankbardon/nexus/pkg/a2a"
)

// This file completes the broker A2A ingress's integration coverage against
// REAL spawned processes, and it covers the cases a2a_integration_test.go
// deliberately left to it:
//
//   - the WHOLE journey in one run — card, stream, cold spawn, completion,
//     release, a task read that still answers, and a transparent re-spawn;
//   - the refusals: an unknown profile, a crash with a turn in flight, an
//     unauthenticated caller, and one principal reaching for another's task;
//   - serial queueing, observed from two clients' sockets rather than from the
//     queue's own bookkeeping.
//
// Everything here runs on the same stub instance the claim suite spawns, so the
// suite still needs no API key and makes no LLM call. Two stub switches earn
// their keep below: STUB_TURN_DELAY stretches a turn's WORKING window so a
// second client can observe it, and STUB_CRASH_MID_TURN kills the instance with
// a turn already under way.

// ---- fixtures ----

// a2aProfileFor builds the standard one-skill profile bound to a throwaway
// config file, naming a binary registry entry.
//
// It exists because three tests here need a profile that differs from the
// default in exactly one field, and each of them assembling the whole struct
// would make "what is this test varying?" invisible.
func a2aProfileFor(t *testing.T, binary string) AgentProfile {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(configPath, []byte("engine:\n  name: stub\n"), 0o600); err != nil {
		t.Fatalf("write profile config: %v", err)
	}
	return AgentProfile{
		Binary:         binary,
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
}

// stubEntryWithEnv is the reserved binary registry entry with extra environment
// for the stub.
//
// The switches ride the REGISTRY ENTRY rather than os.Setenv on purpose: an
// entry's Env is per broker, so a test that slows its stub down cannot slow down
// another test's, and the whole file stays safe to run alongside the rest of the
// suite.
func stubEntryWithEnv(stubBin string, env map[string]string) map[string]BinaryEntry {
	return map[string]BinaryEntry{
		reservedBinaryName: {Path: stubBin, ResolvedPath: stubBin, Env: env},
	}
}

// ---- wire helpers ----

// a2aRequest performs one HTTP request against a profile's A2A surface with an
// optional bearer token ("" sends no Authorization header at all, which is what
// an unauthenticated caller looks like on the wire).
//
// It asserts nothing, so a caller can inspect a refusal, and it uses a bounded
// client because "never a hang" is a property under test here — an unbounded
// request would report a hang as a suite timeout with no attribution.
func a2aRequest(t *testing.T, method, base, path, token, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://"+base+path, reader)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", a2a.ContentTypeJSON)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// a2aRPCBody renders a JSON-RPC envelope for one operation.
func a2aRPCBody(t *testing.T, id int, method string, params any) string {
	t.Helper()
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("encode %s params: %v", method, err)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, encoded)
}

// a2aRPC performs a JSON-RPC call and returns the decoded envelope halves. It
// fails on a non-200, since a JSON-RPC refusal rides the BODY: a transport-level
// status here means the URL or the credential was wrong, not the operation, and
// a caller inspecting that uses a2aRequest directly.
func a2aRPC(t *testing.T, base, profile, token string, id int, method string, params any) (result, rpcErr json.RawMessage) {
	t.Helper()
	resp := a2aRequest(t, http.MethodPost, base, agentJSONRPCPath(profile), token,
		a2aRPCBody(t, id, method, params))
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s response: %v", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s status = %d: %s", method, resp.StatusCode, raw)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("%s response is not a JSON-RPC envelope: %v (%s)", method, err, raw)
	}
	return envelope.Result, envelope.Error
}

// getA2ATask reads one task back over the JSON-RPC binding, failing on a
// refusal.
func getA2ATask(t *testing.T, base, profile, token, taskID string) a2a.Task {
	t.Helper()
	result, rpcErr := a2aRPC(t, base, profile, token, 2, a2a.MethodGetTask, a2a.GetTaskRequest{ID: taskID})
	if rpcErr != nil {
		t.Fatalf("GetTask %s was refused: %s", taskID, rpcErr)
	}
	var task a2a.Task
	if err := json.Unmarshal(result, &task); err != nil {
		t.Fatalf("GetTask result is not a Task: %v (%s)", err, result)
	}
	return task
}

// a2aFrame is one decoded SSE record, stamped with the instant it was read.
//
// The timestamp is deliberately NOT a cross-stream sequence number. Two
// independent readers cannot order two sockets' frames reliably: several frames
// are written microseconds apart when one task ends and the next is promoted,
// and which goroutine is scheduled first in that window is arbitrary. A DURATION
// between two frames survives that; a sequence number does not.
type a2aFrame struct {
	at       time.Time
	taskID   string
	state    string
	artifact string
}

// a2aStreamHandle is a live SSE stream: frames arrive on a channel as they are
// written, and the channel closes at end of stream.
//
// It is a channel rather than a "read it all" call because two of the tests
// below have to ACT on a stream that is still open — send a second message while
// the first turn runs, read a task back while it is still queued — and a helper
// that only returned at EOF could express neither.
type a2aStreamHandle struct {
	frames <-chan a2aFrame
}

// openA2AStream sends a streaming message and returns the stream.
//
// The request itself is performed on the CALLING goroutine, so a refusal fails
// the test where it happened; only the scan runs in the background, and it
// reports problems with t.Errorf, which is legal off the test goroutine where
// t.Fatalf is not.
func openA2AStream(t *testing.T, base, profile, token, contextID, text string) *a2aStreamHandle {
	t.Helper()
	body := a2aRPCBody(t, 7, a2a.MethodSendStreamingMessage, a2a.SendMessageRequest{
		Message: a2a.Message{
			MessageID: "m-" + contextID + "-" + text,
			Role:      a2a.RoleUser,
			ContextID: contextID,
			Parts:     []a2a.Part{a2a.TextPart(text)},
		},
	})
	resp := a2aRequest(t, http.MethodPost, base, agentJSONRPCPath(profile), token, body)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("SendStreamingMessage status = %d: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, a2a.ContentTypeSSE) {
		t.Fatalf("Content-Type = %q, want an SSE stream", ct)
	}

	out := make(chan a2aFrame, 64)
	go func() {
		defer close(out)
		readA2AFrames(t, resp.Body, func(f a2aFrame) { out <- f })
	}()
	return &a2aStreamHandle{frames: out}
}

// next takes the stream's next frame, failing if none arrives in time or the
// stream ended first.
func (h *a2aStreamHandle) next(t *testing.T, within time.Duration) a2aFrame {
	t.Helper()
	select {
	case frame, ok := <-h.frames:
		if !ok {
			t.Fatal("the stream ended before the expected frame arrived")
		}
		return frame
	case <-time.After(within):
		t.Fatalf("no stream frame within %s", within)
		return a2aFrame{}
	}
}

// collect drains a stream to its end, returning every frame including any this
// handle has already yielded to next.
func (h *a2aStreamHandle) collect(t *testing.T, within time.Duration) []a2aFrame {
	t.Helper()
	var frames []a2aFrame
	deadline := time.After(within)
	for {
		select {
		case frame, ok := <-h.frames:
			if !ok {
				return frames
			}
			frames = append(frames, frame)
		case <-deadline:
			t.Fatalf("the stream did not end within %s (%d frames so far)", within, len(frames))
			return frames
		}
	}
}

// streamA2AMessage runs one streaming turn to end of stream and returns every
// frame it carried, in arrival order.
func streamA2AMessage(t *testing.T, base, profile, token, contextID, text string) []a2aFrame {
	t.Helper()
	return openA2AStream(t, base, profile, token, contextID, text).collect(t, 60*time.Second)
}

// readA2AFrames scans an SSE body to EOF, decoding each record into the one
// fact a lifecycle assertion needs — which task it belongs to, and what it said
// — and handing it to emit.
func readA2AFrames(t *testing.T, body io.Reader, emit func(a2aFrame)) {
	scanner := bufio.NewScanner(body)
	// SSE records carrying an artifact can exceed bufio's default 64 KiB line
	// budget once a turn's answer is long; the stub's answers are short, but a
	// scanner that silently stopped mid-stream would look like a truncated task.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var record struct {
			Result struct {
				Task *struct {
					ID     string `json:"id"`
					Status struct {
						State string `json:"state"`
					} `json:"status"`
				} `json:"task"`
				StatusUpdate *struct {
					TaskID string `json:"taskId"`
					Status struct {
						State string `json:"state"`
					} `json:"status"`
				} `json:"statusUpdate"`
				ArtifactUpdate *struct {
					TaskID   string `json:"taskId"`
					Artifact struct {
						Parts []struct {
							Text string `json:"text"`
						} `json:"parts"`
					} `json:"artifact"`
				} `json:"artifactUpdate"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &record); err != nil {
			t.Errorf("SSE record is not JSON: %v (%s)", err, line)
			continue
		}
		frame := a2aFrame{}
		switch {
		case record.Result.Task != nil:
			frame.taskID = record.Result.Task.ID
			frame.state = record.Result.Task.Status.State
		case record.Result.StatusUpdate != nil:
			frame.taskID = record.Result.StatusUpdate.TaskID
			frame.state = record.Result.StatusUpdate.Status.State
		case record.Result.ArtifactUpdate != nil:
			frame.taskID = record.Result.ArtifactUpdate.TaskID
			if parts := record.Result.ArtifactUpdate.Artifact.Parts; len(parts) > 0 {
				frame.artifact = parts[0].Text
			}
		default:
			continue
		}
		frame.at = time.Now()
		emit(frame)
	}
}

// states renders a frame slice as its state sequence, artifacts included as
// "artifact", so an assertion reads like the lifecycle it is checking.
func states(frames []a2aFrame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		if f.state == "" {
			out = append(out, "artifact")
			continue
		}
		out = append(out, f.state)
	}
	return out
}

// firstFrameWith returns the first frame reporting a state, and whether one was
// seen at all.
func firstFrameWith(frames []a2aFrame, state string) (a2aFrame, bool) {
	for _, f := range frames {
		if f.state == state {
			return f, true
		}
	}
	return a2aFrame{}, false
}

// ---- the whole journey ----

// TestA2AJourneyCardStreamReleaseReadRespawn is the effort's headline scenario
// in one run, driven entirely over HTTP by a client that knows nothing but A2A:
//
//	fetch the profile's card       → the URLs and capabilities it will act on
//	send a streaming message       → cold spawn, SUBMITTED → WORKING → COMPLETED
//	release the lease              → the conversation now has no process at all
//	GetTask                        → still answers, from the durable store
//	send again on the same context → a transparent re-spawn carrying history
//
// Each step is asserted from the client's side of the wire. The one thing read
// out of band is the lease registry, and only to prove the OPPOSITE of what the
// client sees: that a process was started, stopped and started again while the
// conversation appeared continuous.
func TestA2AJourneyCardStreamReleaseReadRespawn(t *testing.T) {
	stateDir := t.TempDir()
	base, reg := startStubBrokerWithRegistry(t, buildStubInstance(t),
		withAgents(map[string]AgentProfile{a2aStubProfileName: a2aProfileFor(t, reservedBinaryName)}),
		withStateDir(stateDir),
		withReleaseGrace(2*time.Second),
	)
	const contextID = "conv-journey"

	// 1. Discovery. A client handed this URL needs nothing else, so the card has
	//    to carry absolute endpoints and an honest capability set.
	card := fetchAgentCard(t, base)
	if card.Name != "Support Agent" {
		t.Errorf("card name = %q, want the configured one", card.Name)
	}
	if !card.Capabilities.Streaming {
		t.Fatal("the card does not advertise streaming, so step 2 would be acting on a promise it never made")
	}
	if card.Capabilities.PushNotifications {
		t.Error("the card advertises push notifications, which this ingress does not implement")
	}
	if len(card.SupportedInterfaces) == 0 {
		t.Fatal("the card advertises no interfaces at all")
	}
	wantURL := "http://" + base + agentJSONRPCPath(a2aStubProfileName)
	var sawJSONRPC bool
	for _, iface := range card.SupportedInterfaces {
		if iface.URL == wantURL {
			sawJSONRPC = true
		}
		if !strings.HasPrefix(iface.URL, "http://"+base) {
			t.Errorf("interface URL %q is not absolute against this broker", iface.URL)
		}
	}
	if !sawJSONRPC {
		t.Errorf("the card never advertises %q; a client following the card would not find the endpoint", wantURL)
	}

	// 2. The turn, streamed. This is the cold spawn: no instance exists yet.
	frames := streamA2AMessage(t, base, a2aStubProfileName, "", contextID, "hello")
	got := states(frames)
	for _, want := range []string{
		string(a2a.TaskStateSubmitted),
		string(a2a.TaskStateWorking),
		"artifact",
		string(a2a.TaskStateCompleted),
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("the stream never carried %q; it carried %v", want, got)
		}
	}
	if last := got[len(got)-1]; last != string(a2a.TaskStateCompleted) {
		t.Errorf("the stream ended on %q, want the terminal COMPLETED frame last", last)
	}
	taskID := frames[0].taskID
	if taskID == "" {
		t.Fatal("the opening frame carries no task id, so nothing later can be read back")
	}
	freshSession := stubcore.NewSessionID(stubcore.VariantBase)
	wantAnswer := stubcore.TurnAnswer(stubcore.VariantBase, freshSession, false)
	var sawAnswer bool
	for _, f := range frames {
		if f.artifact == wantAnswer {
			sawAnswer = true
		}
		if f.taskID != taskID {
			t.Errorf("frame belongs to task %q, want the one task %q this stream opened", f.taskID, taskID)
		}
	}
	if !sawAnswer {
		t.Errorf("no artifact carried %q, so the answer a real instance produced never reached the client", wantAnswer)
	}

	leases := reg.Snapshot().Leases
	if len(leases) != 1 {
		t.Fatalf("leases after the first message = %d, want the one instance the message spawned", len(leases))
	}
	firstLease := leases[0].ID

	// 3. The instance goes away — exactly as an idle release or a crash leaves
	//    the conversation.
	resp := a2aRequest(t, http.MethodPost, base, "/release/"+firstLease, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release status = %d, want 200", resp.StatusCode)
	}
	if reg.Has(firstLease) {
		t.Fatal("the lease survived its release")
	}

	// 4. The task is STILL readable. This is the whole reason the store is
	//    durable: a client asks about a task precisely when the process that ran
	//    it is gone.
	task := getA2ATask(t, base, a2aStubProfileName, "", taskID)
	if task.Status.State != a2a.TaskStateCompleted {
		t.Errorf("GetTask after the release = %s, want COMPLETED", task.Status.State)
	}
	if task.ContextID != contextID {
		t.Errorf("GetTask context = %q, want %q", task.ContextID, contextID)
	}
	if len(task.Artifacts) != 1 {
		t.Fatalf("GetTask artifacts = %d, want the single response artifact", len(task.Artifacts))
	}
	if text, _ := task.Artifacts[0].Parts[0].TextValue(); text != wantAnswer {
		t.Errorf("the stored answer = %q, want %q", text, wantAnswer)
	}

	// 5. The next message resumes rather than starting over, and the client is
	//    told none of it.
	resumed := answerOf(t, sendA2AMessage(t, base, contextID, "are you back?"))
	if want := stubcore.TurnAnswer(stubcore.VariantBase, freshSession, true); resumed != want {
		t.Fatalf("the resumed answer = %q, want %q: the re-spawn must carry -recall %s so history replays",
			resumed, want, freshSession)
	}
	leases = reg.Snapshot().Leases
	if len(leases) != 1 {
		t.Fatalf("leases after the resume = %d, want 1", len(leases))
	}
	if leases[0].ID == firstLease {
		t.Error("the resumed conversation reused the released lease id; it must be a new lease")
	}

	// And the first task is still the first task: a resume does not disturb the
	// record of the turn that came before it.
	if again := getA2ATask(t, base, a2aStubProfileName, "", taskID); again.Status.State != a2a.TaskStateCompleted {
		t.Errorf("the original task reads back as %s after a resume, want COMPLETED", again.Status.State)
	}
}

// ---- failure scenarios ----

// TestA2AUnknownProfileRefusesWithoutSpawningAnything is the unknown-profile
// criterion at the level the unit test cannot reach.
//
// TestA2AUnknownProfileIsRefusedOnEveryBinding already pins the SHAPE of the
// refusal — 404, machine-readable, never the mux's HTML default — against an
// httptest server. What only a real broker can prove is the rest of it: that no
// process was started, no lease exists, and the profile that IS configured is
// untouched by a stream of requests for one that is not. A handler that resolved
// the profile after acquiring an instance would satisfy the unit test and leak a
// process per probe.
func TestA2AUnknownProfileRefusesWithoutSpawningAnything(t *testing.T) {
	spawns := &countingRunner{inner: execRunner{}}
	base, reg := startStubBrokerWithRegistry(t, buildStubInstance(t),
		withAgents(map[string]AgentProfile{a2aStubProfileName: a2aProfileFor(t, reservedBinaryName)}),
		withCommandRunner(spawns),
	)
	const unknown = "no-such-agent"

	t.Run("card", func(t *testing.T) {
		resp := a2aRequest(t, http.MethodGet, base, agentCardPath(unknown), "", "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("card status = %d, want 404", resp.StatusCode)
		}
		assertJSONBody(t, resp)
	})

	t.Run("jsonrpc", func(t *testing.T) {
		body := a2aRPCBody(t, 1, a2a.MethodSendMessage, a2a.SendMessageRequest{
			Message: a2a.Message{MessageID: "m1", Role: a2a.RoleUser, Parts: []a2a.Part{a2a.TextPart("hi")}},
		})
		resp := a2aRequest(t, http.MethodPost, base, agentJSONRPCPath(unknown), "", body)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("jsonrpc status = %d, want 404", resp.StatusCode)
		}
		assertJSONBody(t, resp)
	})

	t.Run("rest", func(t *testing.T) {
		resp := a2aRequest(t, http.MethodGet, base, agentRESTPrefix(unknown)+"/tasks", "", "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("rest status = %d, want 404", resp.StatusCode)
		}
		assertJSONBody(t, resp)
	})

	// The known profile is untouched by any of it.
	if resp := a2aRequest(t, http.MethodGet, base, agentCardPath(a2aStubProfileName), "", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("the configured profile's card now answers %d, want 200", resp.StatusCode)
	}
	if n := spawns.count(); n != 0 {
		t.Errorf("spawns = %d after refusals only, want 0", n)
	}
	if n := len(reg.Snapshot().Leases); n != 0 {
		t.Errorf("leases = %d after refusals only, want 0", n)
	}
}

// assertJSONBody fails unless a response carries a parseable JSON object, which
// is the minimum an A2A client can act on.
func assertJSONBody(t *testing.T, resp *http.Response) {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want JSON; an A2A client cannot parse anything else", ct)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("body is not a JSON object: %v (%s)", err, raw)
	}
	if len(decoded) == 0 {
		t.Errorf("body is an empty object, so the refusal says nothing: %s", raw)
	}
}

// TestA2AInstanceCrashMidTaskFailsTheTaskAndFreesTheLease is the crash
// criterion at the surface a client sees.
//
// The stub is configured to publish its opening status and then die, so the task
// has DEMONSTRABLY started — the stream carries WORKING — before the process
// disappears. A crash before the turn began would be a weaker fixture: it is
// covered by the spawn-failure path, and it never exercises the "settle a live
// task" half of the crash watcher.
func TestA2AInstanceCrashMidTaskFailsTheTaskAndFreesTheLease(t *testing.T) {
	stubBin := buildStubInstance(t)
	base, reg := startStubBrokerWithRegistry(t, stubBin,
		withAgents(map[string]AgentProfile{a2aStubProfileName: a2aProfileFor(t, reservedBinaryName)}),
		withBinaries(stubEntryWithEnv(stubBin, map[string]string{"STUB_CRASH_MID_TURN": "1"})),
		withReleaseGrace(2*time.Second),
	)

	started := time.Now()
	frames := streamA2AMessage(t, base, a2aStubProfileName, "", "conv-crash", "hello")
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("the stream took %s to settle; a crashed instance must not hang a client", elapsed)
	}
	got := states(frames)
	if !slices.Contains(got, string(a2a.TaskStateWorking)) {
		t.Fatalf("the stream never reached WORKING, so the turn never started: %v", got)
	}
	if last := got[len(got)-1]; last != string(a2a.TaskStateFailed) {
		t.Fatalf("the stream ended on %q, want FAILED: %v", last, got)
	}
	if slices.Contains(got, "artifact") {
		t.Error("a crashed turn published an answer artifact")
	}

	// The lease the crash watcher owns is gone, and the task's terminal record
	// survives it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(reg.Snapshot().Leases) > 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(reg.Snapshot().Leases); n != 0 {
		t.Errorf("leases = %d after the crash, want 0: the crash watcher must free the slot", n)
	}
	task := getA2ATask(t, base, a2aStubProfileName, "", frames[0].taskID)
	if task.Status.State != a2a.TaskStateFailed {
		t.Errorf("GetTask after the crash = %s, want FAILED", task.Status.State)
	}
	if task.Status.Message == nil {
		t.Error("the terminal status carries no message, so a client is told nothing about why")
	}
}

// TestA2AAuthRefusalPrecedesTheSpawn is the auth criterion at the level the
// unit test cannot reach.
//
// TestA2ARoutesAreBehindTheAuthGuard already pins that every A2A route — the
// card included, which is where this broker deliberately differs from the
// standalone nexus.io.a2a plugin — answers 401 without a credential. What only a
// real broker can prove is ORDERING: the spawn count is the load-bearing
// assertion here, because a guard that answered 401 only AFTER starting an
// instance would satisfy every status-code check ever written while handing an
// unauthenticated caller a process.
func TestA2AAuthRefusalPrecedesTheSpawn(t *testing.T) {
	broker := startAuthedStubBroker(t, buildStubInstance(t),
		withAgents(map[string]AgentProfile{a2aStubProfileName: a2aProfileFor(t, reservedBinaryName)}),
	)
	base := broker.Base

	sendBody := a2aRPCBody(t, 1, a2a.MethodSendMessage, a2a.SendMessageRequest{
		Message: a2a.Message{
			MessageID: "m-auth", Role: a2a.RoleUser, ContextID: "conv-auth",
			Parts: []a2a.Part{a2a.TextPart("hello")},
		},
	})

	surfaces := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"card", http.MethodGet, agentCardPath(a2aStubProfileName), ""},
		{"jsonrpc", http.MethodPost, agentJSONRPCPath(a2aStubProfileName), sendBody},
		{"rest-list", http.MethodGet, agentRESTPrefix(a2aStubProfileName) + "/tasks", ""},
	}
	for _, surface := range surfaces {
		for _, cred := range []struct {
			name  string
			token string
		}{
			{"no credential", ""},
			{"a wrong credential", "not-a-real-token"},
		} {
			resp := a2aRequest(t, surface.method, base, surface.path, cred.token, surface.body)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s with %s = %d, want 401", surface.name, cred.name, resp.StatusCode)
			}
			if challenge := resp.Header.Get("WWW-Authenticate"); challenge == "" {
				t.Errorf("%s with %s carries no WWW-Authenticate challenge", surface.name, cred.name)
			}
		}
	}
	if n := broker.SpawnCount(); n != 0 {
		t.Fatalf("spawns = %d after refusals only, want 0: the guard must precede the spawn", n)
	}

	// The same requests with a real credential work, which is what makes the
	// refusals above about the credential rather than about the routes.
	if resp := a2aRequest(t, http.MethodGet, base, agentCardPath(a2aStubProfileName), broker.Token, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("card with a valid credential = %d, want 200", resp.StatusCode)
	}
	result, rpcErr := a2aRPC(t, base, a2aStubProfileName, broker.Token, 1, a2a.MethodSendMessage,
		a2a.SendMessageRequest{Message: a2a.Message{
			MessageID: "m-auth-ok", Role: a2a.RoleUser, ContextID: "conv-auth",
			Parts: []a2a.Part{a2a.TextPart("hello")},
		}})
	if rpcErr != nil {
		t.Fatalf("an authenticated SendMessage was refused: %s", rpcErr)
	}
	var sent a2a.SendMessageResponse
	if err := json.Unmarshal(result, &sent); err != nil {
		t.Fatalf("result is not a SendMessageResponse: %v (%s)", err, result)
	}
	if sent.Task == nil || sent.Task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("the authenticated turn did not complete: %+v", sent.Task)
	}
	if n := broker.SpawnCount(); n != 1 {
		t.Errorf("spawns = %d after one authenticated turn, want 1", n)
	}
}

// TestA2ACrossPrincipalTaskReadIsIndistinguishableFromUnknown is the isolation
// criterion end to end: one principal runs a task, a second one — holding a
// perfectly VALID credential — asks for it by id.
//
// The assertion is not merely "refused". It is that the refusal is
// BYTE-FOR-BYTE the answer an id that never existed gets, because any
// difference at all — a distinct message, a distinct code, a distinct status —
// turns the endpoint into an existence oracle for ids the caller was never told.
func TestA2ACrossPrincipalTaskReadIsIndistinguishableFromUnknown(t *testing.T) {
	broker := startAuthedStubBroker(t, buildStubInstance(t),
		withAgents(map[string]AgentProfile{a2aStubProfileName: a2aProfileFor(t, reservedBinaryName)}),
		withStateDir(t.TempDir()),
	)
	base := broker.Base

	// The owner runs a turn and holds its task id.
	result, rpcErr := a2aRPC(t, base, a2aStubProfileName, broker.Token, 1, a2a.MethodSendMessage,
		a2a.SendMessageRequest{Message: a2a.Message{
			MessageID: "m-owner", Role: a2a.RoleUser, ContextID: "conv-owned",
			Parts: []a2a.Part{a2a.TextPart("hello")},
		}})
	if rpcErr != nil {
		t.Fatalf("the owner's SendMessage was refused: %s", rpcErr)
	}
	var sent a2a.SendMessageResponse
	if err := json.Unmarshal(result, &sent); err != nil {
		t.Fatalf("result is not a SendMessageResponse: %v (%s)", err, result)
	}
	if sent.Task == nil {
		t.Fatal("SendMessage produced no task")
	}
	taskID := sent.Task.ID

	// The owner can read it.
	if owned := getA2ATask(t, base, a2aStubProfileName, broker.Token, taskID); owned.ID != taskID {
		t.Fatalf("the owner read back task %q, want %q", owned.ID, taskID)
	}

	// The other principal cannot — and cannot tell that it exists.
	_, foreignErr := a2aRPC(t, base, a2aStubProfileName, broker.OtherToken, 2, a2a.MethodGetTask,
		a2a.GetTaskRequest{ID: taskID})
	if foreignErr == nil {
		t.Fatal("a foreign principal read another caller's task")
	}
	_, unknownErr := a2aRPC(t, base, a2aStubProfileName, broker.OtherToken, 2, a2a.MethodGetTask,
		a2a.GetTaskRequest{ID: "task-never-existed"})
	if unknownErr == nil {
		t.Fatal("an id that never existed was answered as a task")
	}
	foreign := strings.ReplaceAll(string(foreignErr), taskID, "<id>")
	unknown := strings.ReplaceAll(string(unknownErr), "task-never-existed", "<id>")
	if foreign != unknown {
		t.Errorf("a foreign task is distinguishable from an unknown one:\n foreign: %s\n unknown: %s",
			foreign, unknown)
	}

	// ListTasks tells the other principal nothing either, including about the
	// context id it now knows.
	listResult, listErr := a2aRPC(t, base, a2aStubProfileName, broker.OtherToken, 3, a2a.MethodListTasks,
		a2a.ListTasksRequest{ContextID: "conv-owned"})
	if listErr != nil {
		t.Fatalf("the other principal's own ListTasks was refused: %s", listErr)
	}
	var listed a2a.ListTasksResponse
	if err := json.Unmarshal(listResult, &listed); err != nil {
		t.Fatalf("result is not a ListTasksResponse: %v (%s)", err, listResult)
	}
	if len(listed.Tasks) != 0 {
		t.Errorf("ListTasks leaked %d of another principal's tasks", len(listed.Tasks))
	}

	// And the owner's own listing is unaffected by any of it.
	ownResult, ownErr := a2aRPC(t, base, a2aStubProfileName, broker.Token, 4, a2a.MethodListTasks,
		a2a.ListTasksRequest{ContextID: "conv-owned"})
	if ownErr != nil {
		t.Fatalf("the owner's ListTasks was refused: %s", ownErr)
	}
	var ownListed a2a.ListTasksResponse
	if err := json.Unmarshal(ownResult, &ownListed); err != nil {
		t.Fatalf("result is not a ListTasksResponse: %v (%s)", err, ownResult)
	}
	if len(ownListed.Tasks) != 1 || ownListed.Tasks[0].ID != taskID {
		t.Errorf("the owner's listing = %+v, want exactly its own task %q", ownListed.Tasks, taskID)
	}
}

// ---- serial queueing ----

// a2aSerialTurnDelay is how long the stub sits mid-turn in the queueing tests.
//
// It is a SLEEP inside the instance, which is what makes the assertions below
// deterministic rather than merely likely: a turn cannot finish sooner than
// this, on any machine, under any load. Every window checked below is derived
// from it, so a slow CI box makes the fixture safer, never flakier.
const a2aSerialTurnDelay = 2 * time.Second

// TestA2ATwoConcurrentTasksOnOneContextRunSerially is the queueing criterion
// observed from two clients' sockets rather than from the queue's own
// bookkeeping.
//
// Two conversations sharing one agent loop is not a hypothetical failure: before
// the queue existed, two simultaneous messages produced two tasks that both
// watched the SAME Nexus turn and both claimed its output as their answer.
//
// The proof is a WINDOW, not an ordering of frames across two sockets. The first
// turn is made to take a known, unshortenable amount of time; the second message
// is sent once the first is provably running; and the second task is then shown
// to produce NOTHING for a long stretch of that turn while sitting in
// SUBMITTED. A frame-order comparison across two readers was tried first and is
// unsound: a task ending and the next one being promoted happen microseconds
// apart, so which reader is scheduled first in that window is arbitrary.
func TestA2ATwoConcurrentTasksOnOneContextRunSerially(t *testing.T) {
	stubBin := buildStubInstance(t)
	base, reg := startStubBrokerWithRegistry(t, stubBin,
		withAgents(map[string]AgentProfile{a2aStubProfileName: a2aProfileFor(t, reservedBinaryName)}),
		withBinaries(stubEntryWithEnv(stubBin, map[string]string{
			"STUB_TURN_DELAY": a2aSerialTurnDelay.String(),
		})),
		withReleaseGrace(2*time.Second),
	)
	const contextID = "conv-serial"

	// The first turn, driven as far as WORKING so the second message is
	// contending with a running turn rather than merely following one.
	first := openA2AStream(t, base, a2aStubProfileName, "", contextID, "first")
	if opening := first.next(t, 30*time.Second); opening.state != string(a2a.TaskStateSubmitted) {
		t.Fatalf("the first stream opened on %q, want SUBMITTED", opening.state)
	}
	firstWorking := first.next(t, 30*time.Second)
	if firstWorking.state != string(a2a.TaskStateWorking) {
		t.Fatalf("the first stream's second frame is %q, want WORKING", firstWorking.state)
	}

	// The second message is ACCEPTED — not refused, not made to wait for a
	// response — and opens on SUBMITTED, the specification's own word for
	// "accepted, not yet started".
	second := openA2AStream(t, base, a2aStubProfileName, "", contextID, "second")
	secondOpening := second.next(t, 30*time.Second)
	if secondOpening.state != string(a2a.TaskStateSubmitted) {
		t.Fatalf("the second stream opened on %q, want SUBMITTED", secondOpening.state)
	}
	if secondOpening.taskID == firstWorking.taskID {
		t.Fatal("both messages were folded into one task; they must be two")
	}

	// The window. The first turn is asleep for a2aSerialTurnDelay from its own
	// WORKING frame, so for the first half of that the second task must produce
	// nothing at all: it has not started, and a task that had started would be
	// emitting WORKING here.
	quiet := a2aSerialTurnDelay / 2
	select {
	case frame, ok := <-second.frames:
		if !ok {
			t.Fatal("the second stream closed while the first turn was still running")
		}
		t.Fatalf("the second task emitted %q %s into the first turn; it must stay SUBMITTED until the "+
			"conversation is free, or two turns are sharing one agent loop",
			frame.state, time.Since(secondOpening.at))
	case <-time.After(quiet):
	}

	// Both then run to completion, in order. Queueing delays a turn; it never
	// drops one.
	firstFrames := append([]a2aFrame{firstWorking}, first.collect(t, 60*time.Second)...)
	secondFrames := append([]a2aFrame{secondOpening}, second.collect(t, 60*time.Second)...)

	for name, frames := range map[string][]a2aFrame{"first": firstFrames, "second": secondFrames} {
		got := states(frames)
		if len(got) == 0 || got[len(got)-1] != string(a2a.TaskStateCompleted) {
			t.Errorf("the %s stream did not end on COMPLETED: %v", name, got)
		}
		if !slices.Contains(got, "artifact") {
			t.Errorf("the %s task published no answer artifact: %v", name, got)
		}
	}

	// The positive statement of the same property, and the one that catches the
	// failure mode this queue exists for.
	//
	// It is deliberately measured to the second task's COMPLETION rather than to
	// its WORKING frame. Without serialization the second task attaches to the
	// live instance and adopts the FIRST turn's output as its own answer — so
	// both tasks finish at the same instant, having run ONE turn between them,
	// and a WORKING-to-WORKING comparison cannot tell that apart from two turns
	// back to back. Two turns take two turns' worth of wall clock; one turn
	// wearing two task ids does not.
	secondDone, ok := firstFrameWith(secondFrames, string(a2a.TaskStateCompleted))
	if !ok {
		t.Fatal("the second task never completed")
	}
	if gap := secondDone.at.Sub(firstWorking.at); gap < 2*a2aSerialTurnDelay {
		t.Errorf("both tasks finished %s after the first turn started, inside the %s ONE turn takes: "+
			"the second task adopted the first turn's output instead of running its own",
			gap, 2*a2aSerialTurnDelay)
	}

	// And both ran on ONE instance: serializing is what makes reuse safe.
	if leases := reg.Snapshot().Leases; len(leases) != 1 {
		t.Fatalf("leases = %d, want the single instance both turns shared", len(leases))
	}
}

// TestA2AQueuedTaskIsReadableWhileItWaits pins the other half of the queueing
// promise: a queued task is a COMPLETE task from the moment it is accepted, not
// a placeholder that becomes real when its turn comes.
//
// It is separate from the ordering test — which is what actually guards the
// queue — because this one asserts a property of the WAITING task: that it
// answers reads, by id and in a listing, while it waits. Folding the two
// together would leave the ordering assertion racing a GetTask.
func TestA2AQueuedTaskIsReadableWhileItWaits(t *testing.T) {
	stubBin := buildStubInstance(t)
	base, reg := startStubBrokerWithRegistry(t, stubBin,
		withAgents(map[string]AgentProfile{a2aStubProfileName: a2aProfileFor(t, reservedBinaryName)}),
		// The same unshortenable turn the ordering test uses, so the read below
		// provably lands while the second task is still queued rather than merely
		// still recent.
		withBinaries(stubEntryWithEnv(stubBin, map[string]string{
			"STUB_TURN_DELAY": a2aSerialTurnDelay.String(),
		})),
		withReleaseGrace(2*time.Second),
	)
	const contextID = "conv-queued-read"

	// Driven as far as WORKING, which is the instant the stub's sleep starts —
	// so everything below happens inside a turn that cannot end early.
	first := openA2AStream(t, base, a2aStubProfileName, "", contextID, "first")
	first.next(t, 30*time.Second)
	if working := first.next(t, 30*time.Second); working.state != string(a2a.TaskStateWorking) {
		t.Fatalf("the first stream's second frame is %q, want WORKING", working.state)
	}
	if n := len(reg.Snapshot().Leases); n != 1 {
		t.Fatalf("leases = %d once the first turn is working, want 1", n)
	}

	// The second message is ACCEPTED immediately — the opening frame arrives
	// without waiting for the first turn — and carries a real task id.
	second := openA2AStream(t, base, a2aStubProfileName, "", contextID, "second")
	opening := second.next(t, 10*time.Second)
	if opening.state != string(a2a.TaskStateSubmitted) {
		t.Fatalf("the queued task opened at %q, want SUBMITTED", opening.state)
	}
	if opening.taskID == "" {
		t.Fatal("the queued task has no id, so nothing can read it back")
	}

	// While it waits it is an ordinary readable task, not a placeholder.
	queued := getA2ATask(t, base, a2aStubProfileName, "", opening.taskID)
	if queued.ID != opening.taskID {
		t.Errorf("GetTask on a queued task returned %q, want %q", queued.ID, opening.taskID)
	}
	if queued.ContextID != contextID {
		t.Errorf("the queued task's context = %q, want %q", queued.ContextID, contextID)
	}
	if queued.Status.State != a2a.TaskStateSubmitted {
		t.Errorf("GetTask on a queued task = %s, want SUBMITTED while it waits", queued.Status.State)
	}

	// And it is in the caller's listing, under the state it is actually in — a
	// client polling for what is outstanding must see the work it submitted.
	listResult, listErr := a2aRPC(t, base, a2aStubProfileName, "", 5, a2a.MethodListTasks,
		a2a.ListTasksRequest{ContextID: contextID, Status: a2a.TaskStateSubmitted})
	if listErr != nil {
		t.Fatalf("ListTasks was refused: %s", listErr)
	}
	var listed a2a.ListTasksResponse
	if err := json.Unmarshal(listResult, &listed); err != nil {
		t.Fatalf("result is not a ListTasksResponse: %v (%s)", err, listResult)
	}
	if len(listed.Tasks) != 1 || listed.Tasks[0].ID != opening.taskID {
		t.Errorf("ListTasks(status=SUBMITTED) = %+v, want exactly the queued task %q", listed.Tasks, opening.taskID)
	}

	// Both then run to completion; queueing delays a turn, it does not drop one.
	firstRest := first.collect(t, 60*time.Second)
	secondRest := second.collect(t, 60*time.Second)
	for name, frames := range map[string][]a2aFrame{"first": firstRest, "second": secondRest} {
		got := states(frames)
		if len(got) == 0 || got[len(got)-1] != string(a2a.TaskStateCompleted) {
			t.Errorf("the %s stream did not end on COMPLETED: %v", name, got)
		}
	}
}
