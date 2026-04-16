# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability, please report it responsibly.

**Do not open a public issue.** Instead, email security concerns to the maintainer or use [GitHub's private vulnerability reporting](https://github.com/frankbardon/nexus/security/advisories/new).

Please include:

- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Suggested fix (if any)

You should receive a response within 72 hours. We'll work with you to understand the issue and coordinate a fix before any public disclosure.

## Scope

Security issues in the following areas are in scope:

- **Engine core** — Event bus, plugin lifecycle, session management
- **LLM providers** — API key handling, request/response security
- **Tool plugins** — Shell execution sandboxing, file I/O restrictions
- **Gates** — Prompt injection detection, content safety, PII detection
- **Desktop shell** — Keychain storage, settings persistence, IPC security

## Known Considerations

- **API keys**: Stored in environment variables or OS keychain (desktop). Never logged or persisted in plaintext session data.
- **Shell plugin**: Executes commands in a restricted sandbox. Allowed commands are explicitly configured per profile.
- **File I/O plugin**: Restricted to configured base directory. Path traversal is blocked.
