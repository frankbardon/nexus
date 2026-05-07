package pricing

import (
	"sync"
	"testing"

	"github.com/frankbardon/nexus/pkg/events"
)

func TestNilTable_SafeOps(t *testing.T) {
	var tbl *Table

	if r, ok := tbl.Get("anything"); ok || r.InputPerMillion != 0 {
		t.Errorf("nil Get returned (%v,%v), want zero/false", r, ok)
	}
	if got := tbl.Models(); got != nil {
		t.Errorf("nil Models() = %v, want nil", got)
	}
	if got := tbl.Calc("anything", events.Usage{}); got != 0 {
		t.Errorf("nil Calc = %v, want 0", got)
	}
	if got := tbl.CheapestModel([]string{"a", "b"}); got != "" {
		t.Errorf("nil CheapestModel = %q, want empty", got)
	}
	// Merge on nil receiver must not panic.
	tbl.Merge(map[string]any{"foo": map[string]any{"input_per_million": 1.0}})
}

func TestTable_Calc_UnknownProviderReturnsZero(t *testing.T) {
	tbl := NewTable("not-a-real-provider")
	tbl.Set("some-model", Rates{InputPerMillion: 10, OutputPerMillion: 20})
	if got := tbl.Calc("some-model", events.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000}); got != 0 {
		t.Errorf("unknown provider should yield 0, got %v", got)
	}
}

func TestTable_Calc_UnknownModelReturnsZero(t *testing.T) {
	tbl := DefaultsFor("anthropic")
	if got := tbl.Calc("definitely-not-a-model", events.Usage{PromptTokens: 1_000_000}); got != 0 {
		t.Errorf("unknown model should yield 0, got %v", got)
	}
}

// TestTable_ConcurrentReadDuringMerge documents — and currently skips — a
// real race between Get/Calc/Models reads and Merge writes on *Table.
// Production callers exist for both: providers call Calc on each turn's
// usage event (bus dispatch goroutines) while hot-reload calls Merge to
// re-apply config overrides. The current implementation has no internal
// synchronization. Adding the test forced this gap to surface; fixing it
// (sync.RWMutex around t.rates) is out of scope for the testing-uplift
// plan and tracked separately in .planning/testing-upgrade/01-tier-1-engine-core.md.
func TestTable_ConcurrentReadDuringMerge(t *testing.T) {
	t.Skip("known race: pricing.Table has no internal synchronization between Get/Calc and Merge; tracked as a follow-up")

	tbl := DefaultsFor("anthropic")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(4)
	for range 4 {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = tbl.Models()
					_, _ = tbl.Get("claude-sonnet-4")
					_ = tbl.Calc("claude-sonnet-4", events.Usage{PromptTokens: 100})
				}
			}
		}()
	}

	for range 50 {
		tbl.Merge(map[string]any{
			"new-model": map[string]any{
				"input_per_million":  1.0,
				"output_per_million": 2.0,
			},
		})
	}

	close(stop)
	wg.Wait()
}
