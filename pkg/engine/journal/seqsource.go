package journal

// SeqSource is the contract the bus implements so journal handlers can
// retrieve the seq + parent_seq the bus assigned for the current dispatch.
//
// Every EmitEvent / EmitVetoable assigns a seq via an atomic counter and
// pushes (seq, eventID) onto a per-goroutine stack at dispatch entry. While
// a wildcard handler runs (the writer's hook), CurrentSeq returns the top of
// the stack and ParentSeq returns the next entry below — the seq of the
// event whose handler triggered the current emit.
//
// The interface lives in the journal package (not the engine package)
// because the writer does not know the engine type, and the engine wires
// the dependency at construction time.
type SeqSource interface {
	CurrentSeq() uint64
	ParentSeq() uint64
}
