//go:build minio

// Kill and resume, against a real object store.
//
// pkg/engine's TestKillAndResumeRestoresIdenticalSessionState (E1-S5) proves
// the headline behaviour of this whole effort -- a session survives the death
// of the process that produced it and resumes elsewhere with the same state --
// against objectstoretest.Memory. This file is that same cycle with MinIO in
// the middle, and it lives here rather than beside it for one reason: the
// engine is in the root module and the only S3 backend is in this one, so this
// is the only package in the repository that can hold a real *engine.Engine and
// a real S3 wire in the same test.
//
// # What a real store can break that the fake cannot
//
//   - The WAL checkpoint. E1-S4 uploads a per-plugin store.db by running
//     wal_checkpoint(TRUNCATE) and then VACUUM INTO a staging copy. Against the
//     memory backend "upload" is a []byte copied inside the process, so a
//     checkpoint that produced a subtly wrong file would still round-trip
//     byte-for-byte and still open. Here the bytes are streamed out over HTTP
//     with an explicit ContentLength, stored by another process, and streamed
//     back -- and TestKillAndResumeAgainstMinIO pulls the object down with a
//     client that is not the backend under test and opens it as a database, so
//     "what MinIO is actually holding is a valid, queryable SQLite file" is
//     asserted rather than inferred.
//   - Durability outside this process. The memory backend's second engine reads
//     the same Go map the first one wrote to; a backend that never sent
//     anything would pass. Here the second engine is given a different
//     t.TempDir() *and* everything it reads has to have crossed a socket.
//   - Key escaping and latency. Session trees contain nested directories and
//     hundreds of small objects, and every one of them is a signed round trip.
//
// # What is deliberately NOT repeated from E1-S5
//
// The whole-tree byte comparison (TestHydratedTreeMatchesTheKilledTree) is not
// reproduced here. It compares against objectStoreExcluded, an unexported
// engine predicate about which local paths the seam is allowed to sync -- a
// rule that is entirely store-independent, so re-deriving it in this module
// would duplicate engine internals to test something MinIO cannot influence.
// The categories the story names (history, artifacts, blobs, per-plugin SQLite)
// are asserted individually below instead, which is what the acceptance
// criteria ask for and what a real store can actually get wrong.
package s3store_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/blobs"
	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/storage"
	"github.com/frankbardon/nexus/pkg/events"

	s3store "github.com/frankbardon/nexus/modules/objectstore-s3"
)

// resumePluginID is the fake plugin whose session-scope SQLite store stands in
// for real per-plugin state. Session scope specifically: it is the only scope
// that lives inside the session tree, and therefore the only one the session
// snapshot carries. Same ID the engine-side suite uses, so a failure here and a
// failure there name the same thing.
const resumePluginID = "nexus.test.resume"

// ---------------------------------------------------------------------------
// Wiring an engine to MinIO
// ---------------------------------------------------------------------------

// registerMinIOBackend registers a factory under a name unique to the caller
// and returns that name, so an engine can select it from YAML.
//
// A per-call name rather than s3store.BackendName: two engines in one test need
// two independently controllable backends (the mid-flush case arms one and
// leaves the other alone), and the registry is process-global, so sharing the
// real name would make the tests order-dependent.
//
// wrap, when non-nil, gets to interpose on the backend the engine will use.
// That is the entire mechanism for the mid-flush case -- there is no seam
// inside the S3 backend for "die here", and adding one would be a production
// change made to satisfy a test.
func registerMinIOBackend(t *testing.T, target minioTarget, label string, wrap func(objectstore.Backend) objectstore.Backend) string {
	t.Helper()

	// The credentials go in through the environment leg of the SDK's default
	// chain, the one leg an emulator can exercise at all, with isolateAWSEnv
	// first so a developer's populated ~/.aws cannot decide what this
	// authenticates as. Done here rather than inside the factory because
	// t.Setenv must run on the test goroutine, and the factory runs during
	// Boot.
	isolateAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", target.accessKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", target.secretKey)

	name := "minio-" + label + "-" + strings.ReplaceAll(t.Name(), "/", "-")
	objectstore.Register(name, func(ctx context.Context, cfg objectstore.Config) (objectstore.Backend, error) {
		b, err := s3store.New(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if wrap != nil {
			return wrap(b), nil
		}
		return b, nil
	})
	t.Cleanup(func() { objectstore.Unregister(name) })
	return name
}

// minioEngine builds an engine rooted at dir and pointed at a registered
// backend, through the exported bytes constructor.
//
// YAML rather than a hand-built *engine.Config: newFromConfig is unexported, so
// NewFromBytes is the only cross-module way in -- and it is the one the house
// style prefers anyway, because it exercises the same load-and-validate path an
// operator's config takes, including the object-store block's validation.
func minioEngine(t *testing.T, backendName, bucket, endpoint string, policy objectstore.FailurePolicy, dir string) *engine.Engine {
	t.Helper()

	cfg := fmt.Sprintf(`
core:
  log_level: error
  sessions:
    root: %s
  storage:
    root: %s
  object_store:
    backend: %s
    bucket: %s
    region: %s
    endpoint: %s
    failure_policy: %s
`,
		yamlString(filepath.Join(dir, "sessions")),
		yamlString(dir),
		yamlString(backendName),
		yamlString(bucket),
		yamlString(minioRegion),
		yamlString(endpoint),
		yamlString(string(policy)),
	)

	eng, err := engine.NewFromBytes([]byte(cfg))
	if err != nil {
		t.Fatalf("NewFromBytes:\n%s\n%v", cfg, err)
	}
	return eng
}

// yamlString renders a value as a YAML double-quoted scalar. strconv.Quote's
// escaping is a subset of YAML's for the ASCII these values are (temp paths,
// backend names, URLs), and it means a t.TempDir() containing a character YAML
// would otherwise treat as syntax cannot silently produce a different config.
func yamlString(s string) string { return strconv.Quote(s) }

// endTurn emits the turn boundary the engine snapshots on. Bus dispatch is
// synchronous, so the snapshot has completed by the time this returns -- the
// property the whole design rests on, and the one that makes "kill the process"
// below mean anything.
func endTurn(t *testing.T, eng *engine.Engine, turnID string) {
	t.Helper()
	if err := eng.Bus.Emit("agent.turn.end", events.TurnInfo{
		SchemaVersion: events.TurnInfoVersion,
		TurnID:        turnID,
	}); err != nil {
		t.Fatalf("emit agent.turn.end: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Reading the bucket without the backend under test
// ---------------------------------------------------------------------------

// bucketKeys lists every key in the bucket with the raw client. Used to make
// assertions about what MinIO is holding that do not go through the code that
// put it there.
func bucketKeys(t *testing.T, raw *s3.Client, bucket string) []string {
	t.Helper()
	var out []string
	pager := s3.NewListObjectsV2Paginator(raw, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	for pager.HasMorePages() {
		page, err := pager.NextPage(context.Background())
		if err != nil {
			t.Fatalf("listing bucket %q: %v", bucket, err)
		}
		for _, obj := range page.Contents {
			out = append(out, aws.ToString(obj.Key))
		}
	}
	return out
}

// getObject downloads one object with the raw client.
func getObject(t *testing.T, raw *s3.Client, bucket, key string) []byte {
	t.Helper()
	out, err := raw.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("getting %q from bucket %q: %v", key, bucket, err)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("reading %q: %v", key, err)
	}
	return body
}

func objectExists(t *testing.T, raw *s3.Client, bucket, key string) bool {
	t.Helper()
	_, err := raw.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	return err == nil
}

// findKeyWithSuffix returns the single key in the bucket ending in suffix.
//
// The engine's object-key scheme is unexported, and duplicating it here would
// mean this file passes or fails on a copy of a constant rather than on what
// the engine did. Locating a key by the tail it must have -- the session-
// relative path -- keeps that dependency observational.
func findKeyWithSuffix(t *testing.T, raw *s3.Client, bucket, suffix string) string {
	t.Helper()
	var found []string
	for _, k := range bucketKeys(t, raw, bucket) {
		if strings.HasSuffix(k, suffix) {
			found = append(found, k)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one object ending in %q, found %v (bucket holds %v)",
			suffix, found, bucketKeys(t, raw, bucket))
	}
	return found[0]
}

// openSQLiteBytes writes body to a scratch file and opens it as a SQLite
// database with nothing beside it -- no -wal, no -shm -- which is the only way
// a hydrated (or downloaded) database is ever presented. Fails unless
// integrity_check says "ok".
//
// The "sqlite" driver is registered by pkg/engine/storage's blank import of
// modernc.org/sqlite, which this file reaches transitively through
// pkg/engine. Importing it directly here would add a require line to this
// module's go.mod for a driver it already has in its graph.
func openSQLiteBytes(t *testing.T, body []byte, label string) map[string]string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "downloaded.db")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("staging %s: %v", label, err)
	}
	return readSQLiteKV(t, path, label)
}

func readSQLiteKV(t *testing.T, path, label string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening %s: %v", label, err)
	}
	defer db.Close()

	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil {
		t.Fatalf("integrity_check on %s: %v", label, err)
	}
	if check != "ok" {
		t.Fatalf("integrity_check on %s = %q; it is not a valid database", label, check)
	}

	rows, err := db.Query(`SELECT k, v FROM kv`)
	if err != nil {
		t.Fatalf("querying %s: %v", label, err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatalf("scanning a row of %s: %v", label, err)
		}
		out[key] = string(value)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating %s: %v", label, err)
	}
	return out
}

// ---------------------------------------------------------------------------
// The full cycle
// ---------------------------------------------------------------------------

// killedSession is everything the first process produced, captured at the
// moment of its last completed turn, plus the writes it made afterwards that
// the kill must have destroyed.
type killedSession struct {
	id string

	history   []byte
	messages  []events.Message
	artifacts map[string][]byte
	blob      blobs.Handle
	blobBytes []byte
	dbRows    map[string]string

	// The three writes made after the turn boundary. A resumed session that
	// contains any of them would mean the recovery point is not where the
	// design claims.
	lostFile    string
	lostKey     string
	lostBlobSHA string
}

// runToKill boots a session against MinIO, fills it with every category of
// state the story names, completes one turn, writes some more, and then
// abandons the engine mid-flight.
//
// No Stop, no shutdown snapshot, no journal close, no SQLite checkpoint-on-
// close. Every handle it holds is still open when the second engine starts.
// That is the fidelity that matters: it is what forces the turn-boundary
// snapshot to be the only thing that could have saved the session.
func runToKill(t *testing.T, backendName, bucket, endpoint string) *killedSession {
	t.Helper()

	eng := minioEngine(t, backendName, bucket, endpoint, objectstore.FailurePolicyStrict, t.TempDir())

	var snapshots []events.SessionSnapshotResult
	eng.Bus.Subscribe("session.snapshot.result", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.SessionSnapshotResult); ok {
			snapshots = append(snapshots, r)
		}
	})

	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	k := &killedSession{id: eng.Session.ID}

	// context/conversation.jsonl specifically, because that is the file Boot's
	// replay reads on recall -- anything else would assert against a private
	// convention rather than the one the engine actually resumes from.
	k.messages = []events.Message{
		{Role: "user", Content: "summarise the findings"},
		{Role: "assistant", Content: "three findings, all in files/report.md"},
	}
	var buf bytes.Buffer
	for _, m := range k.messages {
		line, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal history message: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	k.history = buf.Bytes()
	if err := eng.Session.WriteFile("context/conversation.jsonl", k.history); err != nil {
		t.Fatalf("write history: %v", err)
	}

	k.artifacts = map[string][]byte{
		"files/report.md":             []byte("# Report\n\nthree findings\n"),
		"files/nested/deep/data.json": []byte(`{"finding":"the seam holds"}`),
		"context/scratch.txt":         []byte("working notes"),
	}
	for rel, body := range k.artifacts {
		if err := eng.Session.WriteFile(rel, body); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	// blobs.New over BlobsDir, matching the engine-side suite: it is the
	// constructor every blob-producing plugin uses, and going through it rather
	// than SessionWorkspace.BlobStore keeps the write-through worker out of the
	// picture so the turn-boundary snapshot is the only thing that can have put
	// this blob in the bucket.
	blobStore, err := blobs.New(eng.Session.BlobsDir(), 0)
	if err != nil {
		t.Fatalf("blobs.New: %v", err)
	}
	k.blobBytes = bytes.Repeat([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, 128)
	k.blob, err = blobStore.Put(k.blobBytes, "image/png")
	if err != nil {
		t.Fatalf("blob put: %v", err)
	}

	// Enough rows to guarantee a WAL that has not been folded back into the
	// main database file -- the state in which an un-checkpointed store.db
	// would be uploaded missing most of its content.
	st, err := eng.Storage.Open(storage.ScopeSession, resumePluginID)
	if err != nil {
		t.Fatalf("open session storage: %v", err)
	}
	k.dbRows = make(map[string]string, 500)
	for i := 0; i < 500; i++ {
		key := fmt.Sprintf("row-%04d", i)
		value := fmt.Sprintf("value-%04d-%s", i, strings.Repeat("x", 200))
		if err := st.Put(key, []byte(value)); err != nil {
			t.Fatalf("storage put: %v", err)
		}
		k.dbRows[key] = value
	}
	walPath := filepath.Join(eng.Session.RootDir, "plugins", resumePluginID, "store.db-wal")
	if _, err := os.Stat(walPath); err != nil {
		t.Fatalf("expected an un-checkpointed WAL beside the session store: %v", err)
	}

	// A real turn: the input that started it, then the boundary. The input
	// matters because Boot's crash-resume path re-fires the io.input of a turn
	// whose end is missing from the journal.
	if err := eng.Bus.Emit("io.input", events.UserInput{
		SchemaVersion: events.UserInputVersion,
		Content:       "summarise the findings",
		SessionID:     k.id,
	}); err != nil {
		t.Fatalf("emit io.input: %v", err)
	}
	if err := eng.Bus.Emit("agent.turn.start", events.TurnInfo{
		SchemaVersion: events.TurnInfoVersion,
		TurnID:        "turn-1",
	}); err != nil {
		t.Fatalf("emit agent.turn.start: %v", err)
	}
	endTurn(t, eng, "turn-1")

	if len(snapshots) != 1 || !snapshots[0].OK {
		t.Fatalf("turn boundary produced snapshots %+v, want exactly one successful one", snapshots)
	}

	k.lostFile = "files/after-the-kill.md"
	if err := eng.Session.WriteFile(k.lostFile, []byte("written after the last completed turn")); err != nil {
		t.Fatalf("write post-turn file: %v", err)
	}
	k.lostKey = "row-after-the-kill"
	if err := st.Put(k.lostKey, []byte("uncommitted work")); err != nil {
		t.Fatalf("post-turn storage put: %v", err)
	}
	lost, err := blobStore.Put([]byte("blob written after the last completed turn"), "text/plain")
	if err != nil {
		t.Fatalf("post-turn blob put: %v", err)
	}
	k.lostBlobSHA = lost.SHA256

	// *** kill *** -- the engine is dropped on the floor from here.
	//
	// Abandon is what a kill actually is: every background worker stops where
	// it stands and nothing is persisted on the way out. No shutdown snapshot,
	// no flush, no journal close, no SQLite checkpoint, and the owner marker
	// stays in the bucket carrying a heartbeat that has stopped advancing --
	// which is exactly the evidence a host that died would have left. It also
	// removes the one way this test could lie about a real process death:
	// against MinIO an engine left running keeps a heartbeat and (under
	// degrade) a retry worker writing objects into the bucket that
	// emptyAndRemoveBucket is trying to empty, and DeleteBucket then fails
	// intermittently with BucketNotEmpty.
	eng.Abandon()
	return k
}

// TestKillAndResumeAgainstMinIORestoresIdenticalSessionState is the story: run
// a session against a real S3 implementation in another process, complete a
// turn, kill the process, boot a fresh engine over a clean filesystem, and get
// the same session back -- history, artifacts, blobs and per-plugin SQLite
// alike.
func TestKillAndResumeAgainstMinIORestoresIdenticalSessionState(t *testing.T) {
	target := requireMinIO(t)
	raw := newRawClient(t, target)
	bucket := newBucket(t, raw)
	backendName := registerMinIOBackend(t, target, "resume", nil)

	k := runToKill(t, backendName, bucket, target.endpoint)

	// A brand-new data root: no session tree, no lock, no databases, no blobs.
	// Everything the resumed engine reads has to come back over the wire.
	freshRoot := t.TempDir()
	eng := minioEngine(t, backendName, bucket, target.endpoint, objectstore.FailurePolicyStrict, freshRoot)
	eng.RecallSessionID = k.id

	// Subscribed before Boot, because the replay fires during it.
	var replayed []events.Message
	var replays int
	eng.Bus.Subscribe("io.history.replay", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.HistoryReplay); ok {
			replays++
			replayed = r.Messages
		}
	})
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot resuming %q: %v", k.id, err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	if eng.Session == nil || eng.Session.ID != k.id {
		t.Fatalf("resumed session = %+v, want ID %q", eng.Session, k.id)
	}
	if !strings.HasPrefix(eng.Session.RootDir, freshRoot) {
		t.Fatalf("resumed session lives at %q, outside the fresh root %q; the test proves nothing",
			eng.Session.RootDir, freshRoot)
	}

	// --- history -------------------------------------------------------
	got, err := eng.Session.ReadFile("context/conversation.jsonl")
	if err != nil {
		t.Fatalf("reading restored history: %v", err)
	}
	if !bytes.Equal(got, k.history) {
		t.Errorf("restored conversation.jsonl =\n%s\nwant\n%s", got, k.history)
	}
	if replays != 1 {
		t.Errorf("io.history.replay fired %d times on resume, want 1", replays)
	}
	if len(replayed) != len(k.messages) {
		t.Fatalf("replayed %d messages, want %d", len(replayed), len(k.messages))
	}
	for i, want := range k.messages {
		if replayed[i].Role != want.Role || replayed[i].Content != want.Content {
			t.Errorf("replayed message %d = %+v, want %+v", i, replayed[i], want)
		}
	}

	// --- artifacts -----------------------------------------------------
	for rel, want := range k.artifacts {
		got, err := eng.Session.ReadFile(rel)
		if err != nil {
			t.Errorf("reading restored %s: %v", rel, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("restored %s = %q, want %q", rel, got, want)
		}
	}

	// --- blobs ---------------------------------------------------------
	blobStore, err := blobs.New(eng.Session.BlobsDir(), 0)
	if err != nil {
		t.Fatalf("blobs.New on the restored session: %v", err)
	}
	gotBlob, gotMedia, err := blobStore.Get(k.blob.SHA256)
	if err != nil {
		t.Fatalf("restored blob %s: %v", k.blob.SHA256, err)
	}
	if !bytes.Equal(gotBlob, k.blobBytes) {
		t.Errorf("restored blob differs: %d bytes, want %d", len(gotBlob), len(k.blobBytes))
	}
	if gotMedia != "image/png" {
		t.Errorf("restored blob media type = %q, want %q", gotMedia, "image/png")
	}

	// --- per-plugin SQLite, through the production path ------------------
	//
	// Opened through the engine's own storage manager rather than
	// database/sql, so this asserts the restored file is usable by the code
	// that will actually use it -- WAL re-enabled, sidecars recreated
	// locally -- not merely readable by a test.
	st, err := eng.Storage.Open(storage.ScopeSession, resumePluginID)
	if err != nil {
		t.Fatalf("opening restored session storage: %v", err)
	}
	keys, err := st.List("")
	if err != nil {
		t.Fatalf("listing restored storage: %v", err)
	}
	if len(keys) != len(k.dbRows) {
		t.Errorf("restored store holds %d rows, want %d", len(keys), len(k.dbRows))
	}
	for key, want := range k.dbRows {
		got, ok, err := st.Get(key)
		if err != nil {
			t.Fatalf("restored storage get %s: %v", key, err)
		}
		if !ok {
			t.Errorf("restored store is missing %q", key)
			continue
		}
		if string(got) != want {
			t.Errorf("restored store[%q] = %q, want %q", key, got, want)
		}
	}
	var check string
	if err := st.DB().QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil {
		t.Fatalf("integrity_check through the storage manager: %v", err)
	}
	if check != "ok" {
		t.Errorf("integrity_check = %q on the resumed store", check)
	}

	// --- per-plugin SQLite, as MinIO is holding it -----------------------
	//
	// The acceptance criterion in its strongest form. Everything above could
	// in principle be satisfied by a local file the resumed engine repaired on
	// open; this pulls the object straight out of the bucket with a client
	// that is not the backend under test and asserts THOSE bytes are a valid,
	// queryable database holding all 500 rows. That is what proves E1-S4's
	// wal_checkpoint(TRUNCATE) + VACUUM INTO survived a real upload rather
	// than an in-process handoff -- an uncheckpointed upload lands a main
	// database file that opens fine and is missing most of its content, which
	// is exactly the shape this catches.
	dbKey := findKeyWithSuffix(t, raw, bucket, "/plugins/"+resumePluginID+"/store.db")
	remoteRows := openSQLiteBytes(t, getObject(t, raw, bucket, dbKey), "the store.db object in MinIO")
	if len(remoteRows) != len(k.dbRows) {
		t.Errorf("the store.db object in MinIO holds %d rows, want %d — the WAL checkpoint did not survive the upload",
			len(remoteRows), len(k.dbRows))
	}
	for key, want := range k.dbRows {
		if remoteRows[key] != want {
			t.Errorf("the store.db object in MinIO has row %q = %q, want %q", key, remoteRows[key], want)
		}
	}

	// --- metadata ------------------------------------------------------
	meta, err := eng.Session.SessionMetadata()
	if err != nil {
		t.Fatalf("SessionMetadata: %v", err)
	}
	if meta.ID != k.id {
		t.Errorf("restored metadata ID = %q, want %q", meta.ID, k.id)
	}
	if meta.TurnCount != 1 {
		t.Errorf("restored TurnCount = %d, want 1 — the resumed session forgot the turn it completed", meta.TurnCount)
	}

	// --- the recovery point --------------------------------------------
	//
	// Everything written after the last turn boundary must be gone. If any of
	// it survived, something other than the turn-boundary snapshot persisted
	// the session and the "at most the in-flight turn" guarantee is not the
	// one being tested.
	if eng.Session.FileExists(k.lostFile) {
		t.Errorf("%s survived the kill; it was written after the last completed turn", k.lostFile)
	}
	if _, ok, _ := st.Get(k.lostKey); ok {
		t.Errorf("storage key %q survived the kill; it was written after the last completed turn", k.lostKey)
	}
	if _, _, err := blobStore.Get(k.lostBlobSHA); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("blob written after the last completed turn survived the kill (err = %v)", err)
	}
}

// ---------------------------------------------------------------------------
// The mid-flush kill
// ---------------------------------------------------------------------------

// interruptedBackend is a Backend that can be made to die partway through a
// snapshot, and that counts deletes.
//
// The interruption is expressed as "refuse every Put outside the session tree
// prefix", which lands exactly where a mid-flush process death lands: the tree
// objects of the dead generation are in the bucket -- Put against S3 is
// synchronous, so they are durable the moment it returns -- and the manifest
// and the commit marker, both siblings OUTSIDE that prefix, never got written.
// Nothing has to know their key names for that to hold, which is what keeps
// this test from being a copy of the engine's unexported key scheme.
type interruptedBackend struct {
	objectstore.Backend

	mu sync.Mutex
	// blockOutside, when non-empty, is the key prefix whose Puts still
	// succeed. Everything else fails.
	blockOutside string
	deletes      int
}

func (b *interruptedBackend) arm(treePrefix string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.blockOutside = treePrefix
}

func (b *interruptedBackend) deleteCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deletes
}

func (b *interruptedBackend) Put(ctx context.Context, key, localPath string) error {
	b.mu.Lock()
	blocked := b.blockOutside != "" && !strings.HasPrefix(key, b.blockOutside)
	b.mu.Unlock()
	if blocked {
		return fmt.Errorf("simulated process death before %q could be committed", key)
	}
	return b.Backend.Put(ctx, key, localPath)
}

func (b *interruptedBackend) Delete(ctx context.Context, key string) error {
	b.mu.Lock()
	b.deletes++
	b.mu.Unlock()
	return b.Backend.Delete(ctx, key)
}

// TestMidFlushKillAgainstMinIORestoresTheCommittedGeneration is the mid-flush
// criterion, asserted at the boundary E3-S5 actually recorded rather than the
// stronger one the story text reads as.
//
// # The boundary
//
// A snapshot overwrites in place and the manifest names PATHS, not versions.
// So an interrupted snapshot that got as far as re-uploading a mutable object
// -- context/conversation.jsonl, the active journal segment, a per-plugin
// store.db -- has already replaced the committed generation's bytes at that
// key, and no listing of paths can bring them back. What survives a mid-flush
// kill is therefore:
//
//   - Hydration restores exactly the committed generation's object SET.
//     A key the dead generation added and the committed manifest does not name
//     is not materialised.
//   - Orphaned objects are left in the bucket, not deleted. This effort never
//     removes remote data; reclamation is the operator's.
//
// and what does NOT survive it, asserted here rather than hoped for:
//
//   - An object the dead generation overwrote in place carries the dead
//     generation's bytes. The restored store.db reads generation 2.
//
// pkg/engine's TestInterruptedSnapshotCanOverwriteACommittedObjectInPlace pins
// the same thing against the memory backend; this is that assertion with a real
// wire, a real bucket and a real SQLite file in between, which is where an
// argument that "S3 would somehow keep the old bytes" would be exposed. The fix
// would be per-generation object keys -- a second full copy of every session in
// the bucket for ever -- which E1-S4 costed and rejected.
func TestMidFlushKillAgainstMinIORestoresTheCommittedGeneration(t *testing.T) {
	target := requireMinIO(t)
	raw := newRawClient(t, target)
	bucket := newBucket(t, raw)

	// Two registrations over one bucket. The dying engine's backend stays
	// armed for ever, so nothing it left running -- a retry worker, a
	// heartbeat -- can quietly commit generation 2 behind the resume; the
	// resuming engine gets a clean backend of its own.
	var dying *interruptedBackend
	dyingName := registerMinIOBackend(t, target, "dying", func(b objectstore.Backend) objectstore.Backend {
		dying = &interruptedBackend{Backend: b}
		return dying
	})
	var resuming *interruptedBackend
	resumingName := registerMinIOBackend(t, target, "resuming", func(b objectstore.Backend) objectstore.Backend {
		resuming = &interruptedBackend{Backend: b}
		return resuming
	})

	// Degrade, not strict: strict would raise core.error and close the turn
	// gate, and this test is about what the bucket is left holding.
	eng := minioEngine(t, dyingName, bucket, target.endpoint, objectstore.FailurePolicyDegrade, t.TempDir())
	if err := eng.Boot(context.Background()); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	sessionID := eng.Session.ID

	// --- generation 1, committed ----------------------------------------
	if err := eng.Session.WriteFile("files/generation-1.md", []byte("committed")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	st, err := eng.Storage.Open(storage.ScopeSession, resumePluginID)
	if err != nil {
		t.Fatalf("open session storage: %v", err)
	}
	if err := st.Put("generation", []byte("1")); err != nil {
		t.Fatalf("storage put: %v", err)
	}
	var results []events.SessionSnapshotResult
	eng.Bus.Subscribe("session.snapshot.result", func(ev engine.Event[any]) {
		if r, ok := ev.Payload.(events.SessionSnapshotResult); ok {
			results = append(results, r)
		}
	})
	endTurn(t, eng, "turn-1")
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("the first turn produced snapshots %+v, want exactly one successful one", results)
	}

	// The session's key prefix, learned from the bucket rather than rebuilt
	// from the engine's unexported scheme.
	gen1Key := findKeyWithSuffix(t, raw, bucket, "/files/generation-1.md")
	treePrefix := strings.TrimSuffix(gen1Key, "files/generation-1.md")
	dbKey := findKeyWithSuffix(t, raw, bucket, "/plugins/"+resumePluginID+"/store.db")
	if !strings.HasPrefix(dbKey, treePrefix) {
		t.Fatalf("store.db at %q is not under the session prefix %q; the prefix was derived wrongly", dbKey, treePrefix)
	}

	// --- the kill, partway through generation 2 --------------------------
	dying.arm(treePrefix)
	if err := eng.Session.WriteFile("files/generation-2.md", []byte("never committed")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := st.Put("generation", []byte("2")); err != nil {
		t.Fatalf("storage put: %v", err)
	}
	results = nil
	endTurn(t, eng, "turn-2")
	failed := false
	for _, r := range results {
		if !r.OK {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("the second turn's snapshot reported %+v; the interruption was not simulated", results)
	}

	// *** kill *** -- everything below reads the bucket, never this engine.
	// Abandon stops the retry worker and the heartbeat without persisting
	// anything, which is what a dead process would have done: nothing it left
	// running can quietly commit generation 2 behind the assertions, and
	// nothing is still uploading into the bucket the deferred
	// emptyAndRemoveBucket has to empty. The backend stays armed for the same
	// reason it always did -- a second line of defence costs nothing.
	eng.Abandon()

	// The state the kill left in MinIO, measured with the raw client: the tree
	// of the dead generation is there, its commit is not. If either half of
	// this is wrong the assertions below are not testing a mid-flush kill.
	orphanKey := treePrefix + "files/generation-2.md"
	if !objectExists(t, raw, bucket, orphanKey) {
		t.Fatalf("%q never reached the bucket; the interruption happened before the tree upload, "+
			"not partway through it", orphanKey)
	}
	if remote := openSQLiteBytes(t, getObject(t, raw, bucket, dbKey), "the store.db object in MinIO"); remote["generation"] != "2" {
		t.Fatalf("the store.db object in MinIO reads generation %q, want %q — "+
			"the interrupted snapshot did not overwrite it in place, so the boundary below is not being exercised",
			remote["generation"], "2")
	}

	// --- a fresh host picks the session up -------------------------------
	freshRoot := t.TempDir()
	resumed := minioEngine(t, resumingName, bucket, target.endpoint, objectstore.FailurePolicyDegrade, freshRoot)
	resumed.RecallSessionID = sessionID
	if err := resumed.Boot(context.Background()); err != nil {
		t.Fatalf("Boot resuming %q: %v", sessionID, err)
	}
	t.Cleanup(func() { _ = resumed.Stop(context.Background()) })
	if resumed.Session == nil || resumed.Session.ID != sessionID {
		t.Fatalf("resumed session = %+v, want ID %q", resumed.Session, sessionID)
	}

	// What the manifest DOES fix: the dead generation added a key the
	// committed set does not name, and it is not materialised.
	if resumed.Session.FileExists("files/generation-2.md") {
		t.Error("an artifact from the interrupted snapshot was restored; " +
			"hydration must restore exactly the committed generation's object set")
	}
	body, err := resumed.Session.ReadFile("files/generation-1.md")
	if err != nil {
		t.Errorf("the committed generation's artifact is missing: %v", err)
	} else if string(body) != "committed" {
		t.Errorf("restored files/generation-1.md = %q, want %q", body, "committed")
	}

	// What it does NOT fix. store.db is a key generation 1 names, so it is
	// restored -- carrying the bytes generation 2 overwrote it with. A future
	// change that makes this read "1" is an improvement, and should update this
	// test, its doc comment, and
	// TestInterruptedSnapshotCanOverwriteACommittedObjectInPlace with it.
	restoredDB := filepath.Join(resumed.Session.RootDir, "plugins", resumePluginID, "store.db")
	if rows := readSQLiteKV(t, restoredDB, "the restored store.db"); rows["generation"] != "2" {
		t.Errorf("restored store.db says generation %q, want %q — "+
			"this test pins the in-place overwrite boundary, not a guarantee", rows["generation"], "2")
	}

	// Orphans are left in the bucket. This effort never deletes remote data.
	if !objectExists(t, raw, bucket, orphanKey) {
		t.Errorf("hydration deleted the orphaned object %q from the bucket", orphanKey)
	}
	if resuming == nil {
		t.Fatal("the resuming engine never opened its backend; the delete assertion below would be vacuous")
	}
	if got := resuming.deleteCount(); got != 0 {
		t.Errorf("the resuming engine issued %d deletes; hydration must never delete remote data", got)
	}
}
