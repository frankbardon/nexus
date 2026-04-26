package desktop

import (
	"context"
	"os"
	"path/filepath"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/plugins/io/wails"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// Compile-time assertions that scopedRuntime satisfies both
// the required Runtime and optional FileDialogRuntime interfaces.
var (
	_ wails.Runtime           = (*scopedRuntime)(nil)
	_ wails.FileDialogRuntime = (*scopedRuntime)(nil)
)

// scopedRuntime wraps the Wails runtime context and namespaces all
// event channels by agent ID. This lets multiple agent engines share
// a single Wails webview without their events colliding.
//
// The hub calls EmitEvent("nexus", ...) and OnEvent("nexus.input", ...);
// scopedRuntime translates those to "{agentID}:nexus" and
// "{agentID}:nexus.input" on the actual Wails runtime.
type scopedRuntime struct {
	ctx     context.Context
	agentID string
	store   SettingsStore // may be nil; used to enrich DefaultDirectory
}

func newScopedRuntime(ctx context.Context, agentID string, store SettingsStore) *scopedRuntime {
	return &scopedRuntime{ctx: ctx, agentID: agentID, store: store}
}

// EmitEvent publishes an event to the webview on a scoped channel.
func (r *scopedRuntime) EmitEvent(name string, optionalData ...any) {
	scoped := r.agentID + ":" + name
	wailsruntime.EventsEmit(r.ctx, scoped, optionalData...)
}

// OnEvent registers a callback for inbound events on a scoped channel.
func (r *scopedRuntime) OnEvent(name string, callback func(optionalData ...any)) {
	scoped := r.agentID + ":" + name
	wailsruntime.EventsOn(r.ctx, scoped, callback)
}

// OpenFileDialog presents a native single-file open dialog. When the
// caller has not specified a DefaultDirectory, the runtime enriches it
// from the agent's input_dir setting (then shared_data_dir, then
// ~/Documents) so file dialogs always open where the user's files are.
func (r *scopedRuntime) OpenFileDialog(opts wails.FileDialogOptions) (string, error) {
	// Enrich DefaultDirectory from settings if not already set.
	if opts.DefaultDirectory == "" {
		opts.DefaultDirectory = r.resolveDefaultDir()
	}

	var filters []wailsruntime.FileFilter
	for _, f := range opts.Filters {
		filters = append(filters, wailsruntime.FileFilter{
			DisplayName: f.DisplayName,
			Pattern:     f.Pattern,
		})
	}
	return wailsruntime.OpenFileDialog(r.ctx, wailsruntime.OpenDialogOptions{
		Title:            opts.Title,
		DefaultDirectory: opts.DefaultDirectory,
		Filters:          filters,
	})
}

// resolveDefaultDir finds the best default directory for file dialogs
// by checking settings in priority order.
func (r *scopedRuntime) resolveDefaultDir() string {
	if r.store != nil {
		if val, ok := r.store.Resolve(r.agentID, "input_dir", false); ok && val != "" {
			return engine.ExpandPath(val)
		}
		if val, ok := r.store.Resolve("shell", "shared_data_dir", false); ok && val != "" {
			return engine.ExpandPath(val)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		docs := filepath.Join(home, "Documents")
		if info, err := os.Stat(docs); err == nil && info.IsDir() {
			return docs
		}
	}
	return ""
}
