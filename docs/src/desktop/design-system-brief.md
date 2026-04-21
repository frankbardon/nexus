# Nexus Desktop Design System — Priming Brief

> Paste this whole document into Claude Design as a system/priming prompt. It describes the existing desktop UI system so any new screens or components remain consistent with what's already shipped.

## What you are working on

You are designing UI for **Nexus desktop apps** — Go/Wails-based native apps that embed AI agents. Each desktop app hosts one or more "agents" (domain-specific AI workflows) in a consistent shell. Examples in the repo: a multi-agent reference app (hello-world + staffing-match) and a single-agent phased workflow app (tech lead).

Your job is to design UI that slots into this existing system without reinventing it. Prefer existing primitives and class recipes over new ones.

## Tech stack (non-negotiable)

- **Tailwind CSS** via CDN (`https://cdn.tailwindcss.com`)
- **DaisyUI 4** as the component layer (`https://cdn.jsdelivr.net/npm/daisyui@4/dist/full.min.css`)
- **Font Awesome 6.5.1** (free, solid style by default: `fa-solid`)
- **AlpineJS 3** for state and reactivity (`Alpine.store`, `x-data`, `x-show`, `x-for`, `x-transition`, `x-model`)
- **No build step.** Everything ships as a single `index.html` per app with inline `<script>` and `<style>`.
- **No other libraries.** No React, Vue, Svelte, or component kits. No CSS-in-JS.

Always use DaisyUI utility classes first (`btn`, `card`, `input`, `alert`, `badge`, `collapse`, `loading`, `textarea`, `toggle`, `select`). Fall back to raw Tailwind only for layout, spacing, and DaisyUI gaps.

## Theme

- **Dark theme is default and primary.** `<html data-theme="dark">`. Light mode is not currently supported — don't design light-first mockups.
- **Semantic color tokens** come from DaisyUI: `primary`, `secondary`, `accent`, `success`, `warning`, `error`, `info`, `base-100` (app bg), `base-200` (panel bg), `base-300` (borders/hover), `base-content` (text).
- **Opacity is the workhorse for hierarchy.** Use `text-base-content` at `/40` `/50` `/60` `/80` instead of introducing new grays. Example: section headers use `opacity-50`, body meta uses `opacity-40`.
- **Primary color is used sparingly** — reserved for active nav items, primary CTAs, and "this is the focused thing" accents. Common active-state pattern: `bg-primary/15 text-primary`, icon slot `bg-primary/20`.
- **Status colors**: `success` = done/running-healthy, `warning` = booting/attention, `error` = failed/destructive, `info` = neutral context, `primary` = active/pulsing.

## Typography

- Base font = Tailwind default sans-serif. Monospace (`font-mono`) is used for file paths, IDs, token counts, and code/specs.
- **Section headers**: `text-xs font-semibold uppercase tracking-wider opacity-50` — used for "SESSIONS", "ARTIFACTS", "FILES", "GENERAL", etc.
- **Page title**: `font-semibold text-base leading-tight` in headers, `text-xl font-bold` for phase/view titles in main content.
- **Labels**: `text-sm font-medium` for form labels; `text-xs opacity-50` for field descriptions/helper text.
- **Body text**: `text-sm` default; `text-xs` for meta, timestamps, counts.
- **Never use custom font sizes in px.** Stick to Tailwind's `text-[10px]`, `text-xs`, `text-sm`, `text-base`, `text-lg`, `text-xl`.

## Layout archetypes

Two shell archetypes exist. Pick the one that matches the app and keep within it.

### Archetype A — Multi-agent shell (horizontal)

```
┌─────┬──────────┬───────────────────┬────────┐
│ Nav │ Sessions │ Main              │ Files  │
│ 64↔ │  256px   │ flex-1            │ 288px  │
│220px│ (cond.)  │                   │(toggle)│
└─────┴──────────┴───────────────────┴────────┘
```

- **Left nav** (`w-16` collapsed ↔ `w-[220px]` expanded, `bg-base-200 border-r border-base-300`): logo + agent list + settings. Each agent row is an icon with a status dot and an optional label. Active agent uses `bg-primary/15 text-primary`.
- **Sessions panel** (`w-64`, `bg-base-200/50 border-r border-base-300`): visible when an agent is active. Conditional via `x-show`.
- **Main** (`flex-1`): per-agent content. Header on top with title, description, and inline action buttons. The header is draggable (`wails-drag`).
- **File browser** (`w-72`, `bg-base-200/50 border-l border-base-300`): toggleable right panel rooted in the agent's `input_dir`.

### Archetype B — Single-agent phased shell (vertical)

```
┌──────────────────────────────────────────┐
│  ① ━ ② ━ ③ ━ ④ ━ ⑤     [sessions] [⚙]  │  ← phase stepper bar
├──────────┬─────────────────┬─────────────┤
│ Sessions │  Main content   │ (optional)  │
│ 224px    │  flex-1         │             │
│ (toggle) │                 │             │
├──────────┴─────────────────┴─────────────┤
│ Artifact tree (224px, left)              │
└──────────────────────────────────────────┘
```

- **Phase stepper bar** on top (`bg-base-200 border-b border-base-300 px-6 py-3`, also `wails-drag`): numbered step chips with connector lines. Active step = `bg-primary text-primary-content`, completed = `bg-success/20 text-success` with ✓, future = `text-base-content/40 cursor-default`.
- **Artifact tree** (`w-56`): grouped list of generated artifacts (documents, epics → stories nested, notes, timeline, comms). Each group has an uppercase section header.
- **Main** switches content per active phase via `x-show="$store.reqflow.phase === N"`.

## Shell chrome (apply to both archetypes)

- **Dragging the window** uses the class `wails-drag` (with CSS `--wails-draggable: drag;`). Put it on the topmost header bar.
- **Body**: `bg-base-100 text-base-content`, `overflow-hidden`, root is `h-screen flex` (horizontal shells) or `h-screen flex flex-col` (vertical shells).
- **Panels**: `bg-base-200/50`, borders via `border-base-300`. Don't invent new panel backgrounds.
- **Transitions** for panel enter/exit:
  ```html
  x-transition:enter="transition ease-out duration-150"
  x-transition:enter-start="opacity-0 -translate-x-2"
  x-transition:enter-end="opacity-100 translate-x-0"
  ```
  150ms ease-out, 8px slide + fade. Use `translate-x-2` for right-side panels entering from the right.

## Component recipes (memorize these)

### Buttons

- **Primary action**: `btn btn-primary btn-sm`
- **Secondary/tertiary**: `btn btn-ghost btn-sm` (common) or `btn btn-outline btn-sm`
- **Icon-only square**: `btn btn-ghost btn-sm btn-square` (toolbar), `btn btn-ghost btn-xs btn-square` (in-row)
- **Destructive inline**: `btn btn-ghost btn-xs btn-square hover:text-error` or `... text-error`
- **Inside a button**: icon first (`<i class="fa-solid fa-... text-xs">`), then a label span. Loading state swaps icon for `<span class="loading loading-spinner loading-xs">`.
- **Size ladder**: `btn-xs` (dense toolbars, in-row actions) → `btn-sm` (default) → `btn` (rare, page-level CTA).

### Nav/list items (active/hover pattern)

Reused across the agent nav, session list, artifact tree, and file list:

```html
<button
  class="w-full flex items-center gap-3 px-3 py-2.5 rounded-lg transition-colors text-left group"
  :class="isActive
    ? 'bg-primary/15 text-primary'
    : 'hover:bg-base-300 text-base-content/70 hover:text-base-content'">
  <div class="w-8 h-8 rounded-lg flex items-center justify-center"
       :class="isActive ? 'bg-primary/20' : 'bg-base-300'">
    <i class="fa-solid fa-icon text-sm"></i>
  </div>
  <div class="min-w-0 flex-1">
    <div class="text-sm font-medium truncate">Label</div>
    <div class="text-xs opacity-50 truncate">Sub-label</div>
  </div>
</button>
```

### Status dots

```html
<div class="w-2.5 h-2.5 rounded-full border-2 border-base-200"
     :class="{
       'bg-success': status === 'running',
       'bg-warning animate-pulse': status === 'booting',
       'bg-error': status === 'error'
     }"></div>
```

Small-size variant for list rows: `w-1.5 h-1.5 rounded-full`.

### Cards

```html
<div class="card bg-base-200 border border-base-300 hover:border-primary/40 transition-colors">
  <div class="card-body p-4">...</div>
</div>
```

### Badges

- Count/label: `badge badge-sm`
- Semantic: `badge-success`, `badge-warning`, `badge-error`, `badge-primary`, `badge-ghost`
- Tiny inline: `badge badge-xs`

### Inputs

- Text: `input input-bordered input-sm w-full`
- Textarea: `textarea textarea-bordered w-full text-sm` (add `min-h-[7rem]` for multi-line prompts)
- Select: `select select-bordered select-sm w-full`
- Toggle: `input type="checkbox" class="toggle toggle-primary toggle-sm"`
- Secret (password): same input, add an eye-toggle button absolutely positioned right (`absolute right-2 top-1/2 -translate-y-1/2 btn btn-ghost btn-xs btn-circle`).
- Invalid/required: add `input-warning` class when the field is required and empty.

### Alerts

- Error: `alert alert-error py-2 text-sm` with leading `<i class="fa-solid fa-triangle-exclamation"></i>`
- Warning/missing-config: `alert alert-warning`
- Inline error under a form: `text-xs text-warning` with `<i class="fa-solid fa-circle-exclamation">`

### Empty states

```html
<div class="flex items-center justify-center h-full">
  <div class="text-center max-w-sm">
    <div class="w-16 h-16 rounded-2xl bg-primary/10 flex items-center justify-center mx-auto mb-4">
      <i class="fa-solid fa-icon text-primary text-2xl"></i>
    </div>
    <h2 class="text-lg font-semibold mb-1">Short headline</h2>
    <p class="text-sm opacity-60">One-sentence explainer with a call to action.</p>
  </div>
</div>
```

Compact variant for narrow panels: drop the icon tile to `text-2xl opacity-20`, text to `text-xs opacity-40`, center vertically in the panel.

### Loading states

- Inline (inside a button): `loading loading-spinner loading-xs`
- Panel-level: `loading loading-spinner loading-sm opacity-40`
- Hero / boot overlay: `loading loading-spinner loading-lg text-primary` centered with a subtitle
- "Thinking…" dots: `loading loading-dots loading-sm`
- Full-screen boot overlay: `absolute inset-0 flex items-center justify-center bg-base-100/80 z-10`

### Collapsible sections (settings)

DaisyUI collapse: `collapse collapse-arrow bg-base-200 border border-base-300`, with `<input type="checkbox" checked>` inside to drive state. Title row: `collapse-title font-semibold flex items-center gap-2` with a leading icon.

### Drag-and-drop

Drop overlay (appears while dragging):
```html
<div class="absolute inset-0 z-20 bg-primary/10 border-2 border-dashed border-primary rounded-lg flex items-center justify-center pointer-events-none">
  <div class="text-center">
    <i class="fa-solid fa-cloud-arrow-up text-primary text-2xl mb-2"></i>
    <p class="text-sm font-medium text-primary">Drop files here</p>
  </div>
</div>
```

Persistent drop zone (phase 1 import style):
```html
<div class="border-2 border-dashed border-base-300 rounded-xl p-8 text-center transition-all"
     :class="{ 'drop-active': dragOver }">
  <i class="fa-solid fa-cloud-arrow-up text-4xl text-base-content/20 mb-4 block"></i>
  <p class="text-base-content/40 mb-3">Drop markdown files here</p>
  <button class="btn btn-sm btn-primary">Browse Files</button>
</div>
```

### Chat messages (agent conversation)

- User: right-aligned, `bg-primary text-primary-content` bubble, no avatar or avatar on the right
- Assistant: left-aligned, `bg-base-200` bubble, avatar is `w-8 h-8 rounded-full bg-primary/20` with `fa-robot` icon
- Note/annotation: `bg-warning/10 border border-warning/20` bubble, `fa-sticky-note` avatar
- Max-width bubble: `max-w-[75%] rounded-lg px-4 py-3`
- Auto-scroll on new message via `x-effect="$refs.chatScroll && ($refs.chatScroll.scrollTop = $refs.chatScroll.scrollHeight)"`

## Interaction rules

- **Hover reveal**: destructive or secondary actions inside list rows use `opacity-0 group-hover:opacity-100 transition-opacity`. Never show delete buttons unconditionally.
- **Keyboard**: chat textareas support `@keydown.enter.meta="send()"` and `@keydown.enter.ctrl="send()"` (Cmd/Ctrl+Enter). Show the hint as `text-xs opacity-30` below the input.
- **Required-field gating**: when required settings are missing, show a `alert alert-warning` banner at the top of settings, and mark each missing field with an inline `text-xs text-warning` "Required" hint and `input-warning` class.
- **Restart nudge**: when a running agent has dirty settings, show `badge badge-warning badge-xs` "restart required" on the section and a `btn btn-warning btn-sm` at the bottom.

## Data flow (so designs match reality)

- All agent domain communication flows through a scoped event bus bridge (`createBus(agentID)`), not Wails-bound methods. Your designs should assume **async, event-driven updates** — never blocking waits.
- The shell owns: agent lifecycle (boot/stop), sessions, settings, file dialogs, file browser, drag-and-drop.
- The agent owns: its own UI section, its own Alpine store, its own input/output events.
- State that survives session recall lives in `ui.state.save`/`ui.state.restore` events — designs should gracefully rehydrate partial state.

## What NOT to do

- Don't introduce a new CSS framework, component library, or JS framework. Alpine + Tailwind + DaisyUI is the entire toolbox.
- Don't add light mode without checking first — everything assumes dark.
- Don't invent new named colors. Stick to the DaisyUI semantic tokens.
- Don't use raw gray scales (`text-gray-500`). Use `opacity-*` on `text-base-content`.
- Don't add routing. Views switch via `x-show` against a single activeView string.
- Don't design multi-page flows with URLs. Everything is SPA-style, single `index.html`.
- Don't add modals/dialogs casually — the existing apps don't use them. Use inline collapsibles, side panels, or dedicated views instead. If you need a confirm, use inline "Are you sure?" patterns.
- Don't design for mobile or narrow widths. Desktop only, min width roughly 900×720.
- Don't add animations beyond DaisyUI `animate-pulse` and the existing 150ms panel transitions. No hero scroll effects, no motion beyond utility.
- Don't add icons from other sets — Font Awesome 6 solid only.

## When proposing something new

If your design needs a component not listed above, say so explicitly and propose either (a) a DaisyUI component that isn't yet used, or (b) a raw-Tailwind recipe that matches the existing visual weight. Flag it as a net-new primitive so we can decide whether to adopt it.

## Quick reference — the 10 classes you'll use constantly

| Purpose | Class |
|---|---|
| Active list item | `bg-primary/15 text-primary` |
| Hover list item | `hover:bg-base-300 text-base-content/70 hover:text-base-content` |
| Panel background | `bg-base-200/50` |
| Panel border | `border-base-300` |
| Section header | `text-xs font-semibold uppercase tracking-wider opacity-50` |
| Meta text | `text-xs opacity-50` |
| Primary CTA | `btn btn-primary btn-sm` |
| Icon button | `btn btn-ghost btn-sm btn-square` |
| Card | `card bg-base-200 border border-base-300 hover:border-primary/40` |
| Empty state icon tile | `w-16 h-16 rounded-2xl bg-primary/10` |
