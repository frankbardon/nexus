package events

// Schema-version constants for skill.* payloads. See doc.go.
const (
	SkillCatalogVersion      = 1
	SkillActivationVersion   = 1
	SkillContentVersion      = 1
	SkillRefVersion          = 1
	SkillResourceReqVersion  = 1
	SkillResourceDataVersion = 1
)

// SkillCatalog lists all available skills.
type SkillCatalog struct {
	SchemaVersion int `json:"_schema_version"`

	Skills []SkillSummary
}

// SkillSummary provides a brief description of a skill.
type SkillSummary struct {
	Name        string
	Description string
	Location    string
	Scope       string // "project", "user", "builtin", "config"
	Class       string // Semantic class for progressive discovery.
	Subclass    string // Optional grouping within class.
}

// SkillActivation requests activation of a skill.
type SkillActivation struct {
	SchemaVersion int `json:"_schema_version"`

	Name        string
	RequestedBy string // plugin ID or "user"
}

// SkillContent carries the full content of an activated skill.
type SkillContent struct {
	SchemaVersion int `json:"_schema_version"`

	Name      string
	Body      string
	Resources []string
	Scope     string
	BaseDir   string
	// Runtime selects the codeexec runtime for this skill's helpers.
	// Empty defaults to "yaegi" at the consumer for backwards compatibility;
	// "wasm" routes the skill's run_code through ctx.Sandbox when codeexec
	// is configured with compiler: yaegi-wasm.
	Runtime string
}

// SkillRef is a lightweight reference to a skill by name.
type SkillRef struct {
	SchemaVersion int `json:"_schema_version"`

	Name string
}

// SkillResourceReq requests a specific resource from a skill.
type SkillResourceReq struct {
	SchemaVersion int `json:"_schema_version"`

	SkillName string
	Path      string
}

// SkillResourceData carries the content of a skill resource.
type SkillResourceData struct {
	SchemaVersion int `json:"_schema_version"`

	SkillName string
	Path      string
	Content   []byte
	MimeType  string
}
