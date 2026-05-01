# Installation

## Prerequisites

- **Go 1.21+** — Nexus is written in Go and builds with the standard toolchain
- **An API key for at least one LLM provider** — Nexus ships with first-party providers for [Anthropic](../plugins/providers/anthropic.md) (Claude) and [OpenAI](../plugins/providers/openai.md) (GPT / o-series). Bring your own key for whichever provider(s) your config activates.

Optional:
- **poppler-utils** — Required only if you use the PDF reader plugin (`pdftotext`, `pdfinfo`)

## Building from Source

```bash
git clone https://github.com/frankbardon/nexus.git
cd nexus
make build
```

This produces a binary at `bin/nexus`.

## Available Make Targets

| Command | Description |
|---------|-------------|
| `make build` | Build binary to `bin/nexus` |
| `make run` | Build and run with the default config (`configs/default.yaml`) |
| `make test` | Run all tests |
| `make fmt` | Format code with `gofmt` |
| `make vet` | Run `go vet` |
| `make lint` | Run `staticcheck` (includes vet) |

## Setting Your API Key

Each provider plugin reads its key from an environment variable. The default names match the upstream convention: `ANTHROPIC_API_KEY` for the Anthropic plugin, `OPENAI_API_KEY` for the OpenAI plugin. Set whichever your active config needs:

```bash
# Using Claude
export ANTHROPIC_API_KEY="sk-ant-your-key-here"

# Or using OpenAI
export OPENAI_API_KEY="sk-your-key-here"
```

Or place them in a `.env` file in the project root:

```
ANTHROPIC_API_KEY=sk-ant-your-key-here
OPENAI_API_KEY=sk-your-key-here
```

You can also pass the key inline (`api_key:`) or point at a different env var (`api_key_env:`) per-provider. See the [Anthropic](../plugins/providers/anthropic.md) and [OpenAI](../plugins/providers/openai.md) plugin pages for the full options, plus [Fallback](../plugins/providers/fallback.md) and [Fanout](../plugins/providers/fanout.md) for using multiple providers together.

## Running Nexus

Run with a specific configuration file:

```bash
bin/nexus -config configs/default.yaml
```

### CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-config` | `nexus.yaml` | Path to the YAML configuration file |
| `-recall` | *(none)* | Session ID to recall and resume a previous session |

### Resuming a Session

To resume a previous session, pass the session ID:

```bash
bin/nexus -recall abc123def456
```

This loads the session's config snapshot so the agent starts with the same configuration it had originally.
