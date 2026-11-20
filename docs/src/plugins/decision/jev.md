# Jev

`nexus.decision.jev` is a `decision.provider` backed by a Jev-compatible
**System One** API: state plus typed questions in, typed answers with
probabilities out. See [Decision Providers](./index.md) for the event
contract.

## Where the model runs

TypeSafe AI's proprietary Jev model is only available through their hosted
API — its weights are not distributed. The same wire shape is served by:

| Host | `endpoint` | `dialect` | Key |
|------|------------|-----------|-----|
| **LM Studio** (local, default) with an open Jev-compatible GGUF model loaded | `http://localhost:1234/v1/systemone` | `systemone` | none |
| TypeSafe AI hosted API | the System One URL from your TypeSafe console | `systemone` | `JEV_API_KEY` |
| Vercel AI Gateway | `https://ai-gateway.vercel.sh/v1/evaluate` | `vercel` | gateway key |

The `vercel` dialect only changes the yes/no question type sent (`boolean`
instead of `noul`); responses are parsed from either `noul` or `probability`
whichever the server returns.

## Configuration

Local LM Studio:

```yaml
plugins:
  active:
    - nexus.decision.jev
    - nexus.router.classifier

nexus.decision.jev:
  # endpoint defaults to http://localhost:1234/v1/systemone
  model: apus-openjev-4b   # whatever LM Studio has loaded; omit to use its default
  timeout: 5s

nexus.router.classifier:
  backend: decision
  candidate_roles: [quick, balanced, reasoning]
  fallback_role: balanced
  min_confidence: 0.6
  role_descriptions:
    quick: short factual or conversational replies
    balanced: everyday coding and writing tasks
    reasoning: multi-step reasoning, complex tool use, long documents
```

Hosted:

```yaml
nexus.decision.jev:
  endpoint: https://api.typesafe.ai/v1/systemone
  model: jev-latest
  api_key_env: JEV_API_KEY
```

Every key is listed in the
[configuration reference](../../configuration/reference.md#nexusdecisionjev).

## Behaviour

- Question types map `yes_no` → `noul`/`boolean`, `choice` → `choice` with
  `Choices` as criteria, `score` → `score` with `Levels` as criteria
  (2–10 levels, enforced before the call). Jev's experimental `box` type is not
  exposed.
- A non-2xx response, an undecodable body, or a response missing an answer for
  any asked key sets `DecisionAsk.Error`; the plugin never panics the bus.
- An ask whose `Provider` is already set is left untouched, so a pinned or
  earlier provider wins.
- Token usage and the served model version are copied onto the payload when
  the server reports them.
