# Decision Providers

A **decision model** answers typed questions about a piece of state —
yes/no, pick-one-of, score-on-a-scale — with calibrated probabilities instead
of generated text. They are fast (typically well under a second) and suited to
routing, classification and triage.

They are deliberately **not** LLM providers. There are no messages, no tool
calls and no streaming, so they never ride `llm.request`. Instead a consumer
emits a `*events.DecisionAsk` on `decision.ask`, and the plugin advertising the
`decision.provider` capability fills in the answers before `Emit` returns —
the same pointer-fill pattern as `search.request`.

```go
ask := &events.DecisionAsk{
    SchemaVersion: events.DecisionAskVersion,
    State:         "I was charged twice and need this fixed today.",
    Questions: map[string]events.DecisionQuestion{
        "urgent": {Type: events.DecisionYesNo, Instructions: "Does this need action today?"},
        "team":   {Type: events.DecisionChoice, Instructions: "Which team owns it?",
            Choices: map[string]string{"billing": "payments, refunds", "technical": "bugs, outages"}},
    },
}
_ = bus.Emit("decision.ask", ask)
if ask.Error == "" && ask.Provider != "" {
    p := ask.Answers["urgent"].Probability // 0.96
    t := ask.Answers["team"].Choice        // "billing"
}
```

An empty `Provider` after `Emit` means no decision provider is active.

| Plugin | Backend |
|--------|---------|
| [`nexus.decision.jev`](./jev.md) | Jev-compatible System One API (local LM Studio, TypeSafe hosted, Vercel gateway) |

## Consumers

- [`nexus.router.classifier`](../../configuration/reference.md#nexusrouterclassifier)
  with `backend: decision` picks the model role for each turn with one
  `choice` question instead of an LLM probe.

## Not to be confused with Verdict

[Verdict](https://github.com/frankbardon/verdict)'s Nexus module ships
`nexus.decision.verdict`, which evaluates DMN decision models on
`decision.requested` / `decision.completed`. That is a rules engine
evaluating a model you authored; `decision.ask` asks a statistical model a
question. The two compose: a Verdict `agentDecision` node is a natural place
to put a decision-model call.
