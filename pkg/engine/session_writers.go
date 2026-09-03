package engine

// This file is the answer to one question: for every writer that puts bytes
// under a session tree without going through the SessionWorkspace helpers,
// what does the object-store seam do with those bytes?
//
// The question needs a written answer because "it emits nothing" and "it is
// deliberately not synced in real time" look identical from the outside, and
// the difference between them is the difference between a bug and a design
// decision. Before this table existed, twelve writers were in that ambiguous
// state — including plugins/scene, which emits eight event types, none of them
// about the files it writes, and therefore looks instrumented while being
// entirely invisible to the sync layer.
//
// The set is closed rather than open-ended. Every plugin-level writer here
// obtains its directory from SessionWorkspace.PluginDir or from a config key,
// and the engine-level ones are named subsystems — so this is twelve sites,
// not "every plugin", and it can be enumerated and kept honest by a test.
// Thirteen rows for twelve writers: the journal is one writer spread over
// writer.go and rotate.go, and Source is the key an enforcement test matches a
// raw os.* call against, so each file needs its own row.
//
// A registry in Go rather than a docs table alone: docs cannot be consumed by
// the enforcement test that stops a *new* raw writer from appearing without a
// decision, and a table that only lives in prose is a table that goes stale.
// docs/src/architecture/sessions.md carries the same content for humans.

// SessionWriterDisposition names what the object-store seam does with the
// bytes one writer puts under a session tree.
type SessionWriterDisposition string

const (
	// DispositionEmit means the writer announces every write on the bus via
	// SessionWorkspace.AnnounceWrite / AnnounceAppend (or the WriteFile /
	// AppendFile helpers), so a sync backend can push it the moment it lands
	// and does not have to wait for a turn to end.
	DispositionEmit SessionWriterDisposition = "emit"

	// DispositionTurnBoundary means the writer is silent on the bus by
	// decision, and its bytes reach the store only through the whole-tree
	// snapshot taken at agent.turn.end (and at shutdown). This is a real
	// disposition, not an absence of one: the files still sync, just not in
	// real time.
	DispositionTurnBoundary SessionWriterDisposition = "turn-boundary-only"

	// DispositionExcluded means the bytes must never leave the machine at
	// all — not on the bus, not in the snapshot. Reserved for files that
	// describe the *host* rather than the session, where a faithful copy is
	// precisely the wrong thing to restore.
	DispositionExcluded SessionWriterDisposition = "excluded-by-design"
)

// SessionTreeWriter records one writer's disposition.
type SessionTreeWriter struct {
	// Source is the repo-relative file that owns the raw os.* call. It is
	// the allowlist key: a raw write anywhere else is an undecided writer.
	Source string
	// Writes describes what lands where, in session-relative terms when the
	// writer is inside the tree.
	Writes string
	// Disposition is the decision.
	Disposition SessionWriterDisposition
	// Why records the reasoning, including the rejected alternative where
	// there was a real choice to make.
	Why string
}

// SessionTreeWriters returns the closed set of writers that bypass the
// SessionWorkspace helpers, each with a decided disposition.
//
// Exported so the enforcement test can consume it as an allowlist rather than
// keeping a second copy of the same list that is free to drift from this one.
func SessionTreeWriters() []SessionTreeWriter {
	return []SessionTreeWriter{
		{
			Source:      "plugins/scene/plugin.go",
			Writes:      "plugins/nexus.scene/scenes.json + scenes.jsonl patch journal",
			Disposition: DispositionEmit,
			Why: "Inside the session tree and the highest-churn raw writer in it: " +
				"the JSONL patch journal is appended to on every scene mutation and is " +
				"the durable source of truth the replay primitive reconstructs scene " +
				"state from, so a session killed mid-turn loses exactly the scenes it " +
				"just built. Both writes happen well after plugin Init, on tool.invoke " +
				"and at Shutdown, so neither can race the journal writer's subscription.",
		},
		{
			Source:      "plugins/llm/batch/state.go",
			Writes:      "one JSON state file per in-flight batch, under batch.data_dir",
			Disposition: DispositionTurnBoundary,
			Why: "data_dir defaults to ~/.nexus/batches — machine scope, outside every " +
				"session tree, so in the default configuration there is nothing under a " +
				"session for a real-time push to carry. Pointing data_dir inside a " +
				"session is legal and the whole-tree snapshot then covers it. Not worth " +
				"an emission either way: the coordinator resumes batches by scanning " +
				"that directory at boot, so its durability requirement is a local disk " +
				"that survives a restart, not a remote copy.",
		},
		{
			Source:      "plugins/memory/longterm/storage.go",
			Writes:      "one markdown file per memory key, under the configured base path",
			Disposition: DispositionTurnBoundary,
			Why: "Defaults to ~/.nexus/memory (or ~/.nexus/agents/<id>/memory) and is " +
				"cross-session by definition — the entire point of the plugin is that " +
				"notes outlive the session that wrote them, so its files are deliberately " +
				"not under one. Durable placement for agent- and app-scope roots is a " +
				"separate problem from session sync and is tracked as its own work.",
		},
		{
			Source:      "plugins/rag/ingest/cache.go",
			Writes:      "embedding cache entries under rag.ingest cache_dir",
			Disposition: DispositionTurnBoundary,
			Why: "Defaults to ~/.nexus/vectors/_cache, outside every session tree. Even " +
				"in-tree it would not earn an emission: it is a cache of embeddings " +
				"derivable from the source documents, so pushing it spends bandwidth on " +
				"bytes a resume can regenerate, and a missing cache costs latency rather " +
				"than correctness.",
		},
		{
			Source:      "plugins/control/hitl/registry.go",
			Writes:      "request/response JSON files under the hitl registry dir",
			Disposition: DispositionTurnBoundary,
			Why: "Defaults to ~/.nexus/hitl and is a filesystem IPC rendezvous, not " +
				"session state: an external responder drops a file in, the registry " +
				"consumes it and deletes both halves within the turn. Restoring an " +
				"in-flight pair onto another host would re-ask a question that was " +
				"already answered — the same class of mistake as restoring session.lock.",
		},
		{
			Source:      "plugins/observe/sampler/plugin.go",
			Writes:      "sampled journal segments + metadata.json under sampler out_dir",
			Disposition: DispositionTurnBoundary,
			Why: "Defaults to ~/.nexus/eval/samples: an eval corpus deliberately " +
				"accumulated across sessions and outside all of them, so sampled cases " +
				"survive session cleanup. It is also a copy of journal bytes that are " +
				"themselves snapshotted, so a real-time push would upload the same " +
				"events twice.",
		},
		{
			Source:      "plugins/workflows/icm/session/session.go",
			Writes:      "plugins/nexus.workflows.icm/<runID>/ stage artifacts and sidecars",
			Disposition: DispositionEmit,
			Why: "Inside the session tree, and the artifacts are the run's actual work " +
				"product — a multi-stage ICM run is long enough that waiting for a turn " +
				"boundary means a crash discards completed stages. Every write funnels " +
				"through Session.WriteArtifact and the two input-copy loops, so the " +
				"announcement has three call sites, not one per artifact kind.",
		},
		{
			Source:      "pkg/engine/journal/writer.go",
			Writes:      "journal/events.jsonl (active segment)",
			Disposition: DispositionTurnBoundary,
			Why: "Emitting here is a self-feeding loop, not merely noisy: the writer's " +
				"input is every event on the bus, so an announcement per journal write " +
				"produces an envelope that produces another announcement. The writer " +
				"holds no bus reference at all, which is what makes that impossible by " +
				"construction rather than by discipline. The snapshot captures the " +
				"journal through journal.Writer.Snapshot, which takes a consistent " +
				"instant across the active and rotated segments.",
		},
		{
			Source:      "pkg/engine/journal/rotate.go",
			Writes:      "journal/events-NNN.jsonl.zst (sealed segments)",
			Disposition: DispositionTurnBoundary,
			Why: "Same loop argument as the writer it is part of. Rotated segments are " +
				"additionally immutable once sealed, so objectStoreImmutable makes each " +
				"one a once-ever upload and the turn-boundary cost does not grow with " +
				"session length.",
		},
		{
			Source:      "pkg/engine/toolcache.go",
			Writes:      "journal/cache/<tool>/<argshash>.json",
			Disposition: DispositionTurnBoundary,
			Why: "The cache is a replay companion to journal/events.jsonl and is useless " +
				"without it, so streaming one while the other waits for the turn " +
				"boundary would push half of an artefact pair. It also writes from " +
				"inside a tool.result handler, where an announcement per tool call would " +
				"roughly double bus traffic on the hottest path in a session to advance " +
				"an upload the journal is not ready for anyway.",
		},
		{
			Source:      "pkg/engine/blobs/blobs.go",
			Writes:      "blobs/<xx>/<sha256>.bin and .meta",
			Disposition: DispositionTurnBoundary,
			Why: "Content-addressed and immutable once written, so objectStoreImmutable " +
				"already reduces the whole subtree to a once-ever upload per blob — the " +
				"repeated cost a real-time push would remove is not being paid. The store " +
				"is also a standalone package with no bus dependency, deliberately, so it " +
				"can be used outside an engine; wiring a bus into it to save a delay that " +
				"lasts until the end of the current turn is a bad trade.",
		},
		{
			Source:      "pkg/engine/storage/sqlite.go",
			Writes:      "plugins/<pluginID>/store.db (+ -wal, -shm sidecars)",
			Disposition: DispositionExcluded,
			Why: "Excluded from real-time sync by design. A live handle is a WAL " +
				"database: committed frames sit in store.db-wal until a checkpoint folds " +
				"them in, so streaming partial writes uploads a database that is corrupt " +
				"or silently stale rather than merely behind. It reaches the store only " +
				"as a checkpointed VACUUM INTO snapshot at the turn boundary (see " +
				"pkg/engine/storage/snapshot.go), and the -wal/-shm/-journal sidecars are " +
				"rejected outright by objectStoreExcluded.",
		},
		{
			Source:      "pkg/engine/session_lock.go",
			Writes:      "session.lock",
			Disposition: DispositionExcluded,
			Why: "Never uploaded. The file carries the local PID of the process that " +
				"owns the session, and Boot refuses to start against a lock whose PID is " +
				"still alive. Round-tripping it stamps one host's PID onto every later " +
				"resume: on a fresh container that PID is usually free so the lock reads " +
				"as stale and is overwritten — correct by coincidence, and wrong the " +
				"moment the number happens to be in use. objectStoreExcluded rejects it " +
				"on both the hydrate and the upload path.",
		},
	}
}
