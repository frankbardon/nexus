# Nexus Desktop Design System — Full Reference

> Comprehensive reference for the Nexus desktop UI system. Derived from the shipped frontends at [cmd/desktop/frontend/dist/index.html](../../../cmd/desktop/frontend/dist/index.html) and [cmd/techlead/frontend/dist/index.html](../../../cmd/techlead/frontend/dist/index.html), plus the shell framework at [pkg/desktop/](../../../pkg/desktop/). Pair with [design-system-brief.md](design-system-brief.md) for a shorter priming version.

---

## 1. Scope and context

Nexus ships desktop apps via **Wails v2** — each app is a Go binary wrapping a webview. The webview loads a single `index.html` containing the entire UI. Two kinds of apps exist today:

- **Multi-agent shells** (reference: `cmd/desktop`) — host several domain-specific agents under one roof, each with its own UI section.
- **Single-agent workflow apps** (reference: `cmd/techlead`) — one agent, one workflow, typically organized as a phased progression.

Both share the same visual language, component vocabulary, and tech stack. A designer working on either should be able to port patterns between them without friction.

### Integration with the engine

The UI does not directly "call" the agent. All domain communication flows through a **scoped event bus bridge** (`createBus(agentID)`) that wraps Wails event APIs. Every design should assume:

- Actions emit bus events; results arrive as inbound bus events (asynchronous).
- The shell (not the agent) owns file dialogs, the file browser, settings, and session management — these appear in UI via Wails-bound methods on the shell.
- UI state that must survive session recall is saved via `ui.state.save` events and rehydrated via `ui.state.restore`.

### What doesn't exist yet

- No light theme. Everything assumes `data-theme="dark"`.
- No modals or dialog components are in use. New screens should avoid them unless explicitly requested.
- No routing library. Views switch via Alpine stores against `activeView`/phase number.
- No component library beyond DaisyUI. No icon set beyond Font Awesome 6 solid.
- No responsive/mobile layouts. Min window size ~900×720.

---

## 2. Tech stack

| Layer | Choice | Source |
|---|---|---|
| CSS framework | Tailwind CSS | `https://cdn.tailwindcss.com` |
| Component layer | DaisyUI 4 | `https://cdn.jsdelivr.net/npm/daisyui@4/dist/full.min.css` |
| Icons | Font Awesome 6.5.1 Free | `https://cdnjs.cloudflare.com/ajax/libs/font-awesome/6.5.1/css/all.min.css` |
| JS reactivity | AlpineJS 3 | `https://cdn.jsdelivr.net/npm/alpinejs@3/dist/cdn.min.js` |
| Shell runtime | Wails v2 | Go module `github.com/wailsapp/wails/v2` |
| Desktop chrome | System native (macOS/Windows/Linux) via Wails |  |

Everything is CDN-loaded; there is no bundler, no npm install, no TypeScript, no build step. A new frontend is one self-contained `index.html`.

---

## 3. Design tokens

### 3.1 Color tokens (DaisyUI semantic)

Never introduce raw gray scales. Use DaisyUI's semantic tokens.

| Token | Purpose | Typical use |
|---|---|---|
| `primary` | Brand / active / focus | Active nav, primary CTA, selected item |
| `secondary` | Alt accent | Story IDs, tertiary badges |
| `accent` | Third accent | Epic IDs |
| `success` | Healthy / complete | Status dots (running healthy), completed phase checks |
| `warning` | Attention / boot | Status dot (booting), unsaved marker, required-field highlight |
| `error` | Failure / destructive | Error alerts, failed status, destructive hover |
| `info` | Neutral context | Document icons in artifact tree |
| `base-100` | App background | `bg-base-100` on body and main content |
| `base-200` | Panel background | Nav sidebars, session panel, file panel, cards |
| `base-300` | Borders, hover surfaces | `border-base-300`, `hover:bg-base-300` |
| `base-content` | Primary text | Body copy, labels |

**Opacity for hierarchy** — the conventional ladder:

| Token | Where used |
|---|---|
| `text-base-content` (100%) | Primary body text, button labels |
| `text-base-content/80` | Body text one step de-emphasized |
| `text-base-content/70` | Unfocused nav/list item labels |
| `text-base-content/60` | Secondary prose, helper text |
| `opacity-50` | Meta text, section headers, non-active icons |
| `opacity-40` | Timestamps, sub-counts |
| `opacity-30` | Path hints, "not configured" text |
| `opacity-20` | Empty state background icons, disabled glyphs |

**Common `/N` accent patterns:**

| Pattern | Meaning |
|---|---|
| `bg-primary/10` | Background wash for empty state icon tiles |
| `bg-primary/15 text-primary` | Active nav/list row state |
| `bg-primary/20` | Active icon slot background, avatars |
| `border-primary/40` | Hover border on cards |
| `bg-primary/5` | Drag-over wash (very subtle) |
| `bg-warning/10 border-warning/20` | Note/annotation bubbles |
| `bg-warning/30` | Completed-phase step marker circle |
| `bg-success/20 text-success` | Completed-phase step button |

### 3.2 Spacing scale

Follows Tailwind's default 4px-based scale. Conventions observed:

| Context | Padding |
|---|---|
| Panel headers | `px-3 py-3` (256-column panels) or `px-6 py-3` (main-content headers) |
| List item | `px-2.5 py-2` to `px-3 py-2.5` |
| Card body | `card-body p-4` |
| Form sections | `space-y-4` between fields, `space-y-6` between major sections |
| Section top padding | `py-5` for content areas, `py-3` for toolbars |
| Collapsed nav | `w-16` / `w-[64px]` |
| Expanded nav | `w-[220px]` |
| Session panel | `w-64` (256px) in multi-agent, `w-56` (224px) in single-agent |
| File panel | `w-72` (288px) |
| Artifact tree | `w-56` (224px) |

### 3.3 Typography scale

All sizes come from Tailwind defaults. Do not use custom px.

| Class | Pixel | Use |
|---|---|---|
| `text-[10px]` | 10 | Micro-labels inside nested artifact trees |
| `text-xs` | 12 | Meta, timestamps, sub-labels, section headers |
| `text-sm` | 14 | Default body, form inputs, list item labels |
| `text-base` | 16 | Content title in header |
| `text-lg` | 18 | Empty-state headline, page title in detail view |
| `text-xl` | 20 | Phase title (`h2` for active phase/view) |
| `text-2xl` | 24 | Rare — hero icons, boot screen |
| `text-4xl` | 36 | Empty-state background icon |

Weight conventions: `font-medium` for item labels, `font-semibold` for headers, `font-bold` for `h2` page titles.

Font family: Tailwind's default sans-serif. `font-mono` for: file paths, artifact IDs (epic/story IDs like `E-001`), token counts, USD cost, code, raw markdown, session preview paths.

### 3.4 Radii

| Class | Pixel | Use |
|---|---|---|
| `rounded` | 4 | Tight micro-chips |
| `rounded-md` / `rounded` | 4-6 | Tight list rows inside trees |
| `rounded-lg` | 8 | Default for nav items, cards, message bubbles, panels inside content |
| `rounded-xl` | 12 | Drop zones, larger panels |
| `rounded-2xl` | 16 | Empty-state icon tiles (`w-16 h-16 rounded-2xl`) |
| `rounded-full` | 999 | Status dots, avatars |

### 3.5 Shadows, transitions, motion

- Default transition on interactive surfaces: `transition-colors`.
- Panel enter/exit: `transition ease-out duration-150` with an 8px slide + fade.
- Hover card: `transition-shadow` + `hover:shadow-md` on epic cards, `hover:border-primary/40` elsewhere.
- Status pulse: `animate-pulse` on booting/running indicator dots.
- Loading spinner: DaisyUI `loading loading-spinner`, never custom keyframes.
- `loading-dots` for chat "thinking" indicator.
- No parallax, no hero scroll effects, no Framer Motion.

---

## 4. Shell archetypes

### 4.1 Multi-agent horizontal shell

```
┌─────┬──────────┬─────────────────────────┬────────┐
│ Nav │ Sessions │ Header (wails-drag)     │  Files │
│     │          ├─────────────────────────┤        │
│     │          │ Main content            │        │
│     │          │                         │        │
│     │          │                         │        │
└─────┴──────────┴─────────────────────────┴────────┘
  64px     256px    flex-1                    288px
  (coll.)  (cond.)                           (toggle)
```

Skeleton:

```html
<body class="bg-base-100 text-base-content">
  <div x-data class="h-screen flex" x-init="$store.shell.init()">
    <nav class="nav-sidebar flex-shrink-0 bg-base-200 border-r border-base-300 flex flex-col h-full"
         :style="{ width: $store.shell.collapsed ? '64px' : '220px' }">...</nav>

    <aside x-show="activeAgent" class="flex-shrink-0 w-64 bg-base-200/50 border-r border-base-300 flex flex-col h-full">
      <!-- Sessions panel -->
    </aside>

    <div class="flex-1 flex flex-col min-w-0 h-full">
      <header class="px-6 py-3 border-b border-base-300 flex items-center gap-3 bg-base-100 wails-drag">...</header>
      <div class="flex-1 flex overflow-hidden relative">
        <main class="flex-1 overflow-hidden relative">...</main>
        <aside x-show="filesOpen" class="flex-shrink-0 w-72 bg-base-200/50 border-l border-base-300 flex flex-col h-full">
          <!-- File panel -->
        </aside>
      </div>
    </div>
  </div>
</body>
```

Key properties:

- `body { overflow: hidden; }` — the UI is not scrolled; individual panels manage their own overflow.
- Nav width animates via inline CSS transition: `.nav-sidebar { transition: width 200ms ease; }`.
- Nav collapse state is persisted in `Alpine.store('shell').collapsed`.
- Active-agent switch triggers lazy engine boot via `window.go.desktop.Shell.EnsureAgentRunning(agentID)`.

### 4.2 Single-agent phased vertical shell

```
┌──────────────────────────────────────────────────┐
│ ①━②━③━④━⑤━⑥━⑦          [sessions] [⚙]         │  wails-drag
├──────────┬──────────┬────────────────────────────┤
│ Sessions │ Artifacts│ Main content              │
│ (toggle) │ tree     │                            │
│  224px   │  224px   │ flex-1                     │
│          │          │                            │
│          │          ├────────────────────────────┤
│          │          │ Bottom bar                 │
└──────────┴──────────┴────────────────────────────┘
```

Skeleton:

```html
<div x-data class="h-screen flex flex-col" x-init="$store.reqflow.init()">
  <!-- Top phase stepper bar (wails-drag) -->
  <div class="wails-drag flex-shrink-0 bg-base-200 border-b border-base-300 px-6 py-3">
    <!-- phase chips + toolbar -->
  </div>

  <!-- Body -->
  <div class="flex-1 flex overflow-hidden">
    <aside x-show="showSessions" class="w-56 flex-shrink-0 bg-base-200/50 border-r border-base-300 p-3 overflow-y-auto flex flex-col">...</aside>
    <aside class="w-56 flex-shrink-0 bg-base-200/50 border-r border-base-300 p-3 overflow-y-auto">...</aside>
    <main class="flex-1 flex flex-col overflow-hidden">
      <div class="flex-1 overflow-y-auto p-6">...</div>
      <!-- Bottom action bar -->
      <div class="flex-shrink-0 border-t border-base-300 bg-base-200/50 px-6 py-3 flex items-center justify-between">...</div>
    </main>
  </div>
</div>
```

### 4.3 Window dragging

Wails exposes `--wails-draggable: drag;` as a CSS custom property. Apply via:

```css
.wails-drag { --wails-draggable: drag; }
```

Put this on the topmost chrome surface only (header bar in horizontal shell, phase stepper bar in vertical shell). Don't apply it to anything interactive — dragging steals click events.

---

## 5. Component catalog

### 5.1 Buttons

| Variant | Class | Use |
|---|---|---|
| Primary action | `btn btn-primary btn-sm` | "Find matches", "Generate", "Send", "Save" |
| Outline secondary | `btn btn-sm btn-outline` | "Add Note", "Next Phase" |
| Ghost | `btn btn-ghost btn-sm` | Tertiary actions in toolbars |
| Icon-only square (sm) | `btn btn-ghost btn-sm btn-square` | Header toolbar icons (sessions, settings, file toggle) |
| Icon-only square (xs) | `btn btn-ghost btn-xs btn-square` | In-row actions (delete, refresh) |
| Icon-only circle (xs) | `btn btn-ghost btn-xs btn-circle` | Show/hide secret toggle |
| Warning | `btn btn-warning btn-sm` | "Restart agent" when settings are dirty |
| Destructive hint | add `text-error` or `hover:text-error` | Delete buttons |
| Inside input group | size matched to the input, e.g. `btn btn-ghost btn-sm` alongside `input-sm` |

**Button size ladder:** `btn-xs` (toolbars, in-row) → `btn-sm` (default) → `btn` (rare; used only for page-level CTAs inside empty-state cards like hello-world's "Say hello").

**Tab/segment group** for mode switches (editor/preview):
```html
<div class="btn-group">
  <button class="btn btn-xs" :class="mode === 'split' ? 'btn-active' : ''">
    <i class="fa-solid fa-columns"></i>
  </button>
  <button class="btn btn-xs" :class="mode === 'edit' ? 'btn-active' : ''">
    <i class="fa-solid fa-code"></i>
  </button>
  <button class="btn btn-xs" :class="mode === 'preview' ? 'btn-active' : ''">
    <i class="fa-solid fa-eye"></i>
  </button>
</div>
```

**Button loading state:** Swap icon for `<span class="loading loading-spinner loading-xs mr-1"></span>` while still showing the label.

### 5.2 Nav / list item

The single most reused pattern in the app. Same shape serves agent nav, session list, file list, artifact rows.

```html
<button
  class="w-full flex items-center gap-3 px-3 py-2.5 rounded-lg transition-colors text-left group"
  :class="isActive
    ? 'bg-primary/15 text-primary'
    : 'hover:bg-base-300 text-base-content/70 hover:text-base-content'">

  <!-- Icon slot with optional status dot -->
  <div class="relative flex-shrink-0 w-8 h-8 rounded-lg flex items-center justify-center"
       :class="isActive ? 'bg-primary/20' : 'bg-base-300 group-hover:bg-base-100'">
    <i class="fa-solid fa-icon text-sm"></i>
    <div x-show="status !== 'idle'"
         class="absolute -bottom-0.5 -right-0.5 w-2.5 h-2.5 rounded-full border-2 border-base-200"
         :class="{
           'bg-success': status === 'running',
           'bg-warning animate-pulse': status === 'booting',
           'bg-error': status === 'error'
         }"></div>
  </div>

  <!-- Labels -->
  <div class="min-w-0 flex-1">
    <div class="text-sm font-medium truncate">Primary label</div>
    <div class="text-xs opacity-50 truncate">Secondary label</div>
  </div>
</button>
```

Density variants:
- **Artifact tree rows** (single-agent): `px-2 py-1 rounded text-sm` — smaller, no icon slot background.
- **Nested list rows** (stories under epics): `pl-6 pr-2 py-0.5 rounded text-xs`.
- **Session list in multi-agent**: uses the `group` pattern so a delete button appears on hover (`opacity-0 group-hover:opacity-50 hover:!opacity-100`).

### 5.3 Cards

Basic container card:

```html
<div class="card bg-base-200 border border-base-300 hover:border-primary/40 transition-colors">
  <div class="card-body p-4">
    <!-- content -->
  </div>
</div>
```

Grid of cards (epics):

```html
<div class="grid grid-cols-1 md:grid-cols-2 gap-4">
  <div class="card bg-base-200 shadow-sm cursor-pointer hover:shadow-md transition-shadow">
    <div class="card-body p-4">
      <div class="flex items-center gap-2 mb-1">
        <span class="badge badge-primary badge-sm font-mono">E-001</span>
        <h3 class="card-title text-sm">Epic title</h3>
      </div>
      <p class="text-xs text-base-content/50 line-clamp-3">Description excerpt...</p>
      <div class="card-actions justify-end mt-2">
        <span class="text-xs text-base-content/30">5 stories</span>
      </div>
    </div>
  </div>
</div>
```

Candidate/ranked-result card:

```html
<div class="card bg-base-200 border border-base-300 hover:border-primary/40 transition-colors">
  <div class="card-body p-4">
    <div class="flex items-start gap-3">
      <div class="shrink-0 w-10 h-10 rounded-full bg-primary/10 text-primary flex items-center justify-center font-semibold text-sm">1</div>
      <div class="flex-1 min-w-0">
        <div class="flex items-center gap-2 mb-1">
          <span class="font-semibold text-base truncate">Jane Doe</span>
          <span class="badge badge-sm badge-success">0.92</span>
        </div>
        <p class="text-sm opacity-80">Reasoning...</p>
        <div class="mt-2 text-xs opacity-40 font-mono">/path/to/resume.pdf</div>
      </div>
    </div>
  </div>
</div>
```

### 5.4 Badges

| Use | Class |
|---|---|
| Count/meta | `badge badge-sm` |
| Status good | `badge badge-success` |
| Status warning | `badge badge-warning` |
| Tiny inline meta | `badge badge-xs` |
| Epic ID | `badge badge-primary font-mono` (or `badge-sm`) |
| Story ID | `badge badge-secondary badge-sm font-mono` |
| Outline reference | `badge badge-outline badge-xs` |
| Restart-required nudge | `badge badge-warning badge-xs` |
| PRD marker | `badge badge-primary badge-xs` |
| Ghost/neutral | `badge badge-ghost` |

Score badges in candidate cards are threshold-driven:
```js
c.Score >= 0.85 ? 'badge-success' : c.Score >= 0.7 ? 'badge-warning' : 'badge-ghost'
```

### 5.5 Forms

**Text input (small default):**
```html
<input type="text" class="input input-bordered input-sm w-full">
```

**Required + empty:** add `input-warning`, with inline hint:
```html
<div x-show="field.required && !hasValue()" class="text-xs text-warning mb-1">
  <i class="fa-solid fa-circle-exclamation"></i> Required
</div>
```

**Number:**
```html
<input type="number" class="input input-bordered input-sm w-full"
       :min="field.validation?.min" :max="field.validation?.max">
```

**Textarea:**
```html
<textarea class="textarea textarea-bordered w-full min-h-[7rem] text-sm leading-snug" rows="5"></textarea>
```
Variants: `textarea-sm` for dense settings; add `font-mono text-sm leading-relaxed resize-none` for markdown/spec editors.

**Select:**
```html
<select class="select select-bordered select-sm w-full">
  <option value="...">Label</option>
</select>
```

**Toggle:**
```html
<input type="checkbox" class="toggle toggle-primary toggle-sm">
```

**Secret (password) with visibility toggle:**
```html
<div class="relative flex-1">
  <input :type="showSecret ? 'text' : 'password'"
         class="input input-bordered input-sm w-full pr-10"
         :placeholder="hasValue() ? '********' : 'Enter value...'"
         @change="saveSecret($event.target.value); $event.target.value = ''">
  <button class="absolute right-2 top-1/2 -translate-y-1/2 btn btn-ghost btn-xs btn-circle"
          @click="showSecret = !showSecret">
    <i class="fa-solid text-xs" :class="showSecret ? 'fa-eye-slash' : 'fa-eye'"></i>
  </button>
</div>
```

Secret inputs never bind `:value` — they show placeholder `********` when a value exists, and only commit on `@change`, clearing the DOM input after save so the raw value never lingers in the DOM.

**Path input + browse:**
```html
<div class="flex gap-2">
  <input type="text" class="input input-bordered input-sm flex-1"
         :value="currentValue()" @change="save($event.target.value)">
  <button class="btn btn-ghost btn-sm" @click="browse()">
    <i class="fa-solid fa-folder-open text-xs"></i> Browse
  </button>
</div>
```

The `browse()` handler calls `window.go.desktop.Shell.PickFolder(...)` or `PickFile(...)`.

**Form label + description:**
```html
<label class="block text-sm font-medium mb-1">Display name</label>
<div class="text-xs opacity-50 mb-2">Optional description.</div>
```

### 5.6 Alerts

| Kind | Class | Use |
|---|---|---|
| Error | `alert alert-error py-2 text-sm` | Runtime errors, validation failures |
| Warning | `alert alert-warning` | Missing required settings banner |
| Success / info / neutral | `alert alert-success` / `alert alert-info` | Not currently used — reserve for future |

Standard error shape:
```html
<div class="alert alert-error py-2 text-sm">
  <i class="fa-solid fa-triangle-exclamation"></i>
  <span>Error message</span>
</div>
```

### 5.7 Collapsible sections (settings)

DaisyUI collapse with arrow, open by default:

```html
<div class="collapse collapse-arrow bg-base-200 border border-base-300">
  <input type="checkbox" checked>
  <div class="collapse-title font-semibold flex items-center gap-2">
    <i class="fa-solid fa-sliders text-sm opacity-60"></i>
    Section title
    <span class="badge badge-warning badge-xs ml-2">restart required</span>
  </div>
  <div class="collapse-content space-y-4">
    <!-- fields -->
  </div>
</div>
```

Highlight a collapse when it has missing required fields: `:class="missingRequired ? 'border-warning' : ''"`.

### 5.8 Empty states

Primary pattern (main content area):
```html
<div class="flex items-center justify-center h-full">
  <div class="text-center max-w-sm">
    <div class="w-16 h-16 rounded-2xl bg-primary/10 flex items-center justify-center mx-auto mb-4">
      <i class="fa-solid fa-magnifying-glass text-primary text-xl"></i>
    </div>
    <h2 class="text-lg font-semibold mb-1">No matches yet</h2>
    <p class="text-sm opacity-60">Upload a PDF and click Find matches.</p>
  </div>
</div>
```

Muted variant (used in phases 1–7 of single-agent shell): drops the tinted icon tile and uses a large low-opacity glyph:
```html
<div class="flex-1 flex items-center justify-center">
  <div class="text-center text-base-content/30 max-w-md">
    <i class="fa-solid fa-file-code text-4xl mb-4 block"></i>
    <p class="font-medium mb-2">No specification yet</p>
    <p class="text-sm mb-4">Generate from your PRD and discussion notes.</p>
    <button class="btn btn-primary btn-sm"><i class="fa-solid fa-wand-magic-sparkles mr-1"></i> Generate Spec</button>
  </div>
</div>
```

Compact (narrow panels, file list empty):
```html
<div class="text-center">
  <i class="fa-solid fa-file-circle-plus text-2xl opacity-20 mb-2"></i>
  <p class="text-xs opacity-40">No files found</p>
  <p class="text-xs opacity-30 mt-1">Drop files here or add to input folder.</p>
</div>
```

### 5.9 Loading states

| Use | Class |
|---|---|
| Inline spinner in button | `loading loading-spinner loading-xs mr-1` |
| Panel-level spinner | `loading loading-spinner loading-sm opacity-40` |
| Hero / full-screen spinner | `loading loading-spinner loading-lg text-primary` |
| Chat "thinking…" | `loading loading-dots loading-sm` |

Boot overlay (covers a section while the engine is starting):
```html
<div x-show="status === 'booting'"
     x-transition
     class="absolute inset-0 flex items-center justify-center bg-base-100/80 z-10">
  <div class="text-center">
    <span class="loading loading-spinner loading-lg text-primary"></span>
    <p class="mt-3 text-sm opacity-60">Starting engine...</p>
  </div>
</div>
```

Generation overlay (long-running content creation):
```html
<div class="flex-1 flex items-center justify-center">
  <div class="text-center">
    <span class="loading loading-spinner loading-lg text-primary"></span>
    <p class="mt-4 text-base-content/60">Generating timeline estimates...</p>
    <p class="text-sm text-base-content/30 mt-1">Analyzing epics and stories</p>
  </div>
</div>
```

### 5.10 Chat / conversation

Layout:

```html
<div class="flex gap-3" :class="msg.role === 'user' ? 'justify-end' : ''">
  <!-- Avatar (non-user only) -->
  <div x-show="msg.role !== 'user'"
       class="w-8 h-8 rounded-full flex items-center justify-center flex-shrink-0"
       :class="msg.role === 'note' ? 'bg-warning/20' : 'bg-primary/20'">
    <i class="text-xs" :class="msg.role === 'note'
      ? 'fa-solid fa-sticky-note text-warning'
      : 'fa-solid fa-robot text-primary'"></i>
  </div>
  <!-- Bubble -->
  <div class="max-w-[75%] rounded-lg px-4 py-3"
       :class="{
         'bg-primary text-primary-content': msg.role === 'user',
         'bg-base-200': msg.role === 'assistant',
         'bg-warning/10 border border-warning/20': msg.role === 'note'
       }">
    <div x-show="msg.role === 'note'" class="text-xs font-semibold text-warning/70 mb-1">Technical Note</div>
    <div class="text-sm whitespace-pre-wrap doc-preview" x-text="msg.content"></div>
  </div>
  <!-- Avatar (user) -->
  <div x-show="msg.role === 'user'"
       class="w-8 h-8 rounded-full bg-primary/20 flex items-center justify-center flex-shrink-0">
    <i class="fa-solid fa-user text-xs text-primary"></i>
  </div>
</div>
```

Auto-scroll to bottom:
```html
<div x-ref="chatScroll"
     x-effect="$refs.chatScroll && ($refs.chatScroll.scrollTop = $refs.chatScroll.scrollHeight)"
     class="flex-1 overflow-y-auto space-y-4 mb-4 pr-2">...</div>
```

Chat input with Cmd/Ctrl+Enter send:
```html
<div class="flex gap-2">
  <textarea class="textarea textarea-bordered flex-1 text-sm" rows="2"
            placeholder="..."
            @keydown.enter.meta="send()" @keydown.enter.ctrl="send()"></textarea>
  <button class="btn btn-primary btn-sm self-end">
    <i class="fa-solid fa-paper-plane"></i>
  </button>
</div>
<div class="text-xs text-base-content/30 mt-1">Cmd+Enter to send</div>
```

### 5.11 Phase stepper

```html
<div class="flex items-center gap-1">
  <template x-for="phase in phases" :key="phase.number">
    <div class="flex items-center">
      <button class="phase-step flex items-center gap-2 px-3 py-1.5 rounded-lg text-sm font-medium transition-colors"
        :class="{
          'active bg-primary text-primary-content': phase.number === current,
          'bg-success/20 text-success': phase.number < current,
          'text-base-content/40 cursor-default': phase.number > current
        }"
        :disabled="phase.number > current"
        @click="phase.number < current && goToPhase(phase.number)">
        <span class="w-5 h-5 rounded-full flex items-center justify-center text-xs font-bold"
              :class="{
                'bg-primary-content/20': phase.number === current,
                'bg-success/30': phase.number < current,
                'bg-base-300': phase.number > current
              }"
              x-text="phase.number < current ? '✓' : phase.number">
        </span>
        <span x-text="phase.label" class="hidden sm:inline"></span>
      </button>
      <div x-show="phase.number < phases.length"
           class="w-4 h-0.5 mx-0.5"
           :class="phase.number < current ? 'bg-success/40' : 'bg-base-300'"></div>
    </div>
  </template>
</div>
```

Supporting CSS:
```css
.phase-step { transition: all 200ms ease; }
.phase-step.active { transform: scale(1.05); }
```

Rules:
- Future phases are disabled, cursor-default, at 40% opacity.
- Completed phases are green (`bg-success/20 text-success`) with a ✓.
- Active phase is primary-filled and slightly scaled up.
- Connectors between steps mirror step state.

### 5.12 Drag-and-drop

**Overlay on a panel while dragging:**
```html
<aside @dragover.prevent="dragOver = true"
       @dragleave.prevent="dragOver = false"
       @drop.prevent="handleDrop($event)"
       class="relative ...">
  <div x-show="dragOver" x-transition
       class="absolute inset-0 z-20 bg-primary/10 border-2 border-dashed border-primary rounded-lg flex items-center justify-center pointer-events-none">
    <div class="text-center">
      <i class="fa-solid fa-cloud-arrow-up text-primary text-2xl mb-2"></i>
      <p class="text-sm font-medium text-primary">Drop files here</p>
    </div>
  </div>
</aside>
```

**Persistent drop zone (centerpiece of a page):**
```html
<div x-data="{ dragOver: false }"
     class="border-2 border-dashed border-base-300 rounded-xl p-8 text-center transition-all"
     :class="{ 'drop-active': dragOver }"
     @dragover.prevent="dragOver = true"
     @dragleave.prevent="dragOver = false"
     @drop.prevent="dragOver = false; handleDrop($event)">
  <i class="fa-solid fa-cloud-arrow-up text-4xl text-base-content/20 mb-4 block"></i>
  <p class="text-base-content/40 mb-3">Drop markdown files here</p>
  <button class="btn btn-sm btn-primary"><i class="fa-solid fa-folder-open mr-1"></i> Browse Files</button>
  <p class="text-xs text-base-content/30 mt-3">Supports .md, .markdown, .txt</p>
</div>
```

Supporting CSS (drop-active emphasis):
```css
.drop-active {
  border-color: oklch(var(--p)) !important;
  background-color: oklch(var(--p) / 0.05);
}
```

### 5.13 Markdown preview

Content rendered via `doc-preview` class gets basic markdown styling:

```css
.doc-preview h1 { font-size: 1.5rem; font-weight: 700; margin: 1rem 0 0.5rem; }
.doc-preview h2 { font-size: 1.25rem; font-weight: 600; margin: 0.75rem 0 0.5rem; }
.doc-preview h3 { font-size: 1.1rem; font-weight: 600; margin: 0.5rem 0 0.25rem; }
.doc-preview p { margin: 0.25rem 0; }
.doc-preview ul, .doc-preview ol { padding-left: 1.5rem; margin: 0.25rem 0; }
.doc-preview li { margin: 0.1rem 0; }
.doc-preview code { background: oklch(var(--b3)); padding: 0.1rem 0.3rem; border-radius: 0.25rem; font-size: 0.875rem; }
.doc-preview pre { background: oklch(var(--b3)); padding: 0.75rem; border-radius: 0.5rem; overflow-x: auto; }
.doc-preview pre code { background: none; padding: 0; }
.doc-preview hr { border-color: oklch(var(--bc) / 0.1); margin: 1rem 0; }
.doc-preview blockquote { border-left: 3px solid oklch(var(--bc) / 0.2); padding-left: 1rem; opacity: 0.8; }
```

Use `oklch(var(--*))` to reference DaisyUI theme variables directly: `--p` (primary), `--b3` (base-300), `--bc` (base-content), etc. This is the only place in the system where CSS custom property access is routine.

### 5.14 Split editor / preview

Three-mode toggle (split/edit/preview) + synchronized textarea and preview pane:

```html
<div class="flex-1 flex gap-4 min-h-0 overflow-hidden">
  <div x-show="viewMode !== 'preview'"
       class="flex-1 flex flex-col min-h-0"
       :class="viewMode === 'split' ? 'max-w-[50%]' : ''">
    <div class="flex items-center justify-between mb-2">
      <span class="text-xs font-semibold text-base-content/50 uppercase tracking-wider">Editor</span>
      <span x-show="dirty" class="text-xs text-warning">
        <i class="fa-solid fa-circle text-[6px] mr-1"></i>Unsaved
      </span>
    </div>
    <textarea class="textarea textarea-bordered flex-1 font-mono text-sm leading-relaxed resize-none"
              x-model="editContent"
              @input="dirty = true"
              @blur="saveEdit()"
              @keydown.meta.s.prevent="saveEdit()"
              @keydown.ctrl.s.prevent="saveEdit()"></textarea>
  </div>
  <div x-show="viewMode !== 'edit'"
       class="flex-1 flex flex-col min-h-0"
       :class="viewMode === 'split' ? 'max-w-[50%]' : ''">
    <div class="flex items-center mb-2">
      <span class="text-xs font-semibold text-base-content/50 uppercase tracking-wider">Preview</span>
    </div>
    <div class="flex-1 overflow-y-auto bg-base-200 rounded-lg p-4 doc-preview">
      <div x-html="renderPreview()"></div>
    </div>
  </div>
</div>
```

### 5.15 Bottom action bar (single-agent only)

```html
<div class="flex-shrink-0 border-t border-base-300 bg-base-200/50 px-6 py-3 flex items-center justify-between">
  <span x-show="status" x-text="status" class="text-sm text-base-content/60"></span>
  <button class="btn btn-sm btn-outline" :disabled="phase >= phases.length" @click="advance()">
    Next Phase <i class="fa-solid fa-chevron-right ml-1"></i>
  </button>
</div>
```

### 5.16 Copy-to-clipboard button

```html
<button class="btn btn-ghost btn-xs" @click="copyToClipboard()">
  <i class="fa-solid fa-copy mr-1"></i>
  <span x-text="copied ? 'Copied!' : 'Copy'"></span>
</button>
```

Toggle the `copied` flag to true for ~1.5s after success, then reset.

### 5.17 Tabs (in-content)

The comms phase uses a button-row "tabs" pattern rather than DaisyUI's tabs component:

```html
<div class="flex gap-1 mb-3 flex-shrink-0">
  <template x-for="item in items" :key="item.template">
    <button class="btn btn-xs"
            :class="selected === item.template ? 'btn-primary' : 'btn-ghost'"
            @click="selected = item.template"
            x-text="item.title"></button>
  </template>
</div>
```

---

## 6. Font Awesome icon inventory

Every icon that appears in the shipped apps, grouped by role. Use these first before reaching for alternatives.

**App/brand:** `fa-bolt` (Nexus logo)

**Nav/chrome:** `fa-gear`, `fa-angles-left`, `fa-angles-right`, `fa-xmark`, `fa-plus`, `fa-chevron-right`, `fa-arrow-left`, `fa-rotate-right`, `fa-arrows-rotate`

**Files:** `fa-folder-open`, `fa-folder`, `fa-folder-plus`, `fa-file`, `fa-file-lines`, `fa-file-code`, `fa-file-circle-plus`, `fa-paperclip`, `fa-cloud-arrow-up`, `fa-arrow-up-right-from-square` (open external)

**Chat / communication:** `fa-comments`, `fa-paper-plane`, `fa-robot`, `fa-user`, `fa-sticky-note`, `fa-bullhorn`

**Generation / action:** `fa-wand-magic-sparkles`, `fa-magnifying-glass`, `fa-copy`

**Workflow:** `fa-layer-group` (epics), `fa-list-check` (stories), `fa-calendar-days` (timeline)

**Settings/fields:** `fa-sliders`, `fa-eye`, `fa-eye-slash`, `fa-trash-can`, `fa-circle-exclamation`, `fa-triangle-exclamation`, `fa-circle` (tiny marker dot)

**Edit/view mode toggle:** `fa-code`, `fa-columns`, `fa-eye`

**Session / history:** `fa-clock-rotate-left`

Always `fa-solid` (solid style). All icons are the free tier — don't introduce Pro-only glyphs. Sizing is driven by the wrapping element, not `fa-*` size classes; use `text-xs`, `text-sm`, `text-lg`, `text-2xl`, `text-4xl` on the `<i>`.

---

## 7. State management patterns

Alpine stores are the state spine. No Redux, no Pinia, no context API.

### 7.1 Store declaration

```js
document.addEventListener('alpine:init', () => {
  Alpine.store('storename', {
    // reactive state
    items: [],
    loading: false,

    // methods
    async init() { ... },
    async load() { ... },
  });
});
```

### 7.2 Scoped bus bridge

Every agent's UI talks to its engine via `createBus(agentID)` — a thin wrapper over Wails events with scoping:

```js
const bus = createBus('agent-id');
bus.emit('domain.request', { data });
bus.on('domain.result', (payload) => { /* handle */ });
bus.off('domain.result', handler);
await bus.call('req.request', 'req.response', payload, timeoutMs);
```

Outbound channel: `{agentID}:nexus` (engine → UI).
Inbound channel: `{agentID}:nexus.input` (UI → engine).
Envelope: `{ type: "event.name", payload: {...}, timestamp: "ISO" }`.

### 7.3 Wails-bound shell services

The shell (`pkg/desktop/shell.go`) exposes Wails methods directly on `window.go.desktop.Shell.*` — these are the only places UI calls non-bus APIs:

| Method | Purpose |
|---|---|
| `ListAgents()` | Enumerate agents with current status |
| `EnsureAgentRunning(agentID)` | Lazy boot |
| `StopAgent(agentID)` | Stop engine |
| `NewSession(agentID)` / `RecallSession(agentID, sessionID)` / `ListSessions(agentID)` / `DeleteSession(agentID, sessionID)` | Session ops |
| `PickFile(agentID, title, filter)` / `PickFolder(agentID, title)` | Native dialogs |
| `OpenExternal(url)` / `RevealInFinder(path)` / `Notify(title, body)` | OS integration |
| `ListFiles(agentID, filter)` / `OutputDir(agentID)` / `CopyFileToInputDir(agentID, sourcePath)` / `WatchInputDir(agentID)` | File portal |
| `GetSettingsSchema()` / `GetSettings()` / `UpdateSetting(scope, key, value)` / `UpdateSecret(scope, key, value)` / `DeleteSetting(scope, key, secret)` / `HasMissingRequired()` | Settings |

Rule: shell services are only called from the UI layer; agents communicate with the shell via bus events, not by invoking these methods.

### 7.4 Settings schema → UI rendering

The shell returns a settings schema with fields like:

```json
{
  "shell": [{ "key": "shared_data_dir", "display": "Shared data folder", "type": "path" }],
  "agents": {
    "my-agent": [
      { "key": "input_dir", "display": "Input folder", "type": "path", "required": true },
      { "key": "api_key", "display": "API key", "type": "string", "secret": true, "required": true }
    ]
  }
}
```

Types and their rendering:

| `type` | Recipe |
|---|---|
| `string` | `input input-bordered input-sm` (or secret variant if `secret: true`) |
| `path` | input + browse button calling `PickFolder`/`PickFile` |
| `number` | `input type=number` with optional `min`/`max` from `validation` |
| `bool` | `toggle toggle-primary toggle-sm` |
| `select` | `select select-bordered select-sm` with `field.options` |
| `text` | `textarea textarea-bordered textarea-sm min-h-[6rem]` |

Secrets never render a current value — just `********` placeholder when present.

### 7.5 Session list

Sessions are tracked per agent. The shell emits `{agentID}:sessions.updated` via Wails events (not through the bus bridge) when the list changes. Session metadata includes:

- `id`, `title` (from `session.meta.title` event), `created_at`, `status`, `preview` (arbitrary JSON from `session.meta.preview` event)

Session-list rendering uses the standard nav/list-item pattern (§5.2) with a status dot, title, timestamp, and optional preview line.

### 7.6 UI state persistence

Two bus events drive cross-session UI state:

- `ui.state.save` — UI emits with opaque `{ state: {...} }` payload. Shell writes to `ui-state.json` in the engine session dir.
- `ui.state.restore` — Shell emits on session recall. UI rehydrates.

Designs that have non-trivial UI state (partially filled forms, scroll positions, expanded panels, active tab) should persist + rehydrate via these events. Keep the payload JSON-serializable and opaque to the shell.

---

## 8. Interaction and accessibility patterns

### 8.1 Keyboard

- `Cmd+Enter` / `Ctrl+Enter` — submit in chat textareas (`@keydown.enter.meta`, `@keydown.enter.ctrl`)
- `Cmd+S` / `Ctrl+S` — save in spec/timeline editors (`@keydown.meta.s.prevent`, `@keydown.ctrl.s.prevent`)
- `Enter` — submit in single-line inputs (hello world)
- No global shortcuts currently.

Show hints inline as `text-xs opacity-30` or `text-xs text-base-content/30 mt-1`.

### 8.2 Hover reveal

Destructive or secondary in-row actions hide by default, appear on hover of the parent `.group`:

```html
<div class="group ...">
  <span>Content</span>
  <button class="btn btn-ghost btn-xs btn-square opacity-0 group-hover:opacity-50 hover:!opacity-100 hover:text-error">
    <i class="fa-solid fa-xmark text-xs"></i>
  </button>
</div>
```

### 8.3 Transitions

Side panel enter/exit (left panel):
```html
x-transition:enter="transition ease-out duration-150"
x-transition:enter-start="opacity-0 -translate-x-2"
x-transition:enter-end="opacity-100 translate-x-0"
```

For right-side panels, reverse the translate (`translate-x-2`). For vertical reveals (add-note input), use default `x-transition` for a simple fade/slide.

### 8.4 Titles and tooltips

Native `title` attribute is the standard tooltip. No tooltip library is used.

```html
<button title="Refresh file list">...</button>
```

### 8.5 Required-field gating

Shell's `HasMissingRequired()` returns a map of `agentID → [missingKeys]`. Pattern:

1. Top-of-settings banner: `alert alert-warning` summarizing "Required settings missing".
2. On the offending collapse: `:class="missing ? 'border-warning' : ''"`.
3. On the field: `input-warning` class + inline "Required" hint.
4. Agents with missing required settings cannot boot — nav-click routes to settings instead.

### 8.6 Dirty / unsaved

- Textarea-based editors track a `dirty` flag set on `@input`, cleared on `@blur` / save.
- Display inline `text-xs text-warning` with a `<i class="fa-solid fa-circle text-[6px] mr-1"></i>` dot.
- Show a Save button (`btn btn-xs btn-primary`) only when dirty.

### 8.7 Restart required

When a running agent has changed settings:
- Badge in section title: `badge badge-warning badge-xs ml-2` "restart required"
- Restart button at bottom of section: `btn btn-warning btn-sm` with `fa-rotate-right` icon

---

## 9. Multi-agent vs single-agent — differences at a glance

| Concern | Multi-agent (`cmd/desktop`) | Single-agent (`cmd/techlead`) |
|---|---|---|
| Primary navigation | Left nav, lazy engine boot | Top phase stepper bar |
| Session list | Dedicated left panel, always visible when agent active | Toggleable panel, opt-in |
| File browser | Right toggleable panel (shell feature) | Integrated into phase 1 import UI |
| Settings | Dedicated view in main, triggered from nav | Modal-ish overlay on main, triggered from toolbar |
| Agent boot | Multiple engines, one per agent, lazy | Single engine, boots on init |
| Next action | User-driven per agent | "Next Phase" button in bottom action bar |
| Progression | Not ordered — user picks agent | Ordered — phase N unlocks when N-1 complete |
| Artifacts | Opaque — agents render their own sections | Shared artifact tree in left sidebar |

When extending **multi-agent**: check whether the feature is in-session (both kinds of shell) or wrapper (desktop shell only). See the CLAUDE.md parity rule.

When extending **single-agent**: new phases should follow the existing N-ary pattern — empty state → generating state → content → edit mode → unsaved indicator.

---

## 10. Opinionated "what NOT to do" list

- **No new frameworks.** Alpine + Tailwind + DaisyUI is the full toolbox. No React, Vue, Svelte, jQuery, Lodash, etc.
- **No light theme** unless explicitly requested — dark theme is the reference experience.
- **No raw gray scales.** Use `opacity-*` on `text-base-content` / `border-base-300` etc.
- **No modals or dialogs** in the absence of a strong reason. Prefer inline reveals, collapsibles, side panels, or dedicated views.
- **No routing.** Views switch via `x-show` driven by a store property.
- **No animations beyond utility.** DaisyUI `animate-pulse` + 150ms panel slides are the entire vocabulary.
- **No custom icon sets.** Font Awesome 6 solid free only.
- **No responsive / mobile layouts.** Desktop-only, min ~900×720.
- **No font imports.** Stick to the system/Tailwind default font stack.
- **No toasts/snackbars.** Errors go in `alert` or inline. Success is silent or a tiny inline "Copied!".
- **No component abstractions** until a pattern is used three times. Inline the classes.

---

## 11. Extensibility hooks

When Claude Design proposes something new, validate against these hooks so it slots into the real system:

- **Is it a bus event or a Wails-bound shell method?** If it changes agent state, design around an event round-trip (request → response). If it asks the OS or shell, call a `window.go.desktop.Shell.*` method.
- **Does it need session-recall persistence?** If yes, design the UI state payload and plan on `ui.state.save`/`ui.state.restore`.
- **Does it touch files?** Use the shell's file portal (`input_dir`, `output_dir`, `ListFiles`, `PickFile`). Don't design a bespoke file picker.
- **Does it share with another agent?** If yes, it probably belongs in `pkg/ui/` shared code, not a single app.
- **Is it a multi-session concern?** If yes, it only belongs in multi-agent / wrapper scopes — don't back-port to single-agent shells that are session-scoped.

---

## 12. Reference quick-lookup

| Situation | Reach for |
|---|---|
| Add a list panel | §5.2 nav/list item + §3.2 spacing (256px for sessions, 224px for trees) |
| Add a new form field | §5.5 forms + §7.4 settings schema |
| Show async progress | §5.9 loading states |
| Signal something's happening in background | `animate-pulse` on a status dot, `loading-dots` in chat |
| A destination that doesn't exist yet | §5.8 empty states |
| Destructive action | §5.1 buttons + §8.2 hover reveal |
| Confirm a change | Don't add a modal; use inline "Unsaved" + blur-to-save |
| Show a generated document | §5.13 markdown preview + §5.14 split editor |
| Show ranked results | §5.3 candidate card |
| Offer drag-and-drop | §5.12 drag-and-drop |
| Switch views/modes | §5.1 btn-group (three-way), or single primary/ghost toggle |

---

## 13. Glossary

| Term | Meaning in this system |
|---|---|
| **Agent** | A domain-specific AI workflow embedded in the desktop app. Has its own engine, UI section, and (optionally) settings schema. |
| **Shell** | The desktop wrapper (`pkg/desktop/shell.go`) that manages agents, sessions, settings, file portal. |
| **Engine** | The Nexus core that runs an agent's plugins. Each agent has its own. |
| **Session** | One run of an agent. Can be recalled, deleted, listed per agent. |
| **Bus** | The event bus inside an engine. `createBus(agentID)` wraps Wails events into a scoped JS adapter. |
| **Plugin** | Composable behavior inside an engine. UI never calls plugins directly — only via bus events. |
| **Input dir / output dir** | Per-agent folders for file I/O, declared as settings and managed by the shell's file portal. |
| **Phase** | One step in a single-agent workflow app. Sequential, gated by prior completion. |
| **Artifact** | A piece of output from a phase — document, epic, story, timeline, comms entry. |
| **Scoped event channel** | `{agentID}:nexus` and `{agentID}:nexus.input` — Wails events namespaced by agent for multi-agent isolation. |
| **`wails-drag`** | CSS class applied to window chrome to make it OS-draggable. |
| **`doc-preview`** | CSS class applied to rendered markdown containers. |
