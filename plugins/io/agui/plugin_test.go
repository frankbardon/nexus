package agui

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/frankbardon/nexus/pkg/agui"
)

// freeAddr grabs an ephemeral loopback port so tests never collide on 8090.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func startServer(t *testing.T, cfg serverConfig) *Server {
	t.Helper()
	if cfg.logger == nil {
		cfg.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := NewServer(cfg)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(t.Context()) })
	// Poll until the listener is accepting.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.Dial("tcp", cfg.addr)
		if err == nil {
			_ = c.Close()
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("server did not come up")
	return nil
}

func post(t *testing.T, url, token, origin, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func TestRunAgentStubStream(t *testing.T) {
	addr := freeAddr(t)
	startServer(t, serverConfig{addr: addr})

	resp := post(t, "http://"+addr+agentPath, "", "", `{"threadId":"t1","runId":"r1"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != agui.ContentType {
		t.Fatalf("content-type = %q, want %q", ct, agui.ContentType)
	}

	events, err := agui.NewSSEReader(resp.Body).ReadAll()
	if err != nil {
		t.Fatalf("read sse: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events))
	}
	if events[0].EventType() != agui.EventRunStarted {
		t.Fatalf("event[0] = %s, want RunStarted", events[0].EventType())
	}
	if events[1].EventType() != agui.EventRunError {
		t.Fatalf("event[1] = %s, want RunError", events[1].EventType())
	}
}

func TestBearerAuthEnforced(t *testing.T) {
	addr := freeAddr(t)
	startServer(t, serverConfig{addr: addr, bearerToken: "secret"})

	// Missing / wrong token -> 401.
	resp := post(t, "http://"+addr+agentPath, "", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", resp.StatusCode)
	}

	resp = post(t, "http://"+addr+agentPath, "wrong", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-token status = %d, want 401", resp.StatusCode)
	}

	// Correct token -> 200.
	resp = post(t, "http://"+addr+agentPath, "secret", "", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid-token status = %d, want 200", resp.StatusCode)
	}
}

func TestCORSHonored(t *testing.T) {
	addr := freeAddr(t)
	startServer(t, serverConfig{addr: addr, corsOrigins: []string{"https://app.example.com"}})

	// Allowed origin gets echoed.
	resp := post(t, "http://"+addr+agentPath, "", "https://app.example.com", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("allow-origin = %q, want echoed origin", got)
	}

	// Disallowed origin gets no header.
	resp = post(t, "http://"+addr+agentPath, "", "https://evil.example.com", `{"threadId":"t","runId":"r"}`)
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("allow-origin = %q, want empty for disallowed origin", got)
	}
}

func TestCORSWildcard(t *testing.T) {
	addr := freeAddr(t)
	startServer(t, serverConfig{addr: addr, corsOrigins: []string{"*"}})

	req, _ := http.NewRequest(http.MethodOptions, "http://"+addr+agentPath, nil)
	req.Header.Set("Origin", "https://any.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://any.example.com" {
		t.Fatalf("wildcard allow-origin = %q, want echoed origin", got)
	}
}

func TestParseCORSOrigins(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"nil", nil, 0},
		{"list", []any{"a", " b ", ""}, 2},
		{"strings", []string{"a", "b"}, 2},
		{"csv", "a, b ,,c", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(parseCORSOrigins(tc.in)); got != tc.want {
				t.Fatalf("len = %d, want %d", got, tc.want)
			}
		})
	}
}
