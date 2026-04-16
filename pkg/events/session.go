package events

// SessionFile describes a file event within a session workspace.
type SessionFile struct {
	Path   string // relative to session root
	Action string // "created", "updated"
	Size   int64
}
