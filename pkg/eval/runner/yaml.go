package runner

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// yamlSetCoreSessionsRoot returns the YAML body with core.sessions.root set
// to root. If the path doesn't exist yet, it's inserted under an existing
// core.sessions or core map (creating intermediate nodes as needed).
//
// The yaml.v3 node API preserves comments and ordering of unrelated keys,
// which keeps test-bundle config.yaml round-trippable on disk.
func yamlSetCoreSessionsRoot(in []byte, root string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(in, &doc); err != nil {
		return nil, fmt.Errorf("unmarshal yaml: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		// Empty or non-mapping document: produce a synthetic one.
		out := map[string]any{
			"core": map[string]any{
				"sessions": map[string]any{
					"root": root,
				},
			},
		}
		return yaml.Marshal(out)
	}
	rootNode := doc.Content[0]
	if rootNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("yaml root is not a mapping")
	}
	core := getOrCreateMapping(rootNode, "core")
	sessions := getOrCreateMapping(core, "sessions")
	setStringKey(sessions, "root", root)
	return yaml.Marshal(&doc)
}

// getOrCreateMapping returns the mapping value of the given key under
// parent, creating an empty mapping (and the key) if absent.
func getOrCreateMapping(parent *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		k := parent.Content[i]
		if k.Value == key && k.Kind == yaml.ScalarNode {
			v := parent.Content[i+1]
			if v.Kind == yaml.MappingNode {
				return v
			}
			// Reset to mapping if the existing value is the wrong shape.
			v.Kind = yaml.MappingNode
			v.Tag = "!!map"
			v.Value = ""
			v.Content = nil
			return v
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"}
	valNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content, keyNode, valNode)
	return valNode
}

// setStringKey upserts a scalar string under parent[key].
func setStringKey(parent *yaml.Node, key, value string) {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		k := parent.Content[i]
		if k.Value == key && k.Kind == yaml.ScalarNode {
			v := parent.Content[i+1]
			v.Kind = yaml.ScalarNode
			v.Tag = "!!str"
			v.Value = value
			v.Content = nil
			return
		}
	}
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}
