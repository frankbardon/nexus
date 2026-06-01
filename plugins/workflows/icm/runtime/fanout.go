package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/frankbardon/nexus/pkg/events"
	"github.com/frankbardon/nexus/plugins/workflows/icm/icmtypes"
	"github.com/frankbardon/nexus/plugins/workflows/icm/session"
	"github.com/frankbardon/nexus/plugins/workflows/icm/workspace"
)

// runFanOut executes a fan-out stage: resolve the source array, dispatch a
// goroutine per item bounded by max_parallel, write per-item artifacts,
// and finally write an aggregate at the plain stage path. Returns the
// aggregate path that downstream stages reference.
func (o *Orchestrator) runFanOut(ctx context.Context, stage *workspace.Stage) (string, error) {
	fanOut := stage.FanOut
	if fanOut == nil {
		return "", fmt.Errorf("runFanOut called on stage %q without fan_out config", stage.ID)
	}

	items, err := o.resolveFanOutItems(stage)
	if err != nil {
		return "", err
	}

	o.withState(func(_ *session.RunState) {
		o.stageStateRef(stage.ID).Items = make([]session.ItemState, len(items))
	})

	// Per-stage cancellation: halt policy needs to cancel siblings on
	// first failure.
	stageCtx, cancelStage := context.WithCancel(ctx)
	defer cancelStage()

	parallel := fanOut.MaxParallel
	if parallel < 1 {
		parallel = 1
	}
	sem := make(chan struct{}, parallel)

	outcomes := make([]fanOutOutcome, len(items))

	idSet := make(map[string]struct{}, len(items))
	var idMu sync.Mutex

	var wg sync.WaitGroup
	for i, raw := range items {
		i, raw := i, raw
		itemID := o.deriveItemID(stage, raw, i, idSet, &idMu)

		o.withState(func(_ *session.RunState) {
			o.stageStateRef(stage.ID).Items[i] = session.ItemState{
				ID:     itemID,
				Index:  i,
				Status: session.StageStatusPending,
			}
		})
		_ = o.saveState()

		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-stageCtx.Done():
				outcomes[i] = fanOutOutcome{index: i, itemID: itemID, item: raw, err: stageCtx.Err(), isError: true}
				return
			}
			defer func() { <-sem }()

			// Update state to running.
			o.withState(func(_ *session.RunState) {
				state := o.stageStateRef(stage.ID)
				state.Items[i].Status = session.StageStatusRunning
				state.Items[i].StartedAt = time.Now().UTC()
			})
			_ = o.saveState()

			if o.Bus != nil {
				_ = o.Bus.Emit("icm.fanout.item", icmtypes.ICMFanoutItem{
					SchemaVersion: icmtypes.ICMFanoutItemVersion,
					RunID:         o.RunID,
					StageID:       stage.ID,
					ItemID:        itemID,
					Index:         i,
					Total:         len(items),
					Status:        "active",
				})
			}

			path, output, ierr := o.runFanOutItem(stageCtx, stage, itemID, raw, i)

			// Update item state.
			o.withState(func(_ *session.RunState) {
				state := o.stageStateRef(stage.ID)
				state.Items[i].CompletedAt = time.Now().UTC()
				state.Items[i].Path = path
				if ierr != nil {
					state.Items[i].Status = session.StageStatusFailed
					state.Items[i].Error = ierr.Error()
				} else {
					state.Items[i].Status = session.StageStatusDone
				}
			})
			_ = o.saveState()

			outcomes[i] = fanOutOutcome{
				index:   i,
				itemID:  itemID,
				item:    raw,
				path:    path,
				output:  output,
				err:     ierr,
				isError: ierr != nil,
			}

			if o.Bus != nil {
				status := "completed"
				errMsg := ""
				if ierr != nil {
					status = "failed"
					errMsg = ierr.Error()
				}
				_ = o.Bus.Emit("icm.fanout.item", icmtypes.ICMFanoutItem{
					SchemaVersion: icmtypes.ICMFanoutItemVersion,
					RunID:         o.RunID,
					StageID:       stage.ID,
					ItemID:        itemID,
					Index:         i,
					Total:         len(items),
					Status:        status,
					Error:         errMsg,
				})
				detail := "item done"
				if status == "failed" {
					detail = "item failed"
				}
				o.emitWorkflowProgress(events.WorkflowProgress{
					Stage:       stage.ID,
					ItemsDone:   i + 1,
					ItemsTotal:  len(items),
					CurrentItem: itemID,
					Status:      events.WorkflowStatusItemDone,
					Detail:      detail,
				})
			}

			if ierr != nil && fanOut.OnItemFailure == workspace.ItemFailureHalt {
				cancelStage()
			}
		}()
	}
	wg.Wait()

	if fanOut.OnItemFailure == workspace.ItemFailureHalt {
		for _, oc := range outcomes {
			if oc.isError {
				return "", fmt.Errorf("fan-out item %q failed: %w", oc.itemID, oc.err)
			}
		}
	}

	// Aggregate the per-item outputs into the canonical stage artifact.
	aggregatePath := o.Session.AggregatePath(stage.ID, stage.Output.Filename)
	aggContent, err := writeFanOutAggregate(stage, outcomesAsRecords(outcomes))
	if err != nil {
		return "", fmt.Errorf("aggregate fan-out: %w", err)
	}
	if err := o.Session.WriteArtifact(aggregatePath, aggContent); err != nil {
		return "", err
	}
	meta := session.ArtifactMeta{
		StageID:   stage.ID,
		WrittenAt: time.Now().UTC(),
	}
	if err := o.Session.WriteSidecar(aggregatePath, meta); err != nil {
		return aggregatePath, err
	}
	return aggregatePath, nil
}

// runFanOutItem dispatches a single fan-out item. When the stage has a
// LoopConfig, the item goes through runLoopInner to iterate per-item;
// otherwise a single runInvocationFull call produces the item artifact.
func (o *Orchestrator) runFanOutItem(ctx context.Context, stage *workspace.Stage, itemID string, item any, index int) (string, []byte, error) {
	if stage.Loop != nil {
		res, err := o.runItemWithLoop(ctx, stage, itemID, item, index)
		if err != nil {
			return "", nil, err
		}
		return res.path, res.output, nil
	}
	res, err := o.runInvocationFull(ctx, invocationCtx{
		stage:     stage,
		itemID:    itemID,
		itemValue: item,
		itemIndex: index,
	})
	if err != nil {
		return "", nil, err
	}
	return res.path, res.output, nil
}

// runItemWithLoop composes fan-out + loop: scoped under
// items/<itemID>/iter_NN/, no per-item human gate (per the locked design).
// On exhaustion the item fails (no restart, no handoff).
func (o *Orchestrator) runItemWithLoop(ctx context.Context, stage *workspace.Stage, itemID string, item any, index int) (invocationResult, error) {
	_ = item // payload pulls item via invocationCtx.itemValue when used inside runLoopInner
	_ = index
	return o.runLoopInner(ctx, stage, itemID)
}

// resolveFanOutItems reads the source artifact, navigates the JSONPath
// expression (or uses the whole document), and asserts a []any result.
func (o *Orchestrator) resolveFanOutItems(stage *workspace.Stage) ([]any, error) {
	fanOut := stage.FanOut
	srcPath, err := o.Session.ResolveLogicalRef(fanOut.Source)
	if err != nil {
		return nil, fmt.Errorf("resolve fan_out source %q: %w", fanOut.Source, err)
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, fmt.Errorf("read fan_out source %q: %w", fanOut.Source, err)
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse fan_out source %q: %w", fanOut.Source, err)
	}

	var target any = doc
	if code := fanOut.CompiledJSONPath(); code != nil {
		iter := code.Run(doc)
		v, _ := iter.Next()
		if errVal, ok := v.(error); ok {
			return nil, fmt.Errorf("evaluate fan_out jsonpath: %w", errVal)
		}
		target = v
	}
	arr, ok := target.([]any)
	if !ok {
		return nil, fmt.Errorf("fan_out source %q jsonpath did not resolve to an array", fanOut.Source)
	}
	return arr, nil
}

// itemIDPattern restricts on-disk item folder names to a safe subset.
var itemIDPattern = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// deriveItemID computes a folder-safe item ID for a fan-out item using
// (in order): the stage's ItemID jq expression, the item's "id" field
// when no expression is set, or the fallback "item_NNNN". Collisions are
// disambiguated by appending "_<idx>".
func (o *Orchestrator) deriveItemID(stage *workspace.Stage, item any, index int, used map[string]struct{}, mu *sync.Mutex) string {
	var raw string
	if code := stage.FanOut.CompiledItemID(); code != nil {
		iter := code.Run(item)
		v, _ := iter.Next()
		if v != nil {
			raw = fmt.Sprintf("%v", v)
		}
	}
	if raw == "" {
		// Convenience: if items are objects with an "id" field, use it.
		if m, ok := item.(map[string]any); ok {
			if id, ok := m["id"].(string); ok && id != "" {
				raw = id
			}
		}
	}
	if raw == "" {
		raw = fmt.Sprintf("item_%04d", index)
	}
	id := sanitizeItemID(raw)
	mu.Lock()
	defer mu.Unlock()
	if _, clash := used[id]; clash {
		id = fmt.Sprintf("%s_%d", id, index)
	}
	used[id] = struct{}{}
	return id
}

// sanitizeItemID replaces every disallowed rune with "_" and trims any
// resulting leading / trailing underscores. The empty string maps to
// "item".
func sanitizeItemID(raw string) string {
	clean := itemIDPattern.ReplaceAllString(raw, "_")
	clean = strings.Trim(clean, "_")
	if clean == "" {
		return "item"
	}
	return clean
}

// fanOutItemRecord is the per-item shape used by the aggregator. Loop
// composition + plain fan-out share the same record format; only the
// outer aggregate format toggles (json vs text).
type fanOutItemRecord struct {
	itemID string
	item   any
	path   string
	output []byte
	err    error
}

// fanOutOutcome is the per-item runtime outcome produced by runFanOut's
// goroutines.
type fanOutOutcome struct {
	index   int
	itemID  string
	item    any
	path    string
	output  []byte
	err     error
	isError bool
}

// outcomesAsRecords converts the goroutine outcome shape into the
// aggregator's record shape.
func outcomesAsRecords(items []fanOutOutcome) []fanOutItemRecord {
	out := make([]fanOutItemRecord, 0, len(items))
	for _, it := range items {
		out = append(out, fanOutItemRecord{
			itemID: it.itemID,
			item:   it.item,
			path:   it.path,
			output: it.output,
			err:    it.err,
		})
	}
	return out
}

// writeFanOutAggregate produces the aggregate bytes for the stage's output
// format. The on-disk path is the canonical stage path (so downstream
// references via "<stage_id>/<filename>" resolve naturally).
func writeFanOutAggregate(stage *workspace.Stage, records []fanOutItemRecord) ([]byte, error) {
	format := stage.Output.Format
	if format == "" {
		format = workspace.OutputJSON
	}
	switch format {
	case workspace.OutputJSON:
		return marshalJSONAggregate(records)
	case workspace.OutputText:
		return marshalTextAggregate(records), nil
	default:
		return nil, fmt.Errorf("unknown output format %q", format)
	}
}

// marshalJSONAggregate produces the canonical JSON shape:
//
//	[
//	  {"item": ..., "path": "...", "result": ...},
//	  {"item": ..., "path": "...", "error": "..."}
//	]
func marshalJSONAggregate(records []fanOutItemRecord) ([]byte, error) {
	out := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		entry := map[string]any{
			"item": rec.item,
			"path": rec.path,
		}
		if rec.err != nil {
			entry["error"] = rec.err.Error()
		} else {
			// Try to embed parsed JSON; fall back to the raw string.
			var parsed any
			if err := json.Unmarshal(rec.output, &parsed); err == nil {
				entry["result"] = parsed
			} else {
				entry["result"] = string(rec.output)
			}
		}
		out = append(out, entry)
	}
	return json.MarshalIndent(out, "", "  ")
}

// marshalTextAggregate produces the canonical text shape:
//
//	## Item: <itemID>
//
//	<output>
//
//	## Item: <itemID> (failed)
//
//	<error>
func marshalTextAggregate(records []fanOutItemRecord) []byte {
	var sb strings.Builder
	for i, rec := range records {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		if rec.err != nil {
			fmt.Fprintf(&sb, "## Item: %s (failed)\n\n%s", rec.itemID, rec.err.Error())
		} else {
			fmt.Fprintf(&sb, "## Item: %s\n\n%s", rec.itemID, string(rec.output))
		}
	}
	return []byte(sb.String())
}
