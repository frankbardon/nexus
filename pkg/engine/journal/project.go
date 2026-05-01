package journal

// ProjectFile walks an existing journal directory and fires the handler
// for every envelope whose type is in the filter set. Pass an empty types
// slice to receive every envelope.
//
// Use case: regenerate a derived file (e.g. plugins/<id>/thinking.jsonl)
// from the journal alone after the file was deleted, or rebuild a
// projection plugins did not exist when the original session ran. Pairs
// with Writer.SubscribeProjection — same handler signature, same filter
// semantics — so the same code path drives both live and post-mortem
// projection.
//
// Errors from Open or Iter are returned to the caller; per-envelope handler
// panics propagate (this is a tooling helper, not a production hot path —
// callers can recover at the boundary if they want to keep going).
func ProjectFile(dir string, types []string, handler func(Envelope)) error {
	r, err := Open(dir)
	if err != nil {
		return err
	}

	typeSet := make(map[string]bool, len(types))
	for _, t := range types {
		typeSet[t] = true
	}

	return r.Iter(func(e Envelope) bool {
		if len(typeSet) == 0 || typeSet[e.Type] {
			handler(e)
		}
		return true
	})
}
