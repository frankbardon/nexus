package agui

import (
	"encoding/json"
	"testing"
)

// applyAll applies a patch to a document and returns the canonical JSON string,
// failing the test on error.
func applyAll(t *testing.T, doc json.RawMessage, patch JSONPatch) string {
	t.Helper()
	out, err := ApplyPatch(doc, patch)
	if err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	return canonical(t, out)
}

// canonical re-marshals a JSON document so map key ordering is normalized for
// string comparison.
func canonical(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(out)
}

func TestDiffState_AddKey(t *testing.T) {
	old := json.RawMessage(`{"a":1}`)
	new := json.RawMessage(`{"a":1,"b":2}`)
	patch, err := DiffState(old, new)
	if err != nil {
		t.Fatalf("DiffState: %v", err)
	}
	if len(patch) != 1 || patch[0].Op != "add" || patch[0].Path != "/b" {
		t.Fatalf("patch = %+v, want single add /b", patch)
	}
	if got, want := applyAll(t, old, patch), canonical(t, new); got != want {
		t.Errorf("reconstruct = %s, want %s", got, want)
	}
}

func TestDiffState_RemoveKey(t *testing.T) {
	old := json.RawMessage(`{"a":1,"b":2}`)
	new := json.RawMessage(`{"a":1}`)
	patch, err := DiffState(old, new)
	if err != nil {
		t.Fatalf("DiffState: %v", err)
	}
	if len(patch) != 1 || patch[0].Op != "remove" || patch[0].Path != "/b" {
		t.Fatalf("patch = %+v, want single remove /b", patch)
	}
	if got, want := applyAll(t, old, patch), canonical(t, new); got != want {
		t.Errorf("reconstruct = %s, want %s", got, want)
	}
}

func TestDiffState_ReplaceScalar(t *testing.T) {
	old := json.RawMessage(`{"a":1}`)
	new := json.RawMessage(`{"a":2}`)
	patch, err := DiffState(old, new)
	if err != nil {
		t.Fatalf("DiffState: %v", err)
	}
	if len(patch) != 1 || patch[0].Op != "replace" || patch[0].Path != "/a" {
		t.Fatalf("patch = %+v, want single replace /a", patch)
	}
	if got, want := applyAll(t, old, patch), canonical(t, new); got != want {
		t.Errorf("reconstruct = %s, want %s", got, want)
	}
}

func TestDiffState_NestedObject(t *testing.T) {
	old := json.RawMessage(`{"scene_1":{"title":"draft","body":"x"}}`)
	new := json.RawMessage(`{"scene_1":{"title":"final","body":"x","tags":["a"]}}`)
	patch, err := DiffState(old, new)
	if err != nil {
		t.Fatalf("DiffState: %v", err)
	}
	// title replaced, tags added; body unchanged (no op).
	if len(patch) != 2 {
		t.Fatalf("patch = %+v, want 2 ops", patch)
	}
	if got, want := applyAll(t, old, patch), canonical(t, new); got != want {
		t.Errorf("reconstruct = %s, want %s", got, want)
	}
}

func TestDiffState_EmptyToObject(t *testing.T) {
	old := json.RawMessage(`{}`)
	new := json.RawMessage(`{"scene_1":{"k":"v"}}`)
	patch, err := DiffState(old, new)
	if err != nil {
		t.Fatalf("DiffState: %v", err)
	}
	if got, want := applyAll(t, old, patch), canonical(t, new); got != want {
		t.Errorf("reconstruct = %s, want %s", got, want)
	}
}

func TestDiffState_NilOldState(t *testing.T) {
	new := json.RawMessage(`{"a":1}`)
	patch, err := DiffState(nil, new)
	if err != nil {
		t.Fatalf("DiffState: %v", err)
	}
	// nil (null) -> object is a whole-document replace at root.
	if len(patch) != 1 || patch[0].Op != "replace" || patch[0].Path != "" {
		t.Fatalf("patch = %+v, want single root replace", patch)
	}
	if got, want := applyAll(t, nil, patch), canonical(t, new); got != want {
		t.Errorf("reconstruct = %s, want %s", got, want)
	}
}

func TestDiffState_NoChange(t *testing.T) {
	doc := json.RawMessage(`{"a":{"b":2},"c":[1,2,3]}`)
	patch, err := DiffState(doc, doc)
	if err != nil {
		t.Fatalf("DiffState: %v", err)
	}
	if len(patch) != 0 {
		t.Fatalf("patch = %+v, want empty", patch)
	}
}

func TestDiffState_KeyWithSlash(t *testing.T) {
	old := json.RawMessage(`{}`)
	new := json.RawMessage(`{"a/b":1}`)
	patch, err := DiffState(old, new)
	if err != nil {
		t.Fatalf("DiffState: %v", err)
	}
	if len(patch) != 1 || patch[0].Path != "/a~1b" {
		t.Fatalf("patch = %+v, want escaped pointer /a~1b", patch)
	}
	if got, want := applyAll(t, old, patch), canonical(t, new); got != want {
		t.Errorf("reconstruct = %s, want %s", got, want)
	}
}

// TestDiffState_OrderedReconstruction applies a sequence of diffs (as a client
// would apply successive StateDeltas) and asserts the final state matches the
// last snapshot.
func TestDiffState_OrderedReconstruction(t *testing.T) {
	states := []json.RawMessage{
		json.RawMessage(`{}`),
		json.RawMessage(`{"s1":{"v":1}}`),
		json.RawMessage(`{"s1":{"v":2}}`),
		json.RawMessage(`{"s1":{"v":2},"s2":{"v":9}}`),
		json.RawMessage(`{"s2":{"v":9}}`),
	}
	cur := states[0]
	for i := 1; i < len(states); i++ {
		patch, err := DiffState(cur, states[i])
		if err != nil {
			t.Fatalf("DiffState step %d: %v", i, err)
		}
		out, err := ApplyPatch(cur, patch)
		if err != nil {
			t.Fatalf("ApplyPatch step %d: %v", i, err)
		}
		if got, want := canonical(t, out), canonical(t, states[i]); got != want {
			t.Fatalf("step %d reconstruct = %s, want %s", i, got, want)
		}
		cur = out
	}
}
