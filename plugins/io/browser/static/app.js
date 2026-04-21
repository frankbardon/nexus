// Nexus Browser UI — AlpineJS Application

document.addEventListener('alpine:init', () => {

  // ── Global Store ──────────────────────────────────────────────────────
  Alpine.store('app', {
    // Connection
    ws: null,
    connected: false,
    sessionId: null,
    reconnectDelay: 1000,
    reconnectTimer: null,

    // Plugins
    plugins: [],
    features: { planner: false, thinking: false, skills: false, memory: false, cancel: false },

    // Cancel/resume
    cancelResumable: false,

    // Chat
    messages: [],
    streamTurnId: null,

    // Status
    status: { state: 'idle', detail: '' },

    // Approval
    pendingApproval: null,

    // Ask user
    pendingAsk: null,

    // Plan (right rail)
    currentPlan: null,

    // Files
    files: [],
    viewingFile: null,  // { name, path, content, loading }
    activeView: 'chat', // 'chat' | 'file'

    // Theme
    currentTheme: localStorage.getItem('nexus-theme') || 'dark',

    // Left nav section
    navSection: 'chat', // 'chat' | 'files'

    // ── Connection ────────────────────────────────────────────────────

    connect() {
      if (this.ws && this.ws.readyState <= 1) return;

      const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      const url = `${proto}//${location.host}/ws`;

      try {
        this.ws = new WebSocket(url);
      } catch (e) {
        this.scheduleReconnect();
        return;
      }

      this.ws.onopen = () => {
        this.connected = true;
        this.reconnectDelay = 1000;
        this.fetchPlugins();
        this.fetchFiles();
      };

      this.ws.onmessage = (event) => {
        try {
          const envelope = JSON.parse(event.data);
          this.handleMessage(envelope);
        } catch (e) {
          console.error('Failed to parse message:', e);
        }
      };

      this.ws.onclose = () => {
        this.connected = false;
        this.scheduleReconnect();
      };

      this.ws.onerror = () => {
        this.connected = false;
      };
    },

    scheduleReconnect() {
      if (this.reconnectTimer) return;
      this.reconnectTimer = setTimeout(() => {
        this.reconnectTimer = null;
        this.reconnectDelay = Math.min(this.reconnectDelay * 2, 30000);
        this.connect();
      }, this.reconnectDelay);
    },

    send(type, payload) {
      if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
      const envelope = {
        type: type,
        id: 'ui-' + Date.now() + '-' + Math.random().toString(36).slice(2, 8),
        session_id: this.sessionId || '',
        timestamp: new Date().toISOString(),
        payload: payload,
      };
      this.ws.send(JSON.stringify(envelope));
    },

    // ── Message Handling ──────────────────────────────────────────────

    handleMessage(env) {
      const payload = env.payload;
      if (env.session_id) this.sessionId = env.session_id;

      switch (env.type) {
        case 'output':
          this.messages.push({
            id: env.id,
            role: payload.role || 'assistant',
            content: payload.content,
            turnId: payload.turn_id,
            type: 'output',
            timestamp: env.timestamp,
          });
          break;

        case 'stream_chunk':
          this._handleStreamChunk(payload, env);
          break;

        case 'stream_end':
          this.streamTurnId = null;
          break;

        case 'status':
          this.status = { state: payload.state, detail: payload.detail || '' };
          break;

        case 'approval_request':
          this.pendingApproval = {
            promptId: payload.prompt_id,
            description: payload.description,
            toolCall: payload.tool_call,
            risk: payload.risk,
          };
          break;

        case 'ask_request':
          this.pendingAsk = {
            promptId: payload.prompt_id,
            question: payload.question,
            turnId: payload.turn_id,
          };
          break;

        case 'thinking':
          this.messages.push({
            id: env.id,
            role: 'thinking',
            content: payload.content,
            phase: payload.phase || 'thinking',
            turnId: payload.turn_id,
          });
          break;

        case 'code_exec_stdout':
          this._handleCodeExecStdout(payload, env);
          break;

        case 'plan':
          if (payload.plan_id) {
            // Plan created by the planner — display it.
            this.currentPlan = {
              planId: payload.plan_id,
              summary: payload.summary,
              steps: (payload.steps || []).map(s => ({
                id: s.id,
                description: s.description,
                status: s.status,
                order: s.order,
              })),
              source: payload.source,
              turnId: payload.turn_id,
            };
          } else if (this.currentPlan) {
            // Step status update for an existing plan.
            // Only apply updates that match the plan's step count to avoid
            // tool-call notifications (e.g. "Run tool: ask_user") overwriting
            // the actual plan steps.
            const steps = payload.steps || [];
            if (steps.length === this.currentPlan.steps.length) {
              for (let i = 0; i < steps.length; i++) {
                this.currentPlan.steps[i].status = steps[i].status;
              }
            }
          }
          break;

        case 'file_changed':
          this.fetchFiles();
          break;

        case 'session_reset':
          this.messages = [];
          this.currentPlan = null;
          this.status = { state: 'idle', detail: '' };
          break;

        case 'cancel_complete':
          this.cancelResumable = payload.resumable || false;
          break;

        case 'pong':
          break;
      }
    },

    _handleCodeExecStdout(payload, env) {
      const msgId = 'code-' + payload.call_id;
      let msg = this.messages.find(m => m.id === msgId);
      if (!msg) {
        msg = {
          id: msgId,
          role: 'code_stdout',
          content: '',
          callId: payload.call_id,
          turnId: payload.turn_id,
          streaming: !payload.final,
          truncated: false,
          collapsed: false,
          timestamp: env.timestamp,
        };
        this.messages.push(msg);
      }
      if (payload.chunk) {
        msg.content += payload.chunk;
      }
      if (payload.final) {
        msg.streaming = false;
        msg.truncated = !!payload.truncated;
      }
    },

    _handleStreamChunk(payload, env) {
      if (this.streamTurnId !== payload.turn_id) {
        this.streamTurnId = payload.turn_id;
        this.messages.push({
          id: 'stream-' + payload.turn_id,
          role: 'assistant',
          content: '',
          turnId: payload.turn_id,
          type: 'stream',
          timestamp: env.timestamp,
        });
      }
      const msg = this.messages.find(
        m => m.turnId === payload.turn_id && m.type === 'stream'
      );
      if (msg) {
        msg.content += payload.content;
      }
    },

    // ── User Actions ──────────────────────────────────────────────────

    sendInput(content) {
      if (!content.trim()) return;
      this.cancelResumable = false;
      this.messages.push({
        id: 'user-' + Date.now(),
        role: 'user',
        content: content.trim(),
        type: 'input',
        timestamp: new Date().toISOString(),
      });
      this.send('input', { content: content.trim(), files: [] });
    },

    requestCancel() {
      this.send('cancel_request', {});
    },

    requestResume() {
      this.cancelResumable = false;
      this.send('resume_request', {});
    },

    respondApproval(approved, always) {
      if (!this.pendingApproval) return;
      this.send('approval_response', {
        prompt_id: this.pendingApproval.promptId,
        approved: approved,
        always: always || false,
      });
      this.pendingApproval = null;
    },

    respondAsk(answer) {
      if (!this.pendingAsk) return;
      this.send('ask_response', {
        prompt_id: this.pendingAsk.promptId,
        answer: answer,
      });
      this.pendingAsk = null;
    },

    // ── Data Fetching ─────────────────────────────────────────────────

    async fetchPlugins() {
      try {
        const resp = await fetch('/api/plugins');
        const data = await resp.json();
        this.plugins = data.active || [];
        this.features = data.features || {};
      } catch (e) {
        console.error('Failed to fetch plugins:', e);
      }
    },

    async fetchFiles() {
      try {
        const resp = await fetch('/api/files');
        this.files = await resp.json();
      } catch (e) {
        console.error('Failed to fetch files:', e);
      }
    },

    async viewFile(file) {
      this.viewingFile = { name: file.name, path: file.path, content: '', loading: true };
      this.activeView = 'file';
      try {
        const resp = await fetch('/api/files/' + encodeURIComponent(file.path));
        this.viewingFile.content = await resp.text();
      } catch (e) {
        this.viewingFile.content = 'Failed to load file.';
      }
      this.viewingFile.loading = false;
    },

    closeFileView() {
      this.viewingFile = null;
      this.activeView = 'chat';
    },

    // ── Theme ─────────────────────────────────────────────────────────

    setTheme(theme) {
      this.currentTheme = theme;
      document.documentElement.setAttribute('data-theme', theme);
      localStorage.setItem('nexus-theme', theme);
    },

    // ── Helpers ────────────────────────────────────────────────────────

    get isRightRailVisible() {
      return !!this.currentPlan;
    },

    get isWorking() {
      return this.status.state !== 'idle';
    },

    formatFileSize(bytes) {
      if (bytes < 1024) return bytes + ' B';
      if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
      return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
    },
  });

  // ── Markdown Renderer ───────────────────────────────────────────────
  // Minimal renderer — handles code blocks, headings, bold, italic,
  // inline code, links, lists, and paragraphs. No external deps.

  window.renderMarkdown = function(text) {
    if (!text) return '';

    // Escape HTML
    const esc = (s) => s
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');

    const lines = text.split('\n');
    const out = [];
    let inCode = false;
    let codeLang = '';
    let codeLines = [];
    let inList = false;
    let listType = '';

    const closeList = () => {
      if (inList) {
        out.push(listType === 'ol' ? '</ol>' : '</ul>');
        inList = false;
      }
    };

    const inlineFormat = (line) => {
      // Bold
      line = line.replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>');
      // Italic
      line = line.replace(/\*(.+?)\*/g, '<em>$1</em>');
      // Inline code
      line = line.replace(/`([^`]+)`/g, '<code>$1</code>');
      // Links
      line = line.replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
      return line;
    };

    for (let i = 0; i < lines.length; i++) {
      const raw = lines[i];

      // Code block toggle
      if (raw.trimStart().startsWith('```')) {
        if (!inCode) {
          closeList();
          codeLang = raw.trimStart().slice(3).trim();
          codeLines = [];
          inCode = true;
        } else {
          out.push('<pre><code' + (codeLang ? ' class="language-' + esc(codeLang) + '"' : '') + '>' + esc(codeLines.join('\n')) + '</code></pre>');
          inCode = false;
        }
        continue;
      }

      if (inCode) {
        codeLines.push(raw);
        continue;
      }

      const trimmed = raw.trim();

      // Empty line
      if (!trimmed) {
        closeList();
        continue;
      }

      // Headings
      const headingMatch = trimmed.match(/^(#{1,6})\s+(.+)/);
      if (headingMatch) {
        closeList();
        const level = headingMatch[1].length;
        out.push('<h' + level + '>' + inlineFormat(esc(headingMatch[2])) + '</h' + level + '>');
        continue;
      }

      // Unordered list
      if (/^[-*+]\s/.test(trimmed)) {
        if (!inList || listType !== 'ul') {
          closeList();
          out.push('<ul>');
          inList = true;
          listType = 'ul';
        }
        out.push('<li>' + inlineFormat(esc(trimmed.replace(/^[-*+]\s/, ''))) + '</li>');
        continue;
      }

      // Ordered list
      if (/^\d+\.\s/.test(trimmed)) {
        if (!inList || listType !== 'ol') {
          closeList();
          out.push('<ol>');
          inList = true;
          listType = 'ol';
        }
        out.push('<li>' + inlineFormat(esc(trimmed.replace(/^\d+\.\s/, ''))) + '</li>');
        continue;
      }

      // Paragraph
      closeList();
      out.push('<p>' + inlineFormat(esc(trimmed)) + '</p>');
    }

    // Close unclosed code block
    if (inCode) {
      out.push('<pre><code>' + esc(codeLines.join('\n')) + '</code></pre>');
    }
    closeList();

    return out.join('\n');
  };

  // ── Alpine Directive: x-markdown ────────────────────────────────────
  Alpine.directive('markdown', (el, { expression }, { evaluate, effect }) => {
    effect(() => {
      const value = evaluate(expression);
      el.innerHTML = renderMarkdown(value);
    });
  });

  // ── Initialize ──────────────────────────────────────────────────────
  const store = Alpine.store('app');
  store.setTheme(store.currentTheme);
  store.connect();
});
