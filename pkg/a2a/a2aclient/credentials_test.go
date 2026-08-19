package a2aclient_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/a2a"
	"github.com/frankbardon/nexus/pkg/a2a/a2aclient"
)

func TestNoCredentialsWorksAgainstAnOpenEndpoint(t *testing.T) {
	agent := newAgent(t, agentConfig{})

	// Explicitly, and by omission: both must reach an open agent.
	for _, opts := range [][]a2aclient.Option{
		nil,
		{a2aclient.WithCredentials(a2aclient.NoCredentials{})},
		{a2aclient.WithCredentials(nil)},
	} {
		client, err := a2aclient.New(agent.URL(), opts...)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"}); err != nil {
			t.Fatalf("GetTask: %v", err)
		}
	}
}

func TestCredentialSourceAppliedToEveryRequest(t *testing.T) {
	agent := newAgent(t, agentConfig{requireAuth: "Bearer secret"})

	var mu sync.Mutex
	calls := 0
	source := a2aclient.CredentialFunc(func(_ context.Context, req *http.Request) error {
		mu.Lock()
		calls++
		mu.Unlock()
		req.Header.Set("Authorization", "Bearer secret")
		return nil
	})

	client, err := a2aclient.New(agent.URL(), a2aclient.WithCredentials(source))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"}); err != nil {
		t.Fatalf("GetTask: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// The card fetch and the operation both carry credentials: an agent may
	// protect its card too.
	if calls != 2 {
		t.Fatalf("credential applications = %d, want 2 (card + operation)", calls)
	}
}

func TestCredentialSourceAppliedPerRetryAttempt(t *testing.T) {
	endpoint, attempts := countingServer(t, func(attempt int, w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer token-%d", attempt) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if attempt < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		okTask(w)
	})

	var mu sync.Mutex
	issued := 0
	source := a2aclient.CredentialFunc(func(_ context.Context, req *http.Request) error {
		mu.Lock()
		issued++
		token := fmt.Sprintf("Bearer token-%d", issued)
		mu.Unlock()
		req.Header.Set("Authorization", token)
		return nil
	})

	client := mustClient(t, a2aclient.WithJSONRPCEndpoint(endpoint),
		a2aclient.WithCredentials(source), fastRetry())

	// A retry must carry a FRESH credential, which is the whole reason Apply
	// runs per attempt rather than once per call.
	if _, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"}); err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestCredentialSourceErrorAbortsBeforeSending(t *testing.T) {
	agent := newAgent(t, agentConfig{})
	sentinel := errors.New("token refresh failed")
	source := a2aclient.CredentialFunc(func(context.Context, *http.Request) error { return sentinel })

	client, err := a2aclient.New(agent.URL(),
		a2aclient.WithCard(cardFor(agent)),
		a2aclient.WithCredentials(source))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the credential source's own error", err)
	}
	if len(agent.seen()) != 0 {
		t.Fatal("a request was sent despite the credential failure")
	}
}

// cardAware records the card it is handed.
type cardAware struct {
	mu    sync.Mutex
	cards []string
	fail  error
}

func (c *cardAware) Apply(context.Context, *http.Request) error { return nil }

func (c *cardAware) UseCard(_ context.Context, card *a2a.AgentCard) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail != nil {
		return c.fail
	}
	c.cards = append(c.cards, card.Name)
	return nil
}

func (c *cardAware) seen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.cards...)
}

func TestCardAwareCredentialSourceIsConfiguredOnce(t *testing.T) {
	agent := newAgent(t, agentConfig{
		securitySchemes: map[string]a2a.SecurityScheme{
			"bearer": a2a.BearerScheme("JWT", "a bearer token"),
		},
	})
	source := &cardAware{}

	client, err := a2aclient.New(agent.URL(), a2aclient.WithCredentials(source))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"}); err != nil {
			t.Fatalf("GetTask: %v", err)
		}
	}

	if got := source.seen(); len(got) != 1 || got[0] != "test-agent" {
		t.Fatalf("UseCard calls = %v, want exactly one for test-agent", got)
	}
}

func TestCardAwareCredentialSourceFailureAbortsTheCall(t *testing.T) {
	agent := newAgent(t, agentConfig{})
	source := &cardAware{fail: errors.New("no scheme this source can satisfy")}

	client, err := a2aclient.New(agent.URL(), a2aclient.WithCredentials(source))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.GetTask(context.Background(), a2a.GetTaskRequest{ID: "t1"}); err == nil {
		t.Fatal("GetTask should fail when the credential source cannot configure itself")
	}
}

// cardFor builds the card the test agent would serve, for tests that must not
// depend on fetching it.
func cardFor(a *agent) *a2a.AgentCard {
	card := a2a.NewAgentCard("test-agent", "an in-process A2A agent", "1.0.0").
		WithInterface(a2a.BindingJSONRPC, a.URL()+testJSONRPCPath).
		WithSkill(a2a.AgentSkill{ID: "chat", Name: "chat", Description: "chat", Tags: []string{"chat"}})
	return &card
}
