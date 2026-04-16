# Installation

## Prerequisites

- **Go 1.21+** — Nexus is written in Go and builds with the standard toolchain
- **An Anthropic API key** — Required for the Claude LLM provider

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

Nexus reads the Anthropic API key from an environment variable. You can set it directly:

```bash
export ANTHROPIC_API_KEY="sk-ant-your-key-here"
```

Or place it in a `.env` file in the project root:

```
ANTHROPIC_API_KEY=sk-ant-your-key-here
```

The environment variable name is configurable per-provider. See the [Anthropic plugin configuration](../plugins/providers/anthropic.md) for details.

## Running Nexus

Run with a specific configuration profile:

```bash
bin/nexus -config configs/default.yaml
```

Nexus ships with several built-in profiles — see [Built-in Profiles](../configuration/profiles.md) for the full list.

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
