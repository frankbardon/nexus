package desktop

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"

	"github.com/zalando/go-keyring"
)

const (
	// keychainService is the service name used for all keychain entries.
	keychainService = "nexus-desktop"

	// secretSentinel is stored in the JSON file for secret fields so
	// the frontend knows a value exists without exposing it.
	secretSentinel = "__keychain__"

	// settingsFileName is the name of the JSON settings file.
	settingsFileName = "settings.json"
)

// SettingsStore manages persistence of user settings. Non-secret
// values live in a JSON file; secrets live in the OS keychain.
type SettingsStore interface {
	// Get returns a plaintext setting value. Scope is an agent ID or
	// "shell". Returns (nil, false) if unset.
	Get(scope, key string) (any, bool)

	// Set writes a plaintext setting value.
	Set(scope, key string, value any) error

	// GetSecret reads a secret from the OS keychain.
	GetSecret(scope, key string) (string, error)

	// SetSecret writes a secret to the OS keychain and records a
	// sentinel in the JSON file so the UI knows a value exists.
	SetSecret(scope, key string, value string) error

	// DeleteSecret removes a secret from the OS keychain and removes
	// the sentinel from the JSON file.
	DeleteSecret(scope, key string) error

	// Delete removes a plaintext setting.
	Delete(scope, key string) error

	// Save flushes the JSON file to disk.
	Save() error

	// Resolve looks up a value with scope fallback: checks the given
	// scope first, then falls back to "shell" scope. For secret fields,
	// reads from keychain. Returns ("", false) if not found anywhere.
	Resolve(scope, key string, secret bool) (string, bool)
}

// settingsData is the on-disk JSON structure.
type settingsData struct {
	Version int                       `json:"version"`
	Shell   map[string]any            `json:"shell"`
	Agents  map[string]map[string]any `json:"agents"`
}

// fileStore implements SettingsStore backed by a JSON file and the
// OS keychain.
type fileStore struct {
	mu   sync.RWMutex
	path string // full path to settings.json
	data settingsData
}

// newFileStore creates or loads a settings store at the given directory.
func newFileStore(dir string) (*fileStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating settings dir: %w", err)
	}

	path := filepath.Join(dir, settingsFileName)
	s := &fileStore{
		path: path,
		data: settingsData{
			Version: 1,
			Shell:   make(map[string]any),
			Agents:  make(map[string]map[string]any),
		},
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil // fresh install, empty store
		}
		return nil, fmt.Errorf("reading settings: %w", err)
	}

	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("parsing settings: %w", err)
	}

	// Ensure maps are initialized even if the file had null values.
	if s.data.Shell == nil {
		s.data.Shell = make(map[string]any)
	}
	if s.data.Agents == nil {
		s.data.Agents = make(map[string]map[string]any)
	}

	return s, nil
}

func (s *fileStore) scopeMap(scope string) map[string]any {
	if scope == "shell" {
		return s.data.Shell
	}
	m, ok := s.data.Agents[scope]
	if !ok {
		m = make(map[string]any)
		s.data.Agents[scope] = m
	}
	return m
}

func (s *fileStore) Get(scope, key string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.scopeMap(scope)
	v, ok := m[key]
	return v, ok
}

func (s *fileStore) Set(scope, key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.scopeMap(scope)
	m[key] = value
	return s.saveLocked()
}

func (s *fileStore) Delete(scope, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.scopeMap(scope)
	delete(m, key)
	return s.saveLocked()
}

func keychainAccount(scope, key string) string {
	return scope + "." + key
}

func (s *fileStore) GetSecret(scope, key string) (string, error) {
	secret, err := keyring.Get(keychainService, keychainAccount(scope, key))
	if err != nil {
		if err == keyring.ErrNotFound {
			return "", nil
		}
		return "", fmt.Errorf("reading keychain: %w", err)
	}
	return secret, nil
}

func (s *fileStore) SetSecret(scope, key string, value string) error {
	if err := keyring.Set(keychainService, keychainAccount(scope, key), value); err != nil {
		return fmt.Errorf("writing keychain: %w", err)
	}

	// Record sentinel in JSON so the UI knows a value exists.
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.scopeMap(scope)
	m[key] = secretSentinel
	return s.saveLocked()
}

func (s *fileStore) DeleteSecret(scope, key string) error {
	err := keyring.Delete(keychainService, keychainAccount(scope, key))
	if err != nil && err != keyring.ErrNotFound {
		return fmt.Errorf("deleting keychain entry: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.scopeMap(scope)
	delete(m, key)
	return s.saveLocked()
}

func (s *fileStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *fileStore) saveLocked() error {
	raw, err := json.MarshalIndent(s.data, "", "    ")
	if err != nil {
		return fmt.Errorf("marshaling settings: %w", err)
	}
	if err := os.WriteFile(s.path, raw, 0o600); err != nil {
		return fmt.Errorf("writing settings: %w", err)
	}
	return nil
}

// Resolve looks up a value with scope fallback. For the given scope,
// it checks that scope first, then falls back to "shell" scope.
// For secret fields it reads from the keychain; for plaintext fields
// it reads from the JSON store. Returns the value as a string and
// whether it was found.
func (s *fileStore) Resolve(scope, key string, secret bool) (string, bool) {
	// Try agent scope first, then shell scope.
	scopes := []string{scope}
	if scope != "shell" {
		scopes = append(scopes, "shell")
	}

	for _, sc := range scopes {
		if secret {
			val, err := s.GetSecret(sc, key)
			if err == nil && val != "" {
				return val, true
			}
		} else {
			if val, ok := s.Get(sc, key); ok && val != secretSentinel {
				return fmt.Sprintf("%v", val), true
			}
		}
	}
	return "", false
}

// AllValues returns all non-secret settings for the frontend. Secret
// fields show the sentinel value, not the actual secret.
func (s *fileStore) AllValues() map[string]map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]map[string]any)

	shellCopy := make(map[string]any, len(s.data.Shell))
	maps.Copy(shellCopy, s.data.Shell)
	result["shell"] = shellCopy

	for agentID, m := range s.data.Agents {
		agentCopy := make(map[string]any, len(m))
		maps.Copy(agentCopy, m)
		result[agentID] = agentCopy
	}
	return result
}
