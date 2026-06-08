// Package delegate is the sub-agent invocation primitive — the runtime that
// lets a parent agent call another agent with a different reasoning posture,
// system prompt, allowed-tools subset, and resource budget. From the parent's
// perspective a delegate call is a single tool invocation; underneath, the
// runtime spawns a sub-session that has its own context window, its own
// envelope identity (Causation.AgentID and Depth), and its own budget.
//
// Postures are looked up via the posture.Registry capability; budgets are
// non-negotiable and enforced here; recursion depth is capped per-posture
// (falling back to MaxDepth on the Runtime). Results are optionally cached
// by a content-addressable key including the posture's Version, so any edit
// to the posture invalidates previously-cached outputs.
package delegate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/pkg/posture"
)

// Status classifies a delegate outcome. Replay tools and the parent agent's
// follow-up prompt branch on this rather than parsing Error strings.
type Status string

const (
	StatusSuccess  Status = "success"
	StatusPartial  Status = "partial"
	StatusError    Status = "error"
	StatusTimeout  Status = "timeout"
	StatusCancel   Status = "cancelled"
	StatusCacheHit Status = "cache_hit"
)

// Input is the parent agent's request. Posture names the registered
// AgentPosture; Task is the natural-language instruction; Context is a
// structured map the runtime serializes into the sub-agent's initial
// user message under a <delegate_context> XML wrapper.
type Input struct {
	Posture     string
	Task        string
	Context     map[string]any
	ParentTurn  string
	ParentDepth int
	Overrides   Overrides
}

// Overrides let a single call tighten (or loosen) the posture default budget.
// Zero fields fall back to the posture's DefaultBudget.
type Overrides struct {
	MaxTokens    int
	MaxToolCalls int
	Timeout      time.Duration
}

// Output is the runtime's response to the parent agent. SubSessionID
// correlates this call with the journal entries the sub-agent produced.
type Output struct {
	Result        string
	Status        Status
	Error         string
	TokensUsed    int
	ToolCallsUsed int
	Elapsed       time.Duration
	SubSessionID  string
	PostureName   string
	PostureVer    string
	Depth         int
}

// Cache caches Output by key. Implementations must be safe for concurrent
// use. Default in-process cache is bounded by the configured Capacity.
type Cache interface {
	Get(key string) (Output, bool)
	Put(key string, out Output)
}

// Runtime executes delegate calls against a posture registry.
type Runtime struct {
	Registry posture.Registry
	Bus      engine.EventBus
	Logger   *slog.Logger
	Cache    Cache

	// MaxDepth caps recursion depth across all postures. Zero disables the
	// global cap (postures may still set MaxRecursionDepth).
	MaxDepth int

	// ToolSnapshot returns the currently-registered tool defs the
	// sub-agent should choose from. Filtered by AllowedTools before the
	// LLM request is built.
	ToolSnapshot func() []events.ToolDef
}

// ErrRecursionLimit is returned when a delegate call exceeds MaxDepth or the
// posture's MaxRecursionDepth.
var ErrRecursionLimit = errors.New("delegate: recursion depth limit exceeded")

// Run executes a single delegate call. Blocks until the sub-agent completes,
// the budget exhausts, the context cancels, or an error halts the loop.
func (r *Runtime) Run(ctx context.Context, in Input) (Output, error) {
	if r == nil || r.Registry == nil || r.Bus == nil {
		return Output{}, errors.New("delegate: runtime not initialized")
	}
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}

	post, err := r.Registry.Get(in.Posture)
	if err != nil {
		return Output{Status: StatusError, Error: err.Error()}, err
	}

	depth := in.ParentDepth + 1
	if r.MaxDepth > 0 && depth > r.MaxDepth {
		return Output{
			Status: StatusError,
			Error:  ErrRecursionLimit.Error(),
			Depth:  depth,
		}, ErrRecursionLimit
	}
	if post.MaxRecursionDepth > 0 && depth > post.MaxRecursionDepth {
		return Output{
			Status: StatusError,
			Error:  ErrRecursionLimit.Error(),
			Depth:  depth,
		}, ErrRecursionLimit
	}

	budget := resolveBudget(post.DefaultBudget, in.Overrides)
	subSessionID := newSubSessionID()
	agentID := "delegate/" + post.Name + "/" + subSessionID

	// Cache lookup before any side effects.
	key := cacheKey(*post, in)
	if r.Cache != nil {
		if cached, ok := r.Cache.Get(key); ok {
			cached.Status = StatusCacheHit
			cached.SubSessionID = subSessionID
			r.emitStart(subSessionID, in, *post, depth)
			r.emitComplete(subSessionID, cached)
			return cached, nil
		}
	}

	// Push causation so every event emitted during the sub-session carries
	// the sub-agent's identity and depth automatically.
	if cc, ok := r.Bus.(engine.CausationController); ok {
		pop := cc.PushCausationContext(engine.CausationContext{
			AgentID: agentID,
			Depth:   depth,
		})
		defer pop()
	}

	r.emitStart(subSessionID, in, *post, depth)

	if budget.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget.Timeout)
		defer cancel()
	}

	start := time.Now()
	tools := r.filterTools(post.AllowedTools)

	out := r.runLoop(ctx, runOpts{
		subSessionID: subSessionID,
		agentID:      agentID,
		depth:        depth,
		posture:      *post,
		input:        in,
		budget:       budget,
		tools:        tools,
		logger:       logger.With("delegate", post.Name, "sub_session", subSessionID, "depth", depth),
	})
	out.Elapsed = time.Since(start)
	out.SubSessionID = subSessionID
	out.PostureName = post.Name
	out.PostureVer = post.Version
	out.Depth = depth

	if r.Cache != nil && (out.Status == StatusSuccess || out.Status == StatusPartial) {
		// Don't cache errors/timeouts — operators want a retry to actually
		// re-execute, not replay a transient failure.
		r.Cache.Put(key, out)
	}
	r.emitComplete(subSessionID, out)
	return out, nil
}

type runOpts struct {
	subSessionID string
	agentID      string
	depth        int
	posture      posture.AgentPosture
	input        Input
	budget       posture.ResourceBudget
	tools        []events.ToolDef
	logger       *slog.Logger
}

func (r *Runtime) runLoop(ctx context.Context, opts runOpts) Output {
	source := "delegate." + opts.subSessionID
	turnID := "delegate_" + opts.subSessionID

	history := buildInitialHistory(opts.posture.SystemPrompt, opts.input)

	var (
		totalTokens int
		toolCalls   int
	)

	for iteration := 0; ; iteration++ {
		if err := ctx.Err(); err != nil {
			status := StatusError
			if errors.Is(err, context.DeadlineExceeded) {
				status = StatusTimeout
			} else if errors.Is(err, context.Canceled) {
				status = StatusCancel
			}
			return Output{
				Status:        status,
				Error:         err.Error(),
				Result:        lastAssistantContent(history),
				TokensUsed:    totalTokens,
				ToolCallsUsed: toolCalls,
			}
		}

		resp, err := r.requestLLM(ctx, source, opts.posture, history, opts.tools)
		if err != nil {
			return Output{
				Status:        StatusError,
				Error:         err.Error(),
				Result:        lastAssistantContent(history),
				TokensUsed:    totalTokens,
				ToolCallsUsed: toolCalls,
			}
		}
		totalTokens += resp.Usage.TotalTokens

		history = append(history, events.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		if opts.budget.MaxTokens > 0 && totalTokens >= opts.budget.MaxTokens {
			opts.logger.Info("budget exhausted: tokens", "used", totalTokens, "cap", opts.budget.MaxTokens)
			return Output{
				Status:        StatusPartial,
				Result:        resp.Content,
				Error:         "max_tokens budget exceeded",
				TokensUsed:    totalTokens,
				ToolCallsUsed: toolCalls,
			}
		}

		if len(resp.ToolCalls) == 0 {
			return Output{
				Status:        StatusSuccess,
				Result:        resp.Content,
				TokensUsed:    totalTokens,
				ToolCallsUsed: toolCalls,
			}
		}

		if opts.budget.MaxToolCalls > 0 && toolCalls+len(resp.ToolCalls) > opts.budget.MaxToolCalls {
			opts.logger.Info("budget exhausted: tool calls",
				"used", toolCalls, "requested", len(resp.ToolCalls), "cap", opts.budget.MaxToolCalls)
			return Output{
				Status:        StatusPartial,
				Result:        resp.Content,
				Error:         "max_tool_calls budget exceeded",
				TokensUsed:    totalTokens,
				ToolCallsUsed: toolCalls,
			}
		}

		results := r.invokeTools(ctx, turnID, resp.ToolCalls)
		toolCalls += len(resp.ToolCalls)
		for _, res := range results {
			content := res.Output
			if res.Error != "" {
				content = "Error: " + res.Error
			}
			history = append(history, events.Message{
				Role:       "tool",
				Content:    content,
				ToolCallID: res.ID,
			})
		}
	}
}

// requestLLM emits an llm.request tagged with the sub-session source so the
// parent agent's response handler ignores it, then waits synchronously for
// the matching llm.response via SyncLLM.
func (r *Runtime) requestLLM(ctx context.Context, source string, post posture.AgentPosture, history []events.Message, tools []events.ToolDef) (events.LLMResponse, error) {
	req := events.LLMRequest{
		Role:      post.Model.ModelRole,
		Model:     post.Model.Model,
		MaxTokens: post.Model.MaxTokens,
		Messages:  history,
		Tools:     tools,
		Metadata: map[string]any{
			"_source":   source,
			"task_kind": "delegate",
			"posture":   post.Name,
		},
		Tags: map[string]string{"source_plugin": "nexus.agent.delegate"},
	}
	if post.Model.Temperature != 0 {
		t := post.Model.Temperature
		req.Temperature = &t
	}
	return SyncLLM(ctx, r.Bus, req)
}

// invokeTools dispatches a batch of tool calls with TurnID set to the
// sub-session's turn ID so result correlation is unambiguous.
func (r *Runtime) invokeTools(_ context.Context, turnID string, calls []events.ToolCallRequest) []events.ToolResult {
	results := make([]events.ToolResult, 0, len(calls))
	resultCh := make(chan events.ToolResult, len(calls))
	unsub := r.Bus.Subscribe("tool.result", func(ev engine.Event[any]) {
		res, ok := ev.Payload.(events.ToolResult)
		if !ok || res.TurnID != turnID {
			return
		}
		select {
		case resultCh <- res:
		default:
		}
	}, engine.WithPriority(1))
	defer unsub()

	for _, tc := range calls {
		var args map[string]any
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			args = map[string]any{}
		}
		call := events.ToolCall{
			SchemaVersion: events.ToolCallVersion,
			ID:            tc.ID,
			Name:          tc.Name,
			Arguments:     args,
			TurnID:        turnID,
		}
		if veto, err := r.Bus.EmitVetoable("before:tool.invoke", &call); err == nil && veto.Vetoed {
			resultCh <- events.ToolResult{
				SchemaVersion: events.ToolResultVersion,
				ID:            tc.ID,
				Name:          tc.Name,
				Error:         "vetoed: " + veto.Reason,
				TurnID:        turnID,
			}
			continue
		}
		_ = r.Bus.Emit("tool.invoke", call)
	}

	for range calls {
		select {
		case res := <-resultCh:
			results = append(results, res)
		default:
		}
	}
	return results
}

func (r *Runtime) filterTools(allowed []string) []events.ToolDef {
	if r.ToolSnapshot == nil {
		return nil
	}
	snap := r.ToolSnapshot()
	if len(allowed) == 0 {
		return snap
	}
	allow := make(map[string]struct{}, len(allowed))
	for _, n := range allowed {
		allow[n] = struct{}{}
	}
	out := make([]events.ToolDef, 0, len(snap))
	for _, t := range snap {
		if _, ok := allow[t.Name]; ok {
			out = append(out, t)
		}
	}
	return out
}

func (r *Runtime) emitStart(subSessionID string, in Input, post posture.AgentPosture, depth int) {
	_ = r.Bus.Emit("delegate.start", map[string]any{
		"sub_session_id": subSessionID,
		"posture":        post.Name,
		"posture_ver":    post.Version,
		"task":           in.Task,
		"parent_turn":    in.ParentTurn,
		"depth":          depth,
	})
}

func (r *Runtime) emitComplete(subSessionID string, out Output) {
	_ = r.Bus.Emit("delegate.complete", map[string]any{
		"sub_session_id":  subSessionID,
		"posture":         out.PostureName,
		"posture_ver":     out.PostureVer,
		"status":          string(out.Status),
		"error":           out.Error,
		"tokens_used":     out.TokensUsed,
		"tool_calls_used": out.ToolCallsUsed,
		"elapsed_ms":      out.Elapsed.Milliseconds(),
		"result":          out.Result,
		"depth":           out.Depth,
	})
}

func buildInitialHistory(systemPrompt string, in Input) []events.Message {
	var msgs []events.Message
	if systemPrompt != "" {
		msgs = append(msgs, events.Message{Role: "system", Content: systemPrompt})
	}
	content := in.Task
	if len(in.Context) > 0 {
		ctxJSON, _ := json.MarshalIndent(in.Context, "", "  ")
		content = "<delegate_context>\n" + string(ctxJSON) + "\n</delegate_context>\n\n<task>\n" + in.Task + "\n</task>"
	}
	msgs = append(msgs, events.Message{Role: "user", Content: content})
	return msgs
}

func lastAssistantContent(history []events.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" && history[i].Content != "" {
			return history[i].Content
		}
	}
	return ""
}

func resolveBudget(def posture.ResourceBudget, ov Overrides) posture.ResourceBudget {
	out := def
	if ov.Timeout > 0 {
		out.Timeout = ov.Timeout
	}
	if ov.MaxTokens > 0 {
		out.MaxTokens = ov.MaxTokens
	}
	if ov.MaxToolCalls > 0 {
		out.MaxToolCalls = ov.MaxToolCalls
	}
	return out
}

func cacheKey(p posture.AgentPosture, in Input) string {
	h := sha256.New()
	h.Write([]byte(p.Name))
	h.Write([]byte{0})
	h.Write([]byte(p.Version))
	h.Write([]byte{0})
	h.Write([]byte(in.Task))
	h.Write([]byte{0})
	// Canonicalize the context map for stable hashing.
	if len(in.Context) > 0 {
		keys := make([]string, 0, len(in.Context))
		for k := range in.Context {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			h.Write([]byte(k))
			h.Write([]byte{0})
			if data, err := json.Marshal(in.Context[k]); err == nil {
				h.Write(data)
			}
			h.Write([]byte{0})
		}
	}
	allowed := append([]string(nil), p.AllowedTools...)
	sort.Strings(allowed)
	for _, t := range allowed {
		h.Write([]byte(t))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func newSubSessionID() string {
	return engine.GenerateID()[:16]
}

// MemoryCache is a goroutine-safe in-process cache with a fixed capacity
// and least-recently-used eviction. Suitable for the single-engine default.
type MemoryCache struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*memoryEntry
	head     *memoryEntry
	tail     *memoryEntry
}

type memoryEntry struct {
	key  string
	val  Output
	prev *memoryEntry
	next *memoryEntry
}

// NewMemoryCache returns a cache with the given capacity (events, not bytes).
// Capacity <= 0 disables eviction — the cache grows unbounded.
func NewMemoryCache(capacity int) *MemoryCache {
	return &MemoryCache{
		capacity: capacity,
		items:    make(map[string]*memoryEntry),
	}
}

func (c *MemoryCache) Get(key string) (Output, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return Output{}, false
	}
	c.promote(e)
	return e.val, true
}

func (c *MemoryCache) Put(key string, out Output) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		e.val = out
		c.promote(e)
		return
	}
	e := &memoryEntry{key: key, val: out}
	c.items[key] = e
	c.insertAtFront(e)
	if c.capacity > 0 && len(c.items) > c.capacity {
		c.evict()
	}
}

func (c *MemoryCache) insertAtFront(e *memoryEntry) {
	e.next = c.head
	if c.head != nil {
		c.head.prev = e
	}
	c.head = e
	if c.tail == nil {
		c.tail = e
	}
}

func (c *MemoryCache) promote(e *memoryEntry) {
	if c.head == e {
		return
	}
	if e.prev != nil {
		e.prev.next = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	}
	if c.tail == e {
		c.tail = e.prev
	}
	e.prev = nil
	e.next = c.head
	if c.head != nil {
		c.head.prev = e
	}
	c.head = e
}

func (c *MemoryCache) evict() {
	if c.tail == nil {
		return
	}
	old := c.tail
	c.tail = old.prev
	if c.tail != nil {
		c.tail.next = nil
	} else {
		c.head = nil
	}
	delete(c.items, old.key)
}
