---
name: code-review
description: >-
  Review code for quality, bugs, security issues, and style.
  Use when the user asks for a code review or wants feedback
  on their code changes.
metadata:
  author: nexus
  version: "1.0"
---

# Code Review

## When to use
Use this skill when the user asks you to review code, check a PR, or provide feedback on code quality.

## Instructions
1. Read all changed files thoroughly before commenting
2. Check for these categories of issues:
   - **Bugs**: Logic errors, off-by-one, null/nil handling, race conditions
   - **Security**: Injection, XSS, hardcoded secrets, unsafe deserialization
   - **Performance**: Unnecessary allocations, N+1 queries, missing indexes
   - **Style**: Naming, formatting, idiomatic patterns for the language
   - **Design**: SOLID violations, coupling, missing abstractions (or premature ones)
3. Prioritize findings by severity (critical > major > minor > nit)
4. For each finding, explain the issue AND suggest a fix
5. Start with a high-level summary before detailed findings
6. Acknowledge what's done well -- reviews shouldn't be purely negative
