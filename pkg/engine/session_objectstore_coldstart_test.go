package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/frankbardon/nexus/pkg/engine/objectstore"
	"github.com/frankbardon/nexus/pkg/engine/objectstore/objectstoretest"
)

// The cold-start budget.
//
// Hydration is eager and whole-tree, so cold start grows with session size —
// that is a documented, accepted property. What was NOT true is that anything
// would notice it getting worse: nothing in the suite failed when hydration did
// more work, and the guide had to say so.
//
// This asserts *work*, not wall-clock. A millisecond budget on a shared CI
// runner is either flaky or so loose it catches nothing, and it would fail for
// reasons that have nothing to do with this code. Backend call counts and bytes
// moved are exactly reproducible and are what actually turns into latency and
// egress cost against a real store.
//
// The property being pinned: resuming a session costs a fixed number of backend
// round trips no matter how large the session is. Eager whole-tree hydration is
// what buys that, and it is the thing a well-meaning refactor toward per-file
// fetching would destroy — turning one request into thousands, which is
// catastrophic against a store with real per-request latency and invisible
// against a local double.
//
// Two calls, not one: the session tree, then the committed-object manifest.
// The second is the one a reader is most likely to assume is free — it is a
// whole extra round trip, and it happens whether or not a manifest is there.
const hydrateCallsPerSession = 2

// seedSessionInStore builds a session tree of n files, puts it in the backend
// under the session's key prefix, and leaves no local copy — the state a
// replaced host wakes up in.
//
// Seeded directly rather than by running a real snapshot, so the fixture is
// exactly the tree named here and nothing else. That leaves no committed-object
// manifest, which is the documented "restore every object" path and does not
// change what is being measured: the manifest read is one Hydrate call whether
// or not an object is there to read, so the round-trip count is the same on
// both paths. TestHydrationRestoresOnlyTheCommittedGeneration covers the
// manifest path's correctness.
func seedSessionInStore(t *testing.T, sessionID string, n int) (*Engine, *objectstoretest.Memory, string) {
	t.Helper()

	eng, backend := newMemoryObjectStoreEngine(t, objectstore.FailurePolicyDegrade)
	if err := eng.openObjectStore(context.Background()); err != nil {
		t.Fatalf("openObjectStore: %v", err)
	}
	seedSessionTree(t, backend, sessionID, n)
	return eng, backend, sessionDestDir(eng, sessionID)
}

// seedSessionTree puts one session's tree into an existing backend. Separate
// from seedSessionInStore so a test can put two sessions in the *same* bucket,
// which is the only way to prove hydration is prefix-scoped -- and which
// calling seedSessionInStore twice cannot do, since each call stands up its own
// engine and its own store.
func seedSessionTree(t *testing.T, backend *objectstoretest.Memory, sessionID string, n int) {
	t.Helper()

	staging := t.TempDir()
	extra := make(map[string]string, n)
	for i := 0; i < n; i++ {
		// Distinct sizes so a check that accidentally measures file count
		// rather than bytes still moves when n does.
		extra[fmt.Sprintf("files/note-%03d.md", i)] = strings.Repeat("x", 64+i)
	}
	writeSessionTree(t, staging, sessionID, extra)

	if err := backend.SeedTree(sessionObjectKeyPrefix(sessionID), staging); err != nil {
		t.Fatalf("seeding %s: %v", sessionID, err)
	}
}

func sessionDestDir(eng *Engine, sessionID string) string {
	return filepath.Join(ExpandPath(eng.Config.Core.Sessions.Root), sessionID)
}

// Backend round trips must not scale with session size. This is the assertion
// that would catch eager whole-tree hydration being replaced by anything
// per-object.
func TestColdStartCostDoesNotScaleWithSessionSize(t *testing.T) {
	sizes := []int{4, 64}
	counts := make([]objectstoretest.Counts, 0, len(sizes))

	for _, n := range sizes {
		t.Run(fmt.Sprintf("%dfiles", n), func(t *testing.T) {
			sessionID := fmt.Sprintf("sess-cold-%d", n)
			eng, backend, dest := seedSessionInStore(t, sessionID, n)

			before := backend.Counts()
			if err := eng.hydrateSessionTree(context.Background(), sessionID); err != nil {
				t.Fatalf("hydrateSessionTree: %v", err)
			}
			after := backend.Counts()

			got := objectstoretest.Counts{
				Hydrates: after.Hydrates - before.Hydrates,
				Puts:     after.Puts - before.Puts,
				Deletes:  after.Deletes - before.Deletes,
				Lists:    after.Lists - before.Lists,
			}
			counts = append(counts, got)

			if got.Hydrates != hydrateCallsPerSession {
				t.Errorf("Hydrate calls = %d, want exactly %d (the tree, then the manifest). "+
					"A count that tracks file count means hydration went per-object, which is "+
					"thousands of round trips against a real store and free against this double",
					got.Hydrates, hydrateCallsPerSession)
			}
			if got.Lists != 0 {
				t.Errorf("List calls = %d, want 0; hydration knows its own key prefix and "+
					"must not enumerate the bucket to find it", got.Lists)
			}
			if got.Puts != 0 || got.Deletes != 0 {
				t.Errorf("hydration wrote to the store (puts=%d deletes=%d); resuming must be "+
					"read-only, or a crashed host's leftovers get mutated by the host replacing it",
					got.Puts, got.Deletes)
			}

			// The work still has to have happened — a hydration that fetched
			// nothing would pass every count assertion above.
			for i := 0; i < n; i++ {
				name := fmt.Sprintf("files/note-%03d.md", i)
				if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
					t.Fatalf("%s missing after hydration: %v", name, err)
				}
			}
		})
	}

	if len(counts) == len(sizes) && counts[0] != counts[1] {
		t.Errorf("cold-start traffic changed with session size: %d files cost %+v, %d files cost %+v. "+
			"Eager whole-tree hydration is what makes this constant; if that changed deliberately, "+
			"the cost is now O(session) round trips and docs/src/guides/object-storage.md "+
			"limitation 5 needs rewriting, not this test relaxing",
			sizes[0], counts[0], sizes[1], counts[1])
	}
}

// A warm tree must cost nothing at all. This is the cheapest cold start there
// is and the one most likely to regress silently, because re-hydrating over a
// live tree still produces a correct-looking result — it just resurrects
// objects the running host deleted and costs a full download every boot.
func TestWarmStartCostsNoBackendTraffic(t *testing.T) {
	const sessionID = "sess-warm"
	eng, backend, dest := seedSessionInStore(t, sessionID, 8)

	// Put the tree back: this is a host restarting with its disk intact.
	if err := eng.hydrateSessionTree(context.Background(), sessionID); err != nil {
		t.Fatalf("first hydrate: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("tree missing after the first hydrate: %v", err)
	}

	before := backend.Counts()
	if err := eng.hydrateSessionTree(context.Background(), sessionID); err != nil {
		t.Fatalf("second hydrate: %v", err)
	}
	after := backend.Counts()

	if after != before {
		t.Errorf("a warm start cost backend traffic: before=%+v after=%+v. "+
			"The local tree is the live working copy and must win outright", before, after)
	}
}

// Hydration must be scoped to this session's key prefix. A single Hydrate that
// pulled the whole bucket would cost the same number of round trips as a
// correct one, so the call-count assertions above cannot catch it -- only the
// bytes that land on disk can.
//
// Both sessions live in one bucket here, which is the point: this is the shape
// a real deployment has, and the failure it guards against is a neighbour's
// session being dragged down with yours.
func TestColdStartMovesOnlyThisSessionsBytes(t *testing.T) {
	const (
		mine    = "sess-mine"
		theirs  = "sess-theirs"
		mineN   = 6
		theirsN = 40
	)

	eng, backend, dest := seedSessionInStore(t, mine, mineN)
	seedSessionTree(t, backend, theirs, theirsN)

	// One object far larger than anything in the small session, so a
	// prefix-blind hydration is obvious in the file list rather than only in a
	// count.
	if err := backend.Seed(sessionObjectKeyPrefix(theirs)+"files/big.md",
		[]byte(strings.Repeat("y", 4096))); err != nil {
		t.Fatalf("seeding the neighbour's large object: %v", err)
	}

	if err := eng.hydrateSessionTree(context.Background(), mine); err != nil {
		t.Fatalf("hydrateSessionTree: %v", err)
	}

	var got []string
	err := filepath.WalkDir(dest, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dest, path)
		if relErr != nil {
			return relErr
		}
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walking the hydrated tree: %v", err)
	}

	// mineN notes plus metadata/session.json, and nothing else. Anything more
	// means another session's objects landed in this tree.
	if want := mineN + 1; len(got) != want {
		sort.Strings(got)
		t.Errorf("hydrated %d files, want %d: %v. A larger session sharing the bucket "+
			"must not affect this one's cold start", len(got), want, got)
	}
	if _, err := os.Stat(filepath.Join(dest, "files", "big.md")); err == nil {
		t.Error("hydration pulled a file belonging to another session; the key prefix " +
			"is not being honoured")
	}
}
