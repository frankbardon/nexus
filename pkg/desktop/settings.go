package desktop

// FieldType describes the kind of UI control a settings field renders as.
type FieldType int

const (
	FieldString FieldType = iota // single-line text input
	FieldPath                    // text input + Browse button (calls Shell.PickFolder)
	FieldText                    // multiline textarea (user-editable prompts)
	FieldNumber                  // number input with optional min/max
	FieldBool                    // toggle switch
	FieldSelect                  // dropdown
)

// fieldTypeNames maps FieldType to a JSON-friendly string for the frontend.
var fieldTypeNames = map[FieldType]string{
	FieldString: "string",
	FieldPath:   "path",
	FieldText:   "text",
	FieldNumber: "number",
	FieldBool:   "bool",
	FieldSelect: "select",
}

// SettingsField declares a single configurable value that an agent (or
// the shell itself) exposes to the user through the settings UI.
type SettingsField struct {
	Key         string           // machine key: "data_dir", "greeting"
	Display     string           // human label: "Data Folder"
	Description string           // help text shown below the field
	Type        FieldType        // determines the UI control
	Secret      bool             // stored in OS keychain, masked in UI
	Default     any              // default value if the user hasn't configured one
	Required    bool             // agent refuses to boot without this
	Validation  *FieldValidation // optional constraints
	ConfigPath  string           // template variable in config YAML, e.g. "${data_dir}"
	Options     []SelectOption   // for FieldSelect only
}

// FieldValidation holds optional constraints for a settings field.
type FieldValidation struct {
	Regex   string   `json:"regex,omitempty"`
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	Message string   `json:"message,omitempty"` // validation error message
}

// SelectOption is one choice in a FieldSelect dropdown.
type SelectOption struct {
	Value   string `json:"value"`
	Display string `json:"display"`
}

// SettingsFieldInfo is the JSON-serializable projection of SettingsField
// sent to the frontend for dynamic UI rendering.
type SettingsFieldInfo struct {
	Key         string           `json:"key"`
	Display     string           `json:"display"`
	Description string           `json:"description,omitempty"`
	Type        string           `json:"type"`
	Secret      bool             `json:"secret,omitempty"`
	Required    bool             `json:"required,omitempty"`
	Default     any              `json:"default,omitempty"`
	Validation  *FieldValidation `json:"validation,omitempty"`
	Options     []SelectOption   `json:"options,omitempty"`
}

// SettingsSchema is the top-level schema sent to the frontend. It
// contains shell-level fields and per-agent fields.
type SettingsSchema struct {
	Shell  []SettingsFieldInfo            `json:"shell"`
	Agents map[string][]SettingsFieldInfo `json:"agents"`
}

// toInfo converts an internal SettingsField to its frontend projection.
func (f *SettingsField) toInfo() SettingsFieldInfo {
	typeName := fieldTypeNames[f.Type]
	if typeName == "" {
		typeName = "string"
	}
	return SettingsFieldInfo{
		Key:         f.Key,
		Display:     f.Display,
		Description: f.Description,
		Type:        typeName,
		Secret:      f.Secret,
		Required:    f.Required,
		Default:     f.Default,
		Validation:  f.Validation,
		Options:     f.Options,
	}
}

// shellSettings are the built-in shell-level settings fields.
var shellSettings = []SettingsField{
	{
		Key:         "session_root",
		Display:     "Session Storage",
		Description: "Directory where session data is stored",
		Type:        FieldPath,
		Default:     "~/.nexus/sessions",
	},
	{
		Key:         "session_retention_days",
		Display:     "Session Retention",
		Description: "Number of days to keep session data before cleanup",
		Type:        FieldNumber,
		Default:     30,
		Validation:  &FieldValidation{Min: ptrFloat(1), Max: ptrFloat(365), Message: "Must be between 1 and 365 days"},
	},
	{
		Key:         "shared_data_dir",
		Display:     "Shared Data Folder",
		Description: "Directory accessible to all agents (optional)",
		Type:        FieldPath,
	},
}

func ptrFloat(f float64) *float64 { return &f }
