package engine

import (
	"sort"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestRequestHeaderLabelKeyRoundTrip(t *testing.T) {
	key := RequestHeaderLabelKey("tenant-id")
	if key != "_header.tenant-id" {
		t.Fatalf("RequestHeaderLabelKey = %q, want %q", key, "_header.tenant-id")
	}
	if !IsReservedLabelKey(key) {
		t.Error("a request-header label key must sit in the reserved namespace")
	}
	name, ok := RequestHeaderName(key)
	if !ok || name != "tenant-id" {
		t.Errorf("RequestHeaderName(%q) = (%q, %v), want (%q, true)", key, name, ok, "tenant-id")
	}
	for _, other := range []string{"tenant-id", "_principal_id", "_header.", "_headerx"} {
		if _, ok := RequestHeaderName(other); ok {
			t.Errorf("RequestHeaderName(%q) reported a header label", other)
		}
	}
}

func TestSetRequestHeaders_BindsAndReads(t *testing.T) {
	ws, _ := newTagTestEngine(t)

	if err := ws.SetRequestHeaders(map[string]string{"tenant": "acme", "timezone": "Europe/Amsterdam"}); err != nil {
		t.Fatalf("SetRequestHeaders: %v", err)
	}

	got, err := ws.RequestHeaders()
	if err != nil {
		t.Fatalf("RequestHeaders: %v", err)
	}
	if got["tenant"] != "acme" || got["timezone"] != "Europe/Amsterdam" {
		t.Fatalf("RequestHeaders = %v", got)
	}
	if v, ok := ws.RequestHeader("tenant"); !ok || v != "acme" {
		t.Errorf("RequestHeader(tenant) = (%q, %v)", v, ok)
	}
	if _, ok := ws.RequestHeader("absent"); ok {
		t.Error("RequestHeader reported an unbound header as present")
	}

	meta, err := ws.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if meta.Labels["_header.tenant"] != "acme" {
		t.Errorf("labels = %v, want _header.tenant bound", meta.Labels)
	}
}

// The whole point of the replace semantics: a turn that arrives without a
// header must not be read by a plugin as still carrying the previous turn's.
func TestSetRequestHeaders_ReplacesPreviousSet(t *testing.T) {
	ws, _ := newTagTestEngine(t)

	if err := ws.SetRequestHeaders(map[string]string{"tenant": "acme", "locale": "nl-NL"}); err != nil {
		t.Fatalf("first SetRequestHeaders: %v", err)
	}
	if err := ws.SetRequestHeaders(map[string]string{"tenant": "globex"}); err != nil {
		t.Fatalf("second SetRequestHeaders: %v", err)
	}

	got, err := ws.RequestHeaders()
	if err != nil {
		t.Fatalf("RequestHeaders: %v", err)
	}
	if len(got) != 1 || got["tenant"] != "globex" {
		t.Fatalf("RequestHeaders = %v, want only tenant=globex", got)
	}
}

func TestSetRequestHeaders_EmptyClearsNamespace(t *testing.T) {
	ws, _ := newTagTestEngine(t)

	if err := ws.SetRequestHeaders(map[string]string{"tenant": "acme"}); err != nil {
		t.Fatalf("SetRequestHeaders: %v", err)
	}
	if err := ws.SetRequestHeaders(nil); err != nil {
		t.Fatalf("clearing SetRequestHeaders: %v", err)
	}

	got, err := ws.RequestHeaders()
	if err != nil {
		t.Fatalf("RequestHeaders: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("RequestHeaders = %v, want empty", got)
	}
}

// General-namespace labels an agent or a transport wrote are none of this
// call's business and must survive a header replacement untouched.
func TestSetRequestHeaders_LeavesOtherLabelsAlone(t *testing.T) {
	ws, _ := newTagTestEngine(t)

	if err := ws.SetLabel("project", "apollo"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	if err := ws.SetReservedLabel("_principal_id", "user-1"); err != nil {
		t.Fatalf("SetReservedLabel: %v", err)
	}
	if err := ws.SetRequestHeaders(map[string]string{"tenant": "acme"}); err != nil {
		t.Fatalf("SetRequestHeaders: %v", err)
	}
	if err := ws.SetRequestHeaders(nil); err != nil {
		t.Fatalf("clearing SetRequestHeaders: %v", err)
	}

	meta, err := ws.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if meta.Labels["project"] != "apollo" {
		t.Errorf("general label lost: %v", meta.Labels)
	}
	if meta.Labels["_principal_id"] != "user-1" {
		t.Errorf("unrelated reserved label lost: %v", meta.Labels)
	}
}

// A header label must be unreachable from the general bus path, which is what
// stops a plugin (or an agent holding the session_tags tools) forging one.
func TestRequestHeaderLabel_NotWritableOverTheBus(t *testing.T) {
	ws, bus := newTagTestEngine(t)

	req := &events.SessionTagSetRequest{
		SchemaVersion: events.SessionTagSetRequestVersion,
		Key:           RequestHeaderLabelKey("tenant"),
		Value:         "forged",
	}
	veto, err := bus.EmitVetoable("before:session.tag.set", req)
	if err != nil {
		t.Fatalf("EmitVetoable: %v", err)
	}
	if !veto.Vetoed {
		t.Fatal("writing a request-header label over the bus was not vetoed")
	}
	got, err := ws.RequestHeaders()
	if err != nil {
		t.Fatalf("RequestHeaders: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("RequestHeaders = %v, want nothing written", got)
	}
}

func TestSetRequestHeaders_RejectsUnusableName(t *testing.T) {
	ws, _ := newTagTestEngine(t)
	if err := ws.SetRequestHeaders(map[string]string{"bad name": "v"}); err == nil {
		t.Fatal("SetRequestHeaders accepted a name that cannot be a label key")
	}
}

// One replacement must be one write, announced per key in a defined order —
// the batch form's whole reason for existing.
func TestSetRequestHeaders_AnnouncesEveryChange(t *testing.T) {
	ws, bus := newTagTestEngine(t)

	if err := ws.SetRequestHeaders(map[string]string{"a": "1", "b": "2"}); err != nil {
		t.Fatalf("SetRequestHeaders: %v", err)
	}

	var set, deleted []string
	unsubSet := bus.Subscribe("session.tag.set", func(ev Event[any]) {
		if p, ok := ev.Payload.(events.SessionTagSet); ok {
			set = append(set, p.Key)
		}
	})
	defer unsubSet()
	unsubDel := bus.Subscribe("session.tag.deleted", func(ev Event[any]) {
		if p, ok := ev.Payload.(events.SessionTagDeleted); ok {
			deleted = append(deleted, p.Key)
		}
	})
	defer unsubDel()

	if err := ws.SetRequestHeaders(map[string]string{"b": "22", "c": "3"}); err != nil {
		t.Fatalf("second SetRequestHeaders: %v", err)
	}

	sort.Strings(set)
	if len(set) != 2 || set[0] != "_header.b" || set[1] != "_header.c" {
		t.Errorf("session.tag.set keys = %v, want the two bound headers", set)
	}
	if len(deleted) != 1 || deleted[0] != "_header.a" {
		t.Errorf("session.tag.deleted keys = %v, want the dropped header", deleted)
	}
}
