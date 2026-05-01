---
name: git-workflow
description: >-
  Guide git workflows including branching, committing, merging, and PR creation.
  Use when the user needs help with git operations or wants to follow
  git best practices.
metadata:
  author: nexus
  version: "1.0"
---

# Git Workflow

## When to use
Use this skill when the user asks about git operations, branching strategies, commit practices, or PR workflows.

## Instructions
1. Before making git changes, always check `git status` and `git log` first
2. Follow conventional commit format: `type(scope): description`
   - Types: feat, fix, docs, style, refactor, test, chore
3. Create feature branches from main: `feature/<short-description>`
4. Keep commits atomic -- one logical change per commit
5. Before pushing, run tests and verify the build passes
6. For PRs:
   - Write a clear title (under 70 chars)
   - Include a summary of changes
   - Reference related issues
   - Add a test plan
7. Never force-push to shared branches
8. Resolve merge conflicts by understanding both sides before choosing
