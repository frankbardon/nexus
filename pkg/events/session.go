package events

// Schema-version constants for session.* payloads. See doc.go.
const (
	SessionFileVersion = 1
)

// SessionFile describes a file event within a session workspace.
type SessionFile struct {
	SchemaVersion int `json:"_schema_version"`

	Path   string // relative to session root
	Action string // "created", "updated"
	Size   int64
}
