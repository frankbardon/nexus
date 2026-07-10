package aguiclient

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/frankbardon/nexus/pkg/agui"
)

// stubHandler returns an httptest server that echoes a canonical AG-UI stream
// for any POST, optionally enforcing a bearer token. It lets the client be
// tested without booting a Nexus engine.
func stubHandler(bearer string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if bearer != "" && r.Header.Get("Authorization") != "Bearer "+bearer {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		in, err := agui.DecodeRunAgentInput(mustReadAll(r))
		if err != nil {
			http.Error(w, "bad input", http.StatusBadRequest)
			return
		}
		agui.WriteHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		sse := agui.NewSSEWriter(w)
		_ = sse.Write(agui.NewRunStarted(in.ThreadID, in.RunID))
		_ = sse.Write(agui.NewTextMessageStart("m1", "assistant"))
		_ = sse.Write(agui.NewTextMessageContent("m1", "hi"))
		_ = sse.Write(agui.NewTextMessageEnd("m1"))
		_ = sse.Write(agui.NewRunFinished(in.ThreadID, in.RunID))
	}
}

func mustReadAll(r *http.Request) []byte {
	defer r.Body.Close()
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf
		}
	}
}

func TestClientRun_DecodesStream(t *testing.T) {
	srv := httptest.NewServer(stubHandler(""))
	defer srv.Close()

	c := New(srv.URL)
	res, err := c.Run(t.Context(), UserMessage("t1", "r1", "hello"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	types := res.Types()
	want := []agui.EventType{
		agui.EventRunStarted,
		agui.EventTextMessageStart,
		agui.EventTextMessageContent,
		agui.EventTextMessageEnd,
		agui.EventRunFinished,
	}
	if len(types) != len(want) {
		t.Fatalf("event count = %d, want %d (%v)", len(types), len(want), types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("event[%d] = %s, want %s", i, types[i], want[i])
		}
	}
	if res.Count(agui.EventTextMessageContent) != 1 {
		t.Fatalf("content count = %d, want 1", res.Count(agui.EventTextMessageContent))
	}
	if res.First(agui.EventRunStarted) == nil {
		t.Fatal("First(RunStarted) = nil")
	}
}

func TestClientRun_BearerRejected(t *testing.T) {
	srv := httptest.NewServer(stubHandler("secret"))
	defer srv.Close()

	// No token -> 401, no events, no error.
	res, err := New(srv.URL).Run(t.Context(), UserMessage("t1", "r1", "hello"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.StatusCode)
	}
	if res.Events != nil {
		t.Fatalf("events = %v, want nil on rejection", res.Types())
	}

	// Correct token -> 200 stream.
	res, err = New(srv.URL, WithBearer("secret")).Run(t.Context(), UserMessage("t1", "r1", "hello"))
	if err != nil {
		t.Fatalf("run with token: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if res.First(agui.EventRunFinished) == nil {
		t.Fatal("no RunFinished in authorized stream")
	}
}
