package openaiconform

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

// BuildFunc produces one vector's request body using the surface's own
// serializer.
//
// It takes *testing.T so a driver can fail fast on something that is not a
// conformance question — a builder that returned an error, say.
type BuildFunc func(t *testing.T, v Vector) map[string]any

// ParseFunc parses one reply fixture using the surface's own parser, returning
// whatever the surface would publish and whatever it would report as a failure.
type ParseFunc func(t *testing.T, rv ReplyVector) (*events.LLMResponse, error)

// RunRequestSuite replays the whole request corpus against one surface.
//
// Each vector is a subtest named for its id, so a failure names the behaviour
// that drifted rather than a line number.
func RunRequestSuite(t *testing.T, surface Surface, build BuildFunc) {
	t.Helper()
	if !surface.Known() {
		t.Fatalf("openaiconform: %q is not a declared surface (want one of %v)", surface, Surfaces())
	}
	for _, v := range Vectors() {
		t.Run(v.ID, func(t *testing.T) {
			body := build(t, v)
			if body == nil {
				t.Fatalf("the driver produced no body for vector %s", v.ID)
			}
			if errs := CheckBody(v, surface, body); len(errs) > 0 {
				t.Error(report("request", surface, v.ID, v.Title, v.Rationale, errs, body))
			}
		})
	}
}

// RunReplySuite replays the whole reply corpus against one surface.
func RunReplySuite(t *testing.T, surface Surface, parse ParseFunc) {
	t.Helper()
	if !surface.Known() {
		t.Fatalf("openaiconform: %q is not a declared surface (want one of %v)", surface, Surfaces())
	}
	for _, rv := range ReplyVectors() {
		t.Run(rv.ID, func(t *testing.T) {
			resp, err := parse(t, rv)
			if errs := CheckReply(rv, surface, resp, err); len(errs) > 0 {
				t.Error(report("reply", surface, rv.ID, rv.Title, rv.Rationale, errs, resp))
			}
		})
	}
}

// report renders one vector's failures.
//
// Every disagreement is listed rather than only the first: a surface that
// drifted has usually drifted in more than one place, and discovering them one
// test run at a time is how a small divergence becomes a long afternoon. The
// rationale is printed because the first instinct on a red conformance test is
// to change the expectation, and the closing note says why that is the wrong
// move.
func report(kind string, surface Surface, id, title, rationale string, errs []error, observed any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s does not conform to %s vector %q (%s)\n", surface, kind, id, title)
	fmt.Fprintf(&b, "  why the vector expects what it does: %s\n", rationale)
	for _, err := range errs {
		fmt.Fprintf(&b, "  - %v\n", err)
	}
	fmt.Fprintf(&b, "  observed:\n%s\n", indent(pretty(observed)))
	b.WriteString("  The other OpenAI Responses surface is pinned to this SAME vector. " +
		"Do not weaken the vector or trim the expected body to make this pass: either this surface has a bug, " +
		"or the difference is legitimate and belongs in RequestDivergences/ReplyDivergences with a rationale.")
	return b.String()
}

func pretty(v any) string {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%#v", v)
	}
	return string(raw)
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}
