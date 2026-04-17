package longterm

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"gopkg.in/yaml.v3"
)

// memoryFile is the on-disk representation with YAML frontmatter.
type memoryFile struct {
	Key           string            `yaml:"key"`
	Tags          map[string]string `yaml:"tags,omitempty"`
	Created       time.Time         `yaml:"created"`
	Updated       time.Time         `yaml:"updated"`
	SourceSession string            `yaml:"source_session"`
}

var keySanitizer = regexp.MustCompile(`[^a-z0-9_-]`)

// sanitizeKey normalises a key into a safe filename stem.
func sanitizeKey(raw string) string {
	k := strings.ToLower(strings.TrimSpace(raw))
	k = keySanitizer.ReplaceAllString(k, "_")
	if len(k) > 128 {
		k = k[:128]
	}
	return k
}

// store persists a memory entry to disk, creating or updating the file.
func store(dir string, req events.LongTermMemoryStoreRequest, sessionID string) error {
	key := sanitizeKey(req.Key)
	if key == "" {
		return fmt.Errorf("longterm: empty key after sanitization")
	}

	path := filepath.Join(dir, key+".md")

	now := time.Now().UTC()
	mf := memoryFile{
		Key:           key,
		Tags:          req.Tags,
		Updated:       now,
		SourceSession: sessionID,
	}

	// Preserve original created timestamp on update.
	if existing, err := readFile(path); err == nil {
		mf.Created = existing.Created
	} else {
		mf.Created = now
	}

	return writeFile(path, mf, req.Content)
}

// read loads a memory entry from disk by key.
func read(dir, key string) (*events.LongTermMemoryEntry, error) {
	key = sanitizeKey(key)
	path := filepath.Join(dir, key+".md")
	return readFile(path)
}

// remove deletes a memory file by key.
func remove(dir, key string) error {
	key = sanitizeKey(key)
	path := filepath.Join(dir, key+".md")
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("longterm: deleting %s: %w", path, err)
	}
	return nil
}

// list scans a directory for memory files and returns index entries,
// optionally filtered by tags (AND semantics).
func list(dir string, tags map[string]string) ([]events.LongTermMemoryIndex, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("longterm: listing %s: %w", dir, err)
	}

	var result []events.LongTermMemoryIndex
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		mem, err := readFile(path)
		if err != nil {
			continue // skip malformed files
		}

		if !matchTags(mem.Tags, tags) {
			continue
		}

		result = append(result, events.LongTermMemoryIndex{
			Key:     mem.Key,
			Preview: firstLine(mem.Content),
			Tags:    mem.Tags,
			Updated: mem.Updated,
		})
	}

	return result, nil
}

// readFile parses a single memory markdown file.
func readFile(path string) (*events.LongTermMemoryEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := string(data)
	var mf memoryFile
	var body string

	if strings.HasPrefix(strings.TrimSpace(content), "---") {
		trimmed := strings.TrimSpace(content)
		rest := trimmed[3:]
		endIdx := strings.Index(rest, "\n---")
		if endIdx == -1 {
			body = content
		} else {
			frontmatter := rest[:endIdx]
			body = strings.TrimSpace(rest[endIdx+4:])
			if err := yaml.Unmarshal([]byte(frontmatter), &mf); err != nil {
				return nil, fmt.Errorf("longterm: parsing frontmatter in %s: %w", path, err)
			}
		}
	} else {
		body = content
	}

	return &events.LongTermMemoryEntry{
		Key:           mf.Key,
		Content:       body,
		Tags:          mf.Tags,
		Created:       mf.Created,
		Updated:       mf.Updated,
		SourceSession: mf.SourceSession,
	}, nil
}

// writeFile serialises a memory entry as YAML frontmatter + markdown body.
func writeFile(path string, mf memoryFile, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("longterm: creating directory: %w", err)
	}

	fm, err := yaml.Marshal(mf)
	if err != nil {
		return fmt.Errorf("longterm: marshalling frontmatter: %w", err)
	}

	var buf strings.Builder
	buf.WriteString("---\n")
	buf.Write(fm)
	buf.WriteString("---\n\n")
	buf.WriteString(body)
	buf.WriteString("\n")

	return os.WriteFile(path, []byte(buf.String()), 0o644)
}

// matchTags returns true if entry tags contain all filter tags (AND).
func matchTags(entryTags, filter map[string]string) bool {
	for k, v := range filter {
		if entryTags[k] != v {
			return false
		}
	}
	return true
}

// firstLine returns the first non-empty line of s.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}
