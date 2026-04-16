# Ask User Tool

Allows the agent to ask the user a free-form question and wait for their response. Useful when the agent needs clarification or confirmation.

## Details

| | |
|---|---|
| **ID** | `nexus.tool.ask` |
| **Tool Name** | `ask_user` |
| **Dependencies** | None |

## Configuration

No configuration options.

## Tool Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `question` | string | Yes | The question to ask the user |

## Events

### Subscribes To

| Event | Priority | Purpose |
|-------|----------|---------|
| `tool.invoke` | 50 | Handles ask requests |
| `io.ask.response` | 50 | Receives the user's answer |

### Emits

| Event | When |
|-------|------|
| `tool.result` | User's response |
| `tool.register` | Registers the `ask_user` tool at boot |
| `io.ask` | Sends the question to the I/O layer |

## How It Works

1. Agent invokes `ask_user` with a question
2. Plugin emits `io.ask` with a prompt ID and the question
3. The I/O plugin (TUI or Browser) displays the question and collects input
4. I/O plugin emits `io.ask.response` with the user's answer
5. Plugin emits `tool.result` with the answer, unblocking the agent

The plugin blocks on a channel until the response arrives, ensuring the agent waits for the user.

## Example Configuration

```yaml
# No config needed — just add to active list
plugins:
  active:
    - nexus.tool.ask
```
