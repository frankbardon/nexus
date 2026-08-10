package nexusauth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
)

// ValidatorTypeStatic is the config `type:` that selects StaticValidator.
const ValidatorTypeStatic = "static"

// StaticToken binds one shared secret to the identity it stands for.
type StaticToken struct {
	// Token is the bearer token value, compared in constant time.
	Token string

	// Principal is the identity a request presenting Token acts as.
	Principal Principal
}

// StaticValidator authenticates a bearer token against a fixed table of
// operator-configured tokens. It is the escape hatch that keeps the broker
// usable without an identity provider — service accounts, CI, a local operator
// CLI — and it makes the whole package testable with no network.
//
// It is immutable after construction and safe for concurrent use.
type StaticValidator struct {
	tokens []StaticToken
}

// NewStaticValidator builds a validator over tokens. It rejects a table that
// could never authenticate anything (no tokens), entries missing a token or a
// principal id, and duplicate token values — a duplicate is always an operator
// mistake, and resolving it silently would pin the identity to whichever entry
// happened to be listed last.
func NewStaticValidator(tokens []StaticToken) (*StaticValidator, error) {
	if len(tokens) == 0 {
		return nil, fmt.Errorf("at least one token is required")
	}
	seen := make(map[string]int, len(tokens))
	out := make([]StaticToken, 0, len(tokens))
	for i, t := range tokens {
		if t.Token == "" {
			return nil, fmt.Errorf("tokens[%d]: token must not be empty", i)
		}
		if t.Principal.ID == "" {
			return nil, fmt.Errorf("tokens[%d]: principal must not be empty", i)
		}
		if prev, dup := seen[t.Token]; dup {
			return nil, fmt.Errorf("tokens[%d]: duplicate token, already bound to principal %q at tokens[%d]",
				i, tokens[prev].Principal.ID, prev)
		}
		seen[t.Token] = i
		out = append(out, StaticToken{Token: t.Token, Principal: t.Principal.Clone()})
	}
	return &StaticValidator{tokens: out}, nil
}

// Validate implements Validator.
func (v *StaticValidator) Validate(_ context.Context, r *http.Request) (Principal, error) {
	presented := BearerToken(r)
	if presented == "" {
		return Principal{}, NewError(KindNoCredential, "no bearer token in the Authorization header", nil)
	}

	// Constant-time compare against every entry, and deliberately no early
	// break: returning as soon as a match is found would make response time
	// depend on the matching token's position in the table, leaking which
	// credential was presented. subtle.ConstantTimeCompare removes the
	// byte-level timing signal; scanning the whole table removes the
	// position-level one.
	match := -1
	for i := range v.tokens {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(v.tokens[i].Token)) == 1 {
			match = i
		}
	}
	if match < 0 {
		return Principal{}, NewError(KindInvalidCredential, "bearer token does not match any configured static token", nil)
	}
	return v.tokens[match].Principal.Clone(), nil
}
