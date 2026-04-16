# Contributing to Nexus

Thanks for your interest in contributing to Nexus! This guide covers the basics.

## Getting Started

1. Fork the repository
2. Clone your fork: `git clone https://github.com/<you>/nexus.git`
3. Create a branch: `git checkout -b my-feature`
4. Make your changes
5. Run checks: `make test && make lint`
6. Commit and push
7. Open a pull request

## Development Setup

- Go 1.23+
- An LLM provider API key for integration testing
- [Wails v2 CLI](https://wails.io/docs/gettingstarted/installation) (only for desktop app work)

```bash
make build    # Build binary
make test     # Run tests
make lint     # Run staticcheck + vet
make fmt      # Format code
```

## Code Conventions

- **Structured logging** with `slog` everywhere
- **Error wrapping** with `fmt.Errorf("context: %w", err)`
- **Plugin IDs** use dotted namespace: `nexus.<category>.<name>`
- **Event types** use dotted namespace: `core.boot`, `llm.request`, etc.
- **No direct plugin-to-plugin calls** — all communication via the event bus
- **Minimal dependencies** — prefer stdlib over third-party packages

## Writing Plugins

Every plugin implements the `engine.Plugin` interface. See [Creating a Custom Plugin](docs/src/skills/custom-plugin.md) for a walkthrough.

Key rules:
- Plugins communicate only through the event bus
- Subscribe to events in `Init()`, unsubscribe in `Shutdown()`
- Use the `PluginContext` for config, logging, and session access
- Declare all subscriptions in `Subscriptions()` and emissions in `Emissions()`

## Pull Request Guidelines

- Keep PRs focused — one feature or fix per PR
- Include tests for new functionality
- Update documentation in `docs/` for user-facing changes
- Run `make test && make lint` before submitting
- Fill out the PR template

## Reporting Bugs

Use the [bug report template](https://github.com/frankbardon/nexus/issues/new?template=bug_report.yml). Include your config (redact API keys), Go version, and OS.

## Suggesting Features

Use the [feature request template](https://github.com/frankbardon/nexus/issues/new?template=feature_request.yml). Describe the problem you're solving, not just the solution you want.
