package events

// SkillCatalog lists all available skills.
type SkillCatalog struct {
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
	Name        string
	RequestedBy string // plugin ID or "user"
}

// SkillContent carries the full content of an activated skill.
type SkillContent struct {
	Name      string
	Body      string
	Resources []string
	Scope     string
	BaseDir   string
}

// SkillRef is a lightweight reference to a skill by name.
type SkillRef struct {
	Name string
}

// SkillResourceReq requests a specific resource from a skill.
type SkillResourceReq struct {
	SkillName string
	Path      string
}

// SkillResourceData carries the content of a skill resource.
type SkillResourceData struct {
	SkillName string
	Path      string
	Content   []byte
	MimeType  string
}
