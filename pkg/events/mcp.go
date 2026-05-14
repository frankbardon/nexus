package events

// Schema-version constants for mcp.* payloads. See doc.go.
const (
	MCPResourceUpdatedVersion = 1
	MCPPromptsListVersion     = 1
)

// MCPResourceUpdated is emitted by nexus.mcp.client when a subscribed MCP
// resource changes upstream. No consumer is wired in Phase 1; the event is
// plumbed so future RAG ingest, memory, or cache-invalidation plugins can
// react without further core changes.
type MCPResourceUpdated struct {
	SchemaVersion int `json:"_schema_version"`

	Server string
	URI    string
	Title  string
}

// MCPPromptsList is a synchronous query event for the currently registered
// MCP prompt slash commands. Same fill-in-place pattern as HistoryQuery and
// ToolCatalogQuery: emit as a pointer payload, handler populates Prompts
// before Emit returns. IO plugins use this to render a /help entry without
// holding a reference to the mcp.client plugin directly.
type MCPPromptsList struct {
	SchemaVersion int `json:"_schema_version"`

	Prompts []MCPPromptDescriptor
}

// MCPPromptDescriptor is the catalog entry returned by MCPPromptsList.
type MCPPromptDescriptor struct {
	Command     string // includes leading slash, e.g. "/mcp.gh.review_pr"
	Server      string
	Prompt      string
	Title       string
	Description string
	Arguments   []MCPPromptArgument
}

// MCPPromptArgument mirrors mcp.PromptArgument without depending on the SDK
// from the events package.
type MCPPromptArgument struct {
	Name        string
	Description string
	Required    bool
}
