package engine

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// checkUnknownConfigKeys rejects a YAML key the typed config would silently
// drop.
//
// Why this exists. yaml.v3 is non-strict by default, so a key the struct does
// not name is discarded without a word, and the schema validator that guards
// plugin blocks with additionalProperties:false never sees the engine's own
// blocks — it rebuilds them from the already-decoded typed config, by which
// point the dropped key is gone. The result was the one hole in the
// "misconfiguration fails at boot" promise this loader otherwise keeps:
//
//	core:
//	  object_stor:          # note the typo
//	    backend: s3
//	    bucket: nexus-prod
//
// booted clean with object storage silently disabled. Every turn succeeded,
// nothing was ever uploaded, and the first sign of trouble was an empty bucket
// after the host was replaced. A typo *inside* a correctly-spelled block was
// always caught, because the block's own validation ran; it was the block name
// that was unguarded.
//
// Why reflection rather than a literal allowlist. A hand-kept list of valid
// keys is a second source of truth that drifts the first time someone adds a
// field and forgets it — and it fails open, accepting nothing and rejecting a
// legitimate new key, which is worse than the hole it closes. Walking the yaml
// tags means the check is defined by the struct it is protecting.
//
// What it does not walk:
//
//   - `plugins:`, whose keys are plugin IDs rather than struct fields. Those
//     blocks already fail closed via their JSON Schemas.
//   - map-, slice- and interface-typed fields, which legitimately hold
//     arbitrary keys. core.models is the motivating case: it is yaml:"-" on the
//     struct and parsed out of the raw map by hand, so it is allowed by name
//     and its contents are left alone.
//
// Unknown keys anywhere else are a boot failure naming the offending path and
// the keys that were valid there.
func checkUnknownConfigKeys(raw map[string]any) error {
	var problems []string
	walkUnknownKeys(reflect.TypeOf(Config{}), raw, "", &problems)
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("config: %s", strings.Join(problems, "; "))
}

// keysHandledOutsideTheStruct are yaml keys the loader parses from the raw map
// by hand, so they carry no struct tag to be discovered by reflection. Each maps
// to the path it is valid at.
//
// Keeping this tiny and explicit is deliberate: every entry is a place where the
// typed config is not the whole truth, and that is worth being able to enumerate.
var keysHandledOutsideTheStruct = map[string]string{
	"core.models": "parsed into CoreConfig.ModelsRaw by LoadConfigFromBytes",
}

// pathsNotWalked are blocks whose keys are data rather than field names.
var pathsNotWalked = map[string]bool{
	"plugins": true,
}

func walkUnknownKeys(t reflect.Type, raw map[string]any, prefix string, problems *[]string) {
	if t.Kind() != reflect.Struct {
		return
	}

	fields := yamlFields(t)
	valid := make([]string, 0, len(fields))
	for name := range fields {
		valid = append(valid, name)
	}
	for full := range keysHandledOutsideTheStruct {
		if parent, key, ok := splitLastSegment(full); ok && parent == prefix {
			valid = append(valid, key)
		}
	}
	sort.Strings(valid)
	allowed := make(map[string]bool, len(valid))
	for _, name := range valid {
		allowed[name] = true
	}

	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}

		if !allowed[key] {
			*problems = append(*problems, fmt.Sprintf(
				"unknown key %q (valid keys here: %s)", path, strings.Join(valid, ", ")))
			continue
		}
		if pathsNotWalked[path] {
			continue
		}

		field, ok := fields[key]
		if !ok {
			// Handled outside the struct — allowed by name, contents opaque.
			continue
		}
		nested, ok := raw[key].(map[string]any)
		if !ok {
			continue
		}
		walkUnknownKeys(derefType(field), nested, path, problems)
	}
}

// yamlFields maps a struct's yaml key to its field type, skipping yaml:"-"
// and any field whose contents are arbitrary rather than named.
func yamlFields(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = f.Type
	}
	return out
}

// derefType unwraps a pointer so a *T field walks T's fields. Anything that is
// not a struct after unwrapping stops the recursion in walkUnknownKeys.
func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

func splitLastSegment(path string) (parent, key string, ok bool) {
	i := strings.LastIndex(path, ".")
	if i < 0 {
		return "", path, true
	}
	return path[:i], path[i+1:], true
}
