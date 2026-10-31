# Integration Testing

Nexus provides an integration test framework for automated validation of test configurations. Tests run real engines with real LLM calls, verifying end-to-end behavior.

## Quick Start

```bash
# Run all integration tests
go test -tags integration ./tests/integration/ -v

# Run a specific test
go test -tags integration ./tests/integration/ -run TestMinimal -v

# With timeout (tests make real API calls)
go test -tags integration ./tests/integration/ -timeout 5m -v
```

**Prerequisites:** `ANTHROPIC_API_KEY` must be set in the environment.

## Architecture

Three components work together:

1. **`nexus.io.test`** — IO plugin that replaces `nexus.io.tui` in test configs. Feeds scripted inputs, collects all events, handles approvals.
2. **`pkg/testharness`** — Go test helper that boots the engine, waits for completion, and provides assertion methods.
3. **Semantic judge** — Optional Haiku-based LLM judge for evaluating dynamic response content.

```
test config (YAML with nexus.io.test)
    ↓
Engine boots normally (real plugins, real LLM)
    ↓
Test IO plugin emits scripted io.input events
    ↓
Collects ALL bus events
    ↓
Go test assertions on collected events
    ↓
Optional: LLM-as-judge for semantic validation
```

## Writing Tests

### Basic Test

```go
//go:build integration

package integration

import (
    "testing"
    "time"

    "github.com/frankbardon/nexus/pkg/testharness"
)

func TestMyFeature(t *testing.T) {
    h := testharness.New(t, "configs/test-my-feature.yaml",
        testharness.WithTimeout(60*time.Second),
    )
    h.Run()

    h.AssertEventEmitted("io.output")
    h.AssertNoSystemOutput()
}
```

### Test Config

Test configs use `nexus.io.test` instead of `nexus.io.tui`:

```yaml
plugins:
  active:
    - nexus.io.test      # replaces nexus.io.tui
    - nexus.llm.anthropic
    - nexus.agent.react
    # ...

  nexus.io.test:
    inputs:
      - "Hello, who are you?"
    approval_mode: approve
    timeout: 60s
```

### Mock LLM Responses

Most gate and plugin tests don't need a real LLM. Use `mock_responses` to inject synthetic responses — no API key, no cost, millisecond execution:

```yaml
nexus.io.test:
  inputs:
    - "Include the word FORBIDDEN in your response."
  mock_responses:
    - content: "This should never be seen."
  timeout: 15s
```

Gates fire before mock responses (priority 10 vs 20), so gate behavior is tested accurately. The mock just replaces the expensive LLM call.

### Override Config Per Test

Use `copyConfig` to create a temp config with different inputs or settings:

```go
func TestStopWords(t *testing.T) {
    cfg := copyConfig(t, "configs/test-all-gates.yaml", map[string]any{
        "nexus.io.test": map[string]any{
            "inputs":        []string{"Include FORBIDDEN_WORD in your response."},
            "approval_mode": "approve",
            "timeout":       "30s",
        },
    })

    h := testharness.New(t, cfg, testharness.WithTimeout(45*time.Second))
    h.Run()

    h.AssertSystemOutputContains("Content blocked")
}
```

## Assertion Reference

### Tier 1: Deterministic (free, fast, reliable)

| Method | What it checks |
|--------|---------------|
| `AssertBooted(pluginIDs...)` | Plugins were initialized |
| `AssertEventEmitted(type)` | At least one event of this type |
| `AssertEventNotEmitted(type)` | No events of this type |
| `AssertEventCount(type, min, max)` | Event count within range |
| `AssertOutputContains(substring)` | Assistant output contains text |
| `AssertOutputNotContains(substring)` | Assistant output does not contain text |
| `AssertSystemOutputContains(substring)` | System-role output (gate messages) contains text |
| `AssertNoSystemOutput()` | No system-role outputs (no gate vetoes) |
| `AssertToolCalled(name)` | Tool was invoked |
| `AssertToolNotCalled(name)` | Tool was not invoked |
| `AssertSessionArtifact(relPath)` | File exists in session directory |

### Tier 2: Semantic (LLM judge, ~$0.001/assertion)

| Method | What it checks |
|--------|---------------|
| `AssertOutputSemantic(criteria)` | Haiku judges if output satisfies criteria |

Semantic assertions require `ANTHROPIC_API_KEY`. Tests are skipped (not failed) if no judge is configured.

```go
h.AssertOutputSemantic("response recalls the user's earlier question about greetings")
```

## Harness Options

| Option | Default | Purpose |
|--------|---------|---------|
| `WithTimeout(duration)` | 90s | Max time before harness gives up |
| `WithRetainSession()` | off | Keep the session dir (and the harness temp root) on failure for debugging; the path is logged |
| `WithKeepSession()` | off | Keep the session dir after `Run` so the test can read post-run artifacts; still removed when the test ends |
| `WithConfiguredRoots()` | off | Use the config's `core.sessions.root` and `core.storage.root` verbatim instead of the per-test temp dir |
| `WithConfigRewrite(fn)` | none | Transform the raw config bytes before the engine is built (repeatable, applied in order); also accepted by `NewEngine` |
| `WithJudge(judge)` | auto Haiku | Custom semantic judge implementation |
| `WithFailFast()` | off | Disable bus panic recovery so handler panics keep their stack trace |

## Session and Storage Roots

`testharness.New` keeps the engine's own state out of `~/.nexus`. Before
building the engine it rewrites the config's `core.sessions.root` to
`<tmp>/sessions` and `core.storage.root` to `<tmp>/data`, where `<tmp>` is a
fresh per-test temp dir (`h.TempRoot()`) removed when the test ends. Whatever
the YAML says is ignored, so runs never share session trees or app/agent
plugin SQLite across invocations. The `root: ~/.nexus/test-sessions` lines in
`configs/test-*.yaml` only matter when you run a test config by hand with
`bin/nexus -config`.

Pass `WithConfiguredRoots()` only when a test prepared a root itself and needs
the engine to find it again (for example, a hand-built session to recall).
A test that cannot use `Run` because something other than scripted
`nexus.io.test` inputs drives it (an AG-UI or A2A request against a serve
transport) builds its engine with `testharness.NewEngine(t, configPath)`,
which registers every plugin and applies the same redirect but leaves boot
and stop to the caller. Tests that construct the engine themselves with
`engine.NewFromBytes`, such as the journal replay, crash-resume and RAG
ingest tests, put `t.TempDir()` roots in their inline config.

Plugin-owned paths (`nexus.memory.longterm` `path`, `nexus.vectorstore.chromem`
`path`, ingest `cache_dir`, ...) are not rewritten. A test whose config writes
to one must override it with `t.TempDir()` through `copyConfig`, as the
longterm-memory, vector-memory, kitchen-sink and cross-provider RAG tests do.

## Loopback Ports

The serve-transport configs bind fixed loopback ports so they stay runnable by
hand: `configs/test-agui-*.yaml` use `127.0.0.1:18190`, `test-a2a-serve.yaml`
uses `127.0.0.1:18191`, and the `test-a2a-loopback-*.yaml` pair uses
`127.0.0.1:18192` (the caller's `base_url` included). Tests never bind them.
`tests/integration/ports_test.go` gives each top-level test its own free port
per default address (`aguiAddr(t)`, `a2aAddr(t)`, `a2aLoopbackAddr(t)`, shared
by that test's subtests) and rewrites every occurrence of the fixed address:
`bootEngine` and `portsOption(t)` (a `WithConfigRewrite` for
`testharness.New`) rewrite the config as it is loaded, and `copyConfig`
rewrites its copy. A run therefore cannot collide with a parallel `go test` or
with anything else on the host holding those ports. A new serve config should
reuse one of the three defaults, or add its own to `portDefaults`; a test that
needs an address with nothing listening uses `freeLoopbackAddr(t)`.

## Raw Event Access

For assertions not covered by built-in methods:

```go
for _, e := range h.Events() {
    if e.Type == "llm.response" {
        // inspect e.Payload
    }
}
```

## Build Tags

Integration tests use `//go:build integration` so they don't run with `go test ./...`. They require API keys and make real LLM calls.

```bash
# Unit tests only (default)
go test ./...

# Integration tests only
go test -tags integration ./tests/integration/

# Both
go test -tags integration ./...
```

## Testing UI Plugins (Future)

The test IO plugin validates the **bus contract** shared by all IO plugins (TUI, browser, wails). When integration tests pass, the bus-side behavior is validated.

Transport-specific rendering requires separate test suites per plugin:

| Plugin | Transport | Test Approach |
|--------|-----------|--------------|
| TUI | BubbleTea | `teatest` package (headless terminal simulation) |
| Browser | HTTP/WS | `httptest` + WebSocket client |
| Wails | Runtime bindings | Mock `runtime.Runtime` interface |

These validate that the transport correctly bridges bus events to/from the UI. The test IO plugin serves as a reference for the event flow, ordering, and payload shapes.

Future work: extract an `IOContract` interface from common subscription/emission patterns so any IO plugin can run a shared contract test suite.
