package icm

import "strings"

// unwrapJudgeJSON normalizes a judge LLM's verdict-JSON response. Small
// "judge" postures (Haiku-class) frequently wrap their JSON output in a
// Markdown code fence (```json ... ``` or bare ``` ... ```) or prefix it
// with a short preamble, even when the rubric asks for raw JSON. Treating
// that as a fatal parse failure would cause an otherwise-fine loop to
// never converge, so we strip the wrapping defensively.
//
// Stripping order:
//  1. Trim whitespace.
//  2. If the body is fenced (```lang? ... ```), strip the fence.
//  3. Otherwise, if `{` appears later in the string, drop everything
//     before the first `{` and after the matching closing `}`. This
//     handles "Here's my verdict: {...}" preambles.
//  4. Otherwise return as-is and let the JSON parser produce a clean
//     error.
//
// The function never returns an empty string when the input has any
// non-whitespace content; the caller still gets a chance to report a
// meaningful parse error.
func unwrapJudgeJSON(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return s
	}

	// Fenced block: ```json\n{...}\n``` or ```\n{...}\n```
	if strings.HasPrefix(s, "```") {
		// Drop the opening fence + optional language tag, then drop the
		// trailing fence. Keep everything in between.
		end := strings.LastIndex(s, "```")
		if end > 3 {
			inner := s[3:end]
			// Strip an optional language hint on the first line.
			if nl := strings.IndexByte(inner, '\n'); nl >= 0 {
				firstLine := strings.TrimSpace(inner[:nl])
				if isLangHint(firstLine) {
					inner = inner[nl+1:]
				}
			}
			return strings.TrimSpace(inner)
		}
	}

	// Preamble + JSON body: trim anything outside the outermost braces.
	if i := strings.IndexByte(s, '{'); i > 0 {
		if j := strings.LastIndexByte(s, '}'); j > i {
			return strings.TrimSpace(s[i : j+1])
		}
	}

	return s
}

// isLangHint reports whether s looks like a code-fence language tag
// (e.g. "json", "JSON") rather than the first line of JSON content.
// A language hint is a short, all-letters string. Anything containing
// braces, colons, or quotes is JSON content.
func isLangHint(s string) bool {
	if s == "" || len(s) > 16 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}
