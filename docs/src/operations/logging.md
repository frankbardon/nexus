# Logging Levels

Nexus logs with stdlib `log/slog` only — no third-party logging
library. `core.log_level` (see the
[Configuration Reference](../configuration/reference.md)) sets the
engine-wide floor; every plugin logs through the `*slog.Logger` handed
to it on `PluginContext.Logger`.

This page is the level-assignment rubric: which of the five levels a
given log call belongs at. It exists so contributors have a standard
to log against instead of defaulting everything to `Info`. It is the
standard the log-levels-audit effort's reclassification stories apply
call site by call site — if you're adding or moving a log call
anywhere in the tree, this is the page to check.

## The five levels

| Level | When to use it |
|-------|-----------------|
| **TRACE** | Fires on every event or iteration — bus dispatch, every tool-call argument, wire payloads. Off even during normal debugging; only for that goes-nowhere-else last resort. |
| **DEBUG** | Useful while actively troubleshooting: plugin init detail, config resolution, which branch was taken, cache hit/miss. Frequent, but not so voluminous it drowns itself. |
| **INFO** | Ops-relevant lifecycle and state-change only: session start/stop, plugin activated, turn boundary. Explicitly **not** per-turn or per-tool-call chatter. |
| **WARN** | Unexpected but recovered or degraded: retry, fallback engaged, default assumed, stale cache used. |
| **ERROR** | Failed and unrecoverable — likely needs operator attention. |

Read top to bottom as increasing severity and decreasing volume: TRACE
should be the noisiest level by a wide margin, and ERROR the rarest.
If a call site logs on literally every agent iteration or every tool
call, it almost never belongs above DEBUG — and usually belongs at
TRACE.

## The TRACE emission convention

`*slog.Logger` only has convenience methods for the four standard
levels (`Debug`, `Info`, `Warn`, `Error`). `engine.LevelTrace` is a
plain `slog.Level` constant below `slog.LevelDebug`
(`pkg/engine/engine.go`), not a new method, so emitting at TRACE goes
through the generic `Log` method with an explicit level and a
`context.Context`:

```go
logger.Log(ctx, engine.LevelTrace, "msg", "key", val)
```

Use `context.Background()` (or a plugin's ambient request context) if
no richer context is available — `slog`'s `Log` signature requires one
regardless of whether the configured handler does anything with it.

`core.log_level: trace` in config parses to `engine.LevelTrace` via
`engine.ParseLogLevel`; any handler built on the standard
`slog.Handler` interface (including the engine's `FanoutHandler`,
`pkg/engine/loghandler.go`) passes the lower level through with no
extra wiring.

## Before/after examples

These are real call sites, shown as they read today and how the
rubric above would classify them. They're illustrative, not a
to-do list — the mechanical reclassification of existing call sites
across the tree is separate, later work; this page only establishes
the standard.

### TRACE vs. DEBUG: a per-request log

`plugins/providers/anthropic/plugin.go`, inside the code path that
resolves every outgoing LLM request:

```go
p.logger.Debug("resolving LLM request", "role", req.Role, "model", model, "max_tokens", maxTokens)
```

This fires once per LLM call — which, in a ReAct loop, is once per
agent iteration, not once per turn. Under the rubric that's "fires
every iteration," which is the TRACE bar, not DEBUG's. A call site
that a contributor would actually want on while troubleshooting a
single hard session (without drowning in per-iteration noise) belongs
at DEBUG; one that only earns its keep when inspecting every single
LLM round-trip belongs at TRACE:

```go
p.logger.Log(ctx, engine.LevelTrace, "resolving LLM request", "role", req.Role, "model", model, "max_tokens", maxTokens)
```

### DEBUG vs. INFO: per-turn chatter

`plugins/agents/react/plugin.go`, in the skill-context handler:

```go
p.logger.Info("loaded skill context", "name", content.Name)
```

Skill loading can happen multiple times within a single turn as the
agent pulls in different skills. INFO is reserved for ops-relevant
*lifecycle* events — session start/stop, plugin activation, turn
boundaries — and explicitly excludes per-turn chatter like this. It's
useful while troubleshooting which skills got pulled in, which is
exactly DEBUG's bar:

```go
p.logger.Debug("loaded skill context", "name", content.Name)
```

### INFO vs. WARN: a retry loop

`plugins/providers/anthropic/retry.go`, inside the HTTP retry loop:

```go
p.logger.Info("retrying API request",
	"attempt", attempt,
	"max_retries", rc.MaxRetries,
	"delay", delay,
)
```

A retry is the textbook WARN case from the rubric: something
unexpected happened (the first attempt failed) and the system is
recovering rather than failing outright. INFO would bury this among
routine lifecycle logging; WARN is where an operator scanning logs for
degraded behavior would look:

```go
p.logger.Warn("retrying API request",
	"attempt", attempt,
	"max_retries", rc.MaxRetries,
	"delay", delay,
)
```

## Applying this elsewhere

When you add a new log call or touch an existing one:

1. Ask "how often does this fire?" first — per-event/per-iteration
   pushes toward TRACE, per-turn toward DEBUG or INFO, per-session
   toward INFO.
2. Ask "would an operator watching production logs at the default
   level (`info`) want to see this?" If the answer is "only while I'm
   debugging," it's DEBUG or TRACE, not INFO.
3. Reserve WARN for degraded-but-recovered paths and ERROR for
   failures that need attention — don't use either for expected,
   successful control flow.
4. If in doubt between two adjacent levels, prefer the quieter one.
   The rubric's failure mode in the wild has been contributors
   defaulting to `Info`; erring toward DEBUG or TRACE keeps that from
   recurring.
