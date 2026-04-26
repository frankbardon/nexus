---
name: competitor-brief
description: >-
  Produce a structured competitor brief in JSON. Use when the user asks for
  a "brief", "summary", "one-pager", or "overview" of a specific competitor,
  product, or company.
class: writing
subclass: structured-output
tags: [brief, competitor, structured, json]
allowed_tools:
  - knowledge_search
  - file_write
metadata:
  author: nexus-demo
  version: "1.0"
output_schema:
  type: object
  required: [competitor, headline, positioning, strengths, weaknesses, sources]
  additionalProperties: false
  properties:
    competitor:
      type: string
      description: Canonical name of the competitor (matches KB if possible).
    headline:
      type: string
      maxLength: 160
      description: One-sentence executive summary suitable for a slide title.
    positioning:
      type: string
      description: How the competitor positions itself in the market — pricing tier, target persona, primary value prop.
    strengths:
      type: array
      minItems: 2
      maxItems: 5
      items: { type: string }
    weaknesses:
      type: array
      minItems: 2
      maxItems: 5
      items: { type: string }
    pricing:
      type: string
      description: Free-form pricing summary; "unknown" if no source available.
    sources:
      type: array
      minItems: 1
      items:
        type: object
        required: [type, ref]
        additionalProperties: false
        properties:
          type:
            type: string
            enum: [kb, web]
          ref:
            type: string
            description: For type=kb, the source path. For type=web, the URL.
---

# Competitor Brief

## When to use
The user asks for a structured brief, one-pager, or summary of a specific
competitor, product, or company. They want a deliverable, not a conversation.

## Instructions
1. Search the KB first via `knowledge_search` for the competitor name.
2. If the KB has solid coverage, draft entirely from KB sources.
3. If the KB is thin, tell the user explicitly and (only if they confirm)
   suggest they ask the Researcher to enrich the KB first.
4. Produce ONLY the JSON object matching the output schema. No preamble,
   no Markdown wrapper. The schema gate will block non-conforming output
   and ask you to retry.
5. After the user accepts the JSON, optionally use `file_write` to save it
   to `<output_dir>/<competitor-slug>.json`.

## Style
- Headlines are punchy, ≤ 160 chars, no hedging.
- Strengths/weaknesses are concrete behaviours/features, not vibes.
- Every claim must have a corresponding entry in `sources`.
