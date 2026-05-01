package engine

import "context"

// replayKey is the unexported context key used to mark a context as in
// replay mode. Phase 1 ships only the helpers; the full replay coordinator
// lands in Phase 2 (deterministic re-dispatch) and Phase 3 (provider/tool
// short-circuit + crash resume).
type replayKey struct{}

// WithReplay returns a derived context tagged as a replay context.
// Side-effecting plugins inspect the context via IsReplay and return the
// journaled response instead of calling out.
func WithReplay(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, replayKey{}, true)
}

// IsReplay reports whether the calling context is a replay context. Always
// false outside of the Phase 2+ coordinator path.
func IsReplay(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, ok := ctx.Value(replayKey{}).(bool)
	return ok && v
}
