package agui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// state.go provides the minimal RFC 6902 JSON Patch helpers the AG-UI shared-
// state feature needs: DiffState computes an ordered patch that transforms one
// JSON document into another, and ApplyPatch replays such a patch. Together they
// let a StateDelta be produced from two StateSnapshots and let a consumer (or a
// test) reconstruct the snapshot by applying the deltas in order.
//
// Scope: these helpers intentionally cover only the operation set DiffState
// emits — "add", "remove", and "replace" — over the JSON value shapes the scene
// store produces (objects, arrays, scalars). They are not a general RFC 6902
// engine (no "move"/"copy"/"test"), which keeps the surface small and the
// behavior easy to reason about for the state-sync path.

// DiffState computes an RFC 6902 JSON Patch that transforms oldState into
// newState. Both inputs are raw JSON documents; a nil/empty input is treated as
// the JSON null document. The returned patch is deterministic (object keys are
// visited in sorted order) so successive diffs over the same states are stable,
// and applying it in order to oldState via ApplyPatch reproduces newState.
func DiffState(oldState, newState json.RawMessage) (JSONPatch, error) {
	oldVal, err := decodeState(oldState)
	if err != nil {
		return nil, fmt.Errorf("agui: decode old state: %w", err)
	}
	newVal, err := decodeState(newState)
	if err != nil {
		return nil, fmt.Errorf("agui: decode new state: %w", err)
	}
	var ops JSONPatch
	if err := diffValue("", oldVal, newVal, &ops); err != nil {
		return nil, err
	}
	return ops, nil
}

// decodeState unmarshals a raw JSON document into a generic value. An empty
// input decodes to nil (the JSON null document) so callers may pass nil for an
// absent state.
func decodeState(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// diffValue appends the ops needed to turn old into new at the given JSON
// pointer path onto ops.
func diffValue(path string, old, new any, ops *JSONPatch) error {
	if jsonEqual(old, new) {
		return nil
	}

	oldObj, oldIsObj := old.(map[string]any)
	newObj, newIsObj := new.(map[string]any)
	if oldIsObj && newIsObj {
		return diffObject(path, oldObj, newObj, ops)
	}

	// Any other shape change (scalar↔scalar, array↔array, type change) is a
	// whole-value replace at this path. Root replacement (empty path) still uses
	// "replace" per RFC 6902 — the whole document is the target.
	value, err := json.Marshal(new)
	if err != nil {
		return fmt.Errorf("agui: marshal replacement at %q: %w", path, err)
	}
	*ops = append(*ops, JSONPatchOp{Op: "replace", Path: path, Value: value})
	return nil
}

// diffObject appends the ops needed to turn old into new for two JSON objects.
// Keys are visited in sorted order so the patch is deterministic.
func diffObject(path string, old, new map[string]any, ops *JSONPatch) error {
	keys := make([]string, 0, len(old)+len(new))
	seen := make(map[string]bool, len(old)+len(new))
	for k := range old {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range new {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	for _, k := range keys {
		child := appendPointer(path, k)
		oldChild, inOld := old[k]
		newChild, inNew := new[k]
		switch {
		case inOld && !inNew:
			*ops = append(*ops, JSONPatchOp{Op: "remove", Path: child})
		case !inOld && inNew:
			value, err := json.Marshal(newChild)
			if err != nil {
				return fmt.Errorf("agui: marshal added value at %q: %w", child, err)
			}
			*ops = append(*ops, JSONPatchOp{Op: "add", Path: child, Value: value})
		default:
			if err := diffValue(child, oldChild, newChild, ops); err != nil {
				return err
			}
		}
	}
	return nil
}

// ApplyPatch replays an RFC 6902 patch onto a JSON document and returns the
// resulting document. It supports the add/remove/replace operation set DiffState
// emits over object and scalar shapes; unsupported ops return an error. It is
// used to verify that snapshot+deltas reconstructs the intended state.
func ApplyPatch(doc json.RawMessage, patch JSONPatch) (json.RawMessage, error) {
	root, err := decodeState(doc)
	if err != nil {
		return nil, fmt.Errorf("agui: decode document: %w", err)
	}
	for i, op := range patch {
		switch op.Op {
		case "add", "replace":
			var value any
			if len(op.Value) > 0 {
				if err := json.Unmarshal(op.Value, &value); err != nil {
					return nil, fmt.Errorf("agui: op %d decode value: %w", i, err)
				}
			}
			root, err = setPointer(root, op.Path, value)
			if err != nil {
				return nil, fmt.Errorf("agui: op %d (%s %s): %w", i, op.Op, op.Path, err)
			}
		case "remove":
			root, err = removePointer(root, op.Path)
			if err != nil {
				return nil, fmt.Errorf("agui: op %d (remove %s): %w", i, op.Path, err)
			}
		default:
			return nil, fmt.Errorf("agui: op %d: unsupported op %q", i, op.Op)
		}
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("agui: marshal result: %w", err)
	}
	return out, nil
}

// setPointer sets the value at the given JSON pointer within root, returning the
// (possibly new) root. An empty pointer replaces the whole document.
func setPointer(root any, pointer string, value any) (any, error) {
	if pointer == "" {
		return value, nil
	}
	tokens := parsePointer(pointer)
	return setTokens(root, tokens, value)
}

func setTokens(node any, tokens []string, value any) (any, error) {
	key := tokens[0]
	obj, ok := node.(map[string]any)
	if !ok {
		if node == nil {
			obj = map[string]any{}
		} else {
			return nil, fmt.Errorf("cannot descend into non-object at %q", key)
		}
	}
	if len(tokens) == 1 {
		obj[key] = value
		return obj, nil
	}
	child, err := setTokens(obj[key], tokens[1:], value)
	if err != nil {
		return nil, err
	}
	obj[key] = child
	return obj, nil
}

// removePointer removes the value at the given JSON pointer within root.
func removePointer(root any, pointer string) (any, error) {
	if pointer == "" {
		return nil, nil
	}
	tokens := parsePointer(pointer)
	return removeTokens(root, tokens)
}

func removeTokens(node any, tokens []string) (any, error) {
	key := tokens[0]
	obj, ok := node.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("cannot descend into non-object at %q", key)
	}
	if len(tokens) == 1 {
		delete(obj, key)
		return obj, nil
	}
	child, err := removeTokens(obj[key], tokens[1:])
	if err != nil {
		return nil, err
	}
	obj[key] = child
	return obj, nil
}

// appendPointer appends a single (unescaped) token to a JSON pointer, applying
// RFC 6901 escaping ("~" -> "~0", "/" -> "~1").
func appendPointer(base, token string) string {
	return base + "/" + escapeToken(token)
}

// parsePointer splits a JSON pointer into its decoded reference tokens.
func parsePointer(pointer string) []string {
	parts := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for i, p := range parts {
		parts[i] = unescapeToken(p)
	}
	return parts
}

func escapeToken(t string) string {
	t = strings.ReplaceAll(t, "~", "~0")
	t = strings.ReplaceAll(t, "/", "~1")
	return t
}

func unescapeToken(t string) string {
	t = strings.ReplaceAll(t, "~1", "/")
	t = strings.ReplaceAll(t, "~0", "~")
	return t
}

// jsonEqual reports whether two decoded JSON values are deeply equal. It relies
// on canonical marshaling so map key ordering does not affect the result.
func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}
