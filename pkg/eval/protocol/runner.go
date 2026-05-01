package protocol

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"
	"github.com/frankbardon/nexus/pkg/engine/journal"
	"github.com/frankbardon/nexus/pkg/events"
	"gopkg.in/yaml.v3"
)

// resultSummaryCap is the maximum byte length of ToolCall.ResultSummary.
// Truncation is byte-based with a UTF-8-safe fallback. 2 KiB matches the
// plan.md specification.
const resultSummaryCap = 2048

// Run executes a single inspect-mode request end-to-end:
//
//  1. Validate the request envelope.
//  2. Resolve the config (path or inline) and overlay nexus.io.test +
//     core.sessions.root so the run is hermetic.
//  3. Boot the engine, drive the single user input, wait for the agent to
//     idle (no further turns) OR the request's MaxTurns cap OR the ctx
//     deadline.
//  4. Project the live session journal into the wire-format response.
//  5. Tear down the engine cleanly.
//
// Run is the embedder seam for the inspect-mode CLI subcommand and for
// tests. It returns a populated Response on the success path; on failure
// it returns either (nil, error) when the engine never reached a useful
// state, or (response, error) when partial data is worth surfacing.
func Run(ctx context.Context, req *Request) (*Response, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	cfgBytes, err := resolveConfig(req)
	if err != nil {
		return nil, ErrConfigLoad(err)
	}

	// Hermetic sessions root: every inspect-mode invocation gets its own
	// throwaway dir under os.TempDir() so headless eval runs do not pollute
	// ~/.nexus/sessions. Cleanup is best-effort — if the harness wants to
	// inspect the journal, set NEXUS_EVAL_INSPECT_KEEP_SESSIONS=1.
	tmpRoot, err := os.MkdirTemp("", "nexus-inspect-")
	if err != nil {
		return nil, ErrInternal(fmt.Errorf("create temp sessions root: %w", err))
	}
	if os.Getenv("NEXUS_EVAL_INSPECT_KEEP_SESSIONS") == "" {
		defer os.RemoveAll(tmpRoot)
	}

	overlaid, err := overlayConfig(cfgBytes, tmpRoot, req.UserInput)
	if err != nil {
		return nil, ErrConfigLoad(fmt.Errorf("overlay io.test + sessions.root: %w", err))
	}

	eng, err := engine.NewFromBytes(overlaid)
	if err != nil {
		return nil, ErrEngineBoot(err)
	}
	allplugins.RegisterAll(eng.Registry)

	bootCtx, bootCancel := context.WithTimeout(ctx, 30*time.Second)
	defer bootCancel()
	if err := eng.Boot(bootCtx); err != nil {
		return nil, ErrEngineBoot(err)
	}

	// Belt-and-braces shutdown.
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = eng.Stop(stopCtx)
		cancel()
	}()

	sessionID := ""
	journalDir := ""
	if eng.Session != nil {
		sessionID = eng.Session.ID
		journalDir = filepath.Join(eng.Session.RootDir, "journal")
	}

	// Wait for completion: ctx done, MaxTurns reached, or session-end fired.
	// nexus.io.test signals io.session.end after the last turn ends, which
	// is the natural completion signal. MaxTurns is a hard cap on top.
	turnCtx, turnCancel := context.WithCancel(ctx)
	defer turnCancel()

	var (
		mu        sync.Mutex
		turnEnds  int
		hitCap    bool
		capReason string
	)

	unsub := eng.Bus.Subscribe("agent.turn.end", func(_ engine.Event[any]) {
		mu.Lock()
		turnEnds++
		if req.MaxTurns > 0 && turnEnds >= req.MaxTurns {
			hitCap = true
			capReason = fmt.Sprintf("max_turns=%d reached", req.MaxTurns)
			turnCancel()
		}
		mu.Unlock()
	})
	defer unsub()

	// Block until: session ended, MaxTurns hit, or ctx cancelled.
	select {
	case <-eng.SessionEnded():
		// Natural completion via nexus.io.test signalling io.session.end.
	case <-turnCtx.Done():
		mu.Lock()
		cap := hitCap
		mu.Unlock()
		if !cap {
			// turnCtx == ctx parent cancelled. Surface as TIMEOUT.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrTimeout("inspect-mode deadline exceeded before session end")
			}
			return nil, ErrTimeout("inspect-mode context cancelled before session end")
		}
		// MaxTurns cap hit — fall through to project the journal as-is.
	}

	// Stop before reading the journal so the writer flushes.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := eng.Stop(stopCtx); err != nil {
		stopCancel()
		eng.Logger.Warn("inspect-mode: engine stop failed", "error", err)
	} else {
		stopCancel()
	}

	resp, perr := projectJournal(journalDir)
	if perr != nil {
		return nil, ErrRunFailed(perr)
	}
	resp.Schema = SchemaVersion
	resp.SessionID = sessionID
	resp.Metadata = req.Metadata
	if resp.ToolCalls == nil {
		resp.ToolCalls = []ToolCall{}
	}
	_ = capReason // recorded in turn-end logs already; reserved for future surfacing

	return resp, nil
}

// resolveConfig picks the YAML body from the request: file path or inline.
// Path expansion uses engine.ExpandPath so `~` works the same way it does
// elsewhere.
func resolveConfig(req *Request) ([]byte, error) {
	if req.ConfigPath != "" {
		path := engine.ExpandPath(req.ConfigPath)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		return data, nil
	}
	return []byte(req.ConfigInline), nil
}

// overlayConfig surgically adjusts the user-supplied YAML to make the
// inspect run hermetic and headless:
//
//   - core.sessions.root → sessionsRoot (always overwritten)
//   - plugins.active includes nexus.io.test (added if absent; existing
//     visual plugins like nexus.io.tui / nexus.io.browser are removed
//     because they would block on stdin / serve HTTP)
//   - plugins.nexus.io.test gets a single-element `inputs:` populated
//     with userInput (preserves any other test-IO config the caller had)
//
// The yaml.v3 node API is used so comments and unrelated keys round-trip.
func overlayConfig(in []byte, sessionsRoot, userInput string) ([]byte, error) {
	var doc yaml.Node
	if len(in) == 0 {
		// Synthesize a minimal mapping.
		doc = yaml.Node{
			Kind: yaml.DocumentNode,
			Content: []*yaml.Node{
				{Kind: yaml.MappingNode, Tag: "!!map"},
			},
		}
	} else if err := yaml.Unmarshal(in, &doc); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		doc = yaml.Node{
			Kind: yaml.DocumentNode,
			Content: []*yaml.Node{
				{Kind: yaml.MappingNode, Tag: "!!map"},
			},
		}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("yaml root is not a mapping")
	}

	core := getOrCreateMap(root, "core")
	sessions := getOrCreateMap(core, "sessions")
	setScalar(sessions, "root", sessionsRoot)

	plugins := getOrCreateMap(root, "plugins")

	// Active list: ensure nexus.io.test is present, drop visual transports.
	active := getOrCreateSeq(plugins, "active")
	stripVisualTransports(active)
	if !sequenceContains(active, "nexus.io.test") {
		active.Content = append(active.Content, &yaml.Node{
			Kind: yaml.ScalarNode, Tag: "!!str", Value: "nexus.io.test",
		})
	}

	// nexus.io.test config: inputs = [userInput], approval_mode = approve,
	// timeout = 600s. We set timeout aggressively so a stuck turn cannot
	// block past the inspect-mode deadline (which lives one level up in ctx).
	testCfg := getOrCreateMap(plugins, "nexus.io.test")
	setInputs(testCfg, []string{userInput})
	setScalar(testCfg, "approval_mode", "approve")
	setScalar(testCfg, "timeout", "600s")

	return yaml.Marshal(&doc)
}

// projectJournal walks the live session's journal and assembles the
// wire-format response. The journal is the source of truth — by the time
// we read it, eng.Stop has flushed every envelope to disk.
func projectJournal(journalDir string) (*Response, error) {
	if journalDir == "" {
		return &Response{
			Schema:    SchemaVersion,
			ToolCalls: []ToolCall{},
		}, nil
	}
	r, err := journal.Open(journalDir)
	if err != nil {
		// Stop ran but journal never came up; produce an empty response
		// rather than failing the whole inspect call.
		return &Response{
			Schema:    SchemaVersion,
			ToolCalls: []ToolCall{},
		}, nil
	}

	var (
		firstTs       time.Time
		lastTs        time.Time
		finalMessage  string
		tokensIn      int
		tokensOut     int
		invokesByID   = map[string]*pendingInvoke{}
		toolCallsList = []ToolCall{}
	)

	err = r.Iter(func(env journal.Envelope) bool {
		if firstTs.IsZero() {
			firstTs = env.Ts
		}
		lastTs = env.Ts

		switch env.Type {
		case "llm.response":
			resp, ok := journalDecodeLLMResponse(env.Payload)
			if !ok {
				return true
			}
			tokensIn += resp.Usage.PromptTokens
			tokensOut += resp.Usage.CompletionTokens
			if isFinalAssistant(resp) {
				finalMessage = resp.Content
			}
		case "tool.invoke":
			call, ok := journalDecodeToolCall(env.Payload)
			if !ok {
				return true
			}
			id := call.ID
			if id == "" {
				// Synthesize a position-based id so the result side can
				// still pair via the same fallback when names match.
				id = fmt.Sprintf("_pos:%d", len(invokesByID))
			}
			invokesByID[id] = &pendingInvoke{
				name:    call.Name,
				args:    call.Arguments,
				invokeT: env.Ts,
				index:   len(toolCallsList),
			}
			toolCallsList = append(toolCallsList, ToolCall{
				Tool: call.Name,
				Args: call.Arguments,
			})
		case "tool.result":
			res, ok := journalDecodeToolResult(env.Payload)
			if !ok {
				return true
			}
			pending, ok := invokesByID[res.ID]
			if !ok {
				// Fallback: pair the most recent invoke with the same name
				// that has not yet been resolved. Defensive — under normal
				// agent behavior IDs match exactly.
				for id, p := range invokesByID {
					if p.name == res.Name && !p.resolved {
						pending = p
						_ = id
						break
					}
				}
			}
			if pending == nil {
				return true
			}
			pending.resolved = true
			summary := res.Output
			if res.Error != "" {
				if summary == "" {
					summary = "ERROR: " + res.Error
				} else {
					summary = summary + "\nERROR: " + res.Error
				}
			}
			toolCallsList[pending.index].ResultSummary = truncateForSummary(summary, resultSummaryCap)
			toolCallsList[pending.index].DurationMs = env.Ts.Sub(pending.invokeT).Milliseconds()
		}
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("iter journal: %w", err)
	}

	resp := &Response{
		Schema:                SchemaVersion,
		FinalAssistantMessage: finalMessage,
		ToolCalls:             toolCallsList,
		Tokens: Tokens{
			Input:  tokensIn,
			Output: tokensOut,
		},
	}
	if !firstTs.IsZero() && !lastTs.IsZero() {
		resp.LatencyMs = max(lastTs.Sub(firstTs).Milliseconds(), 0)
	}
	return resp, nil
}

// pendingInvoke tracks the in-progress invoke→result pairing while we
// stream the journal in seq order.
type pendingInvoke struct {
	name     string
	args     map[string]any
	invokeT  time.Time
	index    int
	resolved bool
}

// isFinalAssistant returns true when an llm.response is a "real" final
// assistant turn (the agent emitted text, not a tool-call-only step).
//
// FinishReason is normalized across providers ("end_turn", "stop", "STOP",
// or empty when the provider didn't supply one). When the response has
// content and no pending tool calls, treat it as final.
func isFinalAssistant(r events.LLMResponse) bool {
	if len(r.ToolCalls) > 0 {
		// Tool-call turn — not the final assistant message.
		return false
	}
	if r.Content == "" {
		return false
	}
	switch strings.ToLower(r.FinishReason) {
	case "", "end_turn", "stop", "endturn", "completed":
		return true
	default:
		// Other reasons (length, content_filter) — still surface as final
		// because nothing else will replace it; the caller can inspect raw
		// usage if they need to disambiguate.
		return true
	}
}

// truncateForSummary returns s capped at cap bytes. UTF-8-safe: if the cap
// lands inside a multi-byte rune, the rune is dropped. An ellipsis suffix
// is appended when truncation occurs.
func truncateForSummary(s string, cap int) string {
	if len(s) <= cap {
		return s
	}
	cut := cap
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	const ell = "…"
	if cut+len(ell) <= len(s) {
		return s[:cut] + ell
	}
	return s[:cut]
}

// -- yaml node helpers (local to avoid coupling with pkg/eval/runner/yaml.go) --

func getOrCreateMap(parent *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		k := parent.Content[i]
		if k.Value == key && k.Kind == yaml.ScalarNode {
			v := parent.Content[i+1]
			if v.Kind != yaml.MappingNode {
				v.Kind = yaml.MappingNode
				v.Tag = "!!map"
				v.Value = ""
				v.Content = nil
			}
			return v
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content, keyNode, valNode)
	return valNode
}

func getOrCreateSeq(parent *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		k := parent.Content[i]
		if k.Value == key && k.Kind == yaml.ScalarNode {
			v := parent.Content[i+1]
			if v.Kind != yaml.SequenceNode {
				v.Kind = yaml.SequenceNode
				v.Tag = "!!seq"
				v.Value = ""
				v.Content = nil
			}
			return v
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valNode := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	parent.Content = append(parent.Content, keyNode, valNode)
	return valNode
}

func setScalar(parent *yaml.Node, key, value string) {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		k := parent.Content[i]
		if k.Value == key && k.Kind == yaml.ScalarNode {
			v := parent.Content[i+1]
			v.Kind = yaml.ScalarNode
			v.Tag = "!!str"
			v.Value = value
			v.Content = nil
			return
		}
	}
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func setInputs(parent *yaml.Node, inputs []string) {
	seq := getOrCreateSeq(parent, "inputs")
	seq.Content = nil
	for _, s := range inputs {
		seq.Content = append(seq.Content, &yaml.Node{
			Kind: yaml.ScalarNode, Tag: "!!str", Value: s,
		})
	}
}

func sequenceContains(seq *yaml.Node, value string) bool {
	for _, n := range seq.Content {
		if n.Kind == yaml.ScalarNode && n.Value == value {
			return true
		}
	}
	return false
}

// stripVisualTransports removes nexus.io.tui / nexus.io.browser /
// nexus.io.wails entries from an active list. They block on stdin or
// serve a UI; the inspect-mode invocation must be headless.
func stripVisualTransports(seq *yaml.Node) {
	out := seq.Content[:0]
	for _, n := range seq.Content {
		if n.Kind != yaml.ScalarNode {
			out = append(out, n)
			continue
		}
		switch n.Value {
		case "nexus.io.tui", "nexus.io.browser", "nexus.io.wails", "nexus.io.oneshot":
			continue
		}
		out = append(out, n)
	}
	seq.Content = out
}
