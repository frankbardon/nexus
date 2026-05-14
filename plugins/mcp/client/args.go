package client

import (
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// parseSlashArgs parses a slash-command argument string against a prompt's
// declared MCP arguments. Supports positional and `k=v` forms; positional
// values map to required arguments in declaration order. `k=v` values can
// appear anywhere and override the positional slot.
//
// Unknown keys produce an error so typos are surfaced to the user rather
// than silently dropped. Missing required args also error out.
//
// Quoted arguments are supported with a small shell-style tokenizer that
// honours double quotes; backslash escapes inside quotes are passed through
// literally to keep the parser predictable for non-shell users.
func parseSlashArgs(raw string, decl []mcp.PromptArgument) (map[string]string, error) {
	tokens, err := tokenizeArgs(raw)
	if err != nil {
		return nil, err
	}

	result := map[string]string{}

	declByName := map[string]bool{}
	for _, a := range decl {
		declByName[a.Name] = true
	}

	posIdx := 0
	for _, tok := range tokens {
		if eq := strings.IndexByte(tok, '='); eq > 0 && !strings.HasPrefix(tok, "=") {
			key := tok[:eq]
			val := tok[eq+1:]
			if !declByName[key] {
				return nil, fmt.Errorf("unknown argument %q", key)
			}
			result[key] = val
			continue
		}

		for posIdx < len(decl) {
			arg := decl[posIdx]
			posIdx++
			if _, taken := result[arg.Name]; taken {
				continue
			}
			result[arg.Name] = tok
			break
		}
	}

	for _, a := range decl {
		if a.Required {
			if _, ok := result[a.Name]; !ok {
				return nil, fmt.Errorf("missing required argument %q", a.Name)
			}
		}
	}

	return result, nil
}

// tokenizeArgs splits a raw argument string into tokens, honouring double
// quotes for values that contain spaces. Returns an error on unterminated
// quotes so the caller can give the user a clear failure rather than a
// silently-misparsed command.
func tokenizeArgs(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var tokens []string
	var buf strings.Builder
	inQuote := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case c == ' ' && !inQuote:
			if buf.Len() > 0 {
				tokens = append(tokens, buf.String())
				buf.Reset()
			}
		default:
			buf.WriteByte(c)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote")
	}
	if buf.Len() > 0 {
		tokens = append(tokens, buf.String())
	}
	return tokens, nil
}
