package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"
	"github.com/frankbardon/nexus/pkg/engine/configwatch"
	"github.com/frankbardon/nexus/pkg/events"
)

func main() {
	// Subcommand dispatch: if the first positional arg is a known
	// subcommand, route to it. Otherwise fall through to the default
	// run-the-config flow so existing invocations stay unchanged.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "ingest":
			os.Exit(runIngest(os.Args[2:]))
		case "eval":
			os.Exit(runEval(os.Args[2:]))
		case "session":
			os.Exit(runSession(os.Args[2:]))
		case "hitl":
			os.Exit(runHITL(os.Args[2:]))
		case "approve":
			os.Exit(runHITLApprove(os.Args[2:]))
		case "reject":
			os.Exit(runHITLReject(os.Args[2:]))
		case "cost":
			os.Exit(runCost(os.Args[2:]))
		}
	}

	configPath := flag.String("config", "nexus.yaml", "path to config file")
	recallSession := flag.String("recall", "", "session ID to recall and resume")
	replaySession := flag.String("replay", "", "session ID to replay deterministically (no external calls)")
	flag.Parse()

	// When recalling or replaying, load config from the source session's
	// snapshot so the replay matches the original plugin set.
	effectiveConfig := *configPath
	sourceForSnapshot := *recallSession
	if *replaySession != "" {
		sourceForSnapshot = *replaySession
	}
	if sourceForSnapshot != "" {
		snapshotPath, err := engine.SessionConfigSnapshotPath(*configPath, sourceForSnapshot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to locate session config: %v\n", err)
			os.Exit(1)
		}
		effectiveConfig = snapshotPath
	}

	// Create engine.
	eng, err := engine.New(effectiveConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create engine: %v\n", err)
		os.Exit(1)
	}

	eng.RecallSessionID = *recallSession

	// Register all built-in plugins.
	allplugins.RegisterAll(eng.Registry)

	if *replaySession != "" {
		ctx := context.Background()
		if err := eng.Boot(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "engine boot failed: %v\n", err)
			os.Exit(1)
		}
		if err := eng.ReplaySession(ctx, *replaySession); err != nil {
			fmt.Fprintf(os.Stderr, "replay error: %v\n", err)
			_ = eng.Stop(context.Background())
			os.Exit(1)
		}
		if err := eng.Stop(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "engine stop error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Boot/Stop directly so SIGHUP can hot-reload while SIGINT/SIGTERM exit.
	// Mirrors what eng.Run does internally, plus the SIGHUP and watcher hooks.
	if err := runWithReload(eng, effectiveConfig); err != nil {
		fmt.Fprintf(os.Stderr, "engine error: %v\n", err)
		os.Exit(1)
	}
}

// runWithReload is the CLI's Boot+wait+Stop loop with SIGHUP reload and an
// optional fsnotify watcher driven by engine.config_watch.enabled. SIGINT
// and SIGTERM continue to terminate the engine; SIGHUP triggers
// engine.ReloadConfig with the same -config path the binary was launched
// with.
func runWithReload(eng *engine.Engine, configPath string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := eng.Boot(ctx); err != nil {
		return fmt.Errorf("boot: %w", err)
	}

	// Optional fsnotify watcher. Off by default; opt in via
	// engine.config_watch.enabled in YAML. The reload callback re-reads
	// the original config path to keep the trigger semantics identical to
	// SIGHUP — operators get the same behavior whether they type kill
	// -HUP or save the file.
	var watcher *configwatch.Watcher
	if eng.Config.Engine.ConfigWatch.Enabled && configPath != "" {
		debounce := eng.Config.Engine.ConfigWatch.Debounce
		if debounce <= 0 {
			debounce = engine.DefaultConfigWatchDebounce
		}
		w, err := configwatch.New(configPath, debounce, eng.Logger, func() {
			if err := reloadFromPath(eng, configPath); err != nil {
				eng.Logger.Error("config watch reload failed", "error", err)
			}
		})
		if err != nil {
			eng.Logger.Warn("config watch failed to start", "error", err)
		} else {
			watcher = w
			eng.Logger.Info("config watch active",
				"path", configPath,
				"debounce", debounce)
		}
	}
	defer func() {
		if watcher != nil {
			_ = watcher.Close()
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				eng.Logger.Info("received SIGHUP — reloading config", "path", configPath)
				if err := reloadFromPath(eng, configPath); err != nil {
					eng.Logger.Error("SIGHUP reload failed", "error", err)
				}
				continue
			}
			eng.Logger.Info("received signal", "signal", sig)
			_ = eng.Bus.Emit("cancel.request", events.CancelRequest{
				SchemaVersion: events.CancelRequestVersion,
				Source:        "signal:" + sig.String(),
			})
			return eng.Stop(context.Background())
		case <-eng.SessionEnded():
			eng.Logger.Info("session ended")
			return eng.Stop(context.Background())
		case <-ctx.Done():
			eng.Logger.Info("context cancelled")
			_ = eng.Bus.Emit("cancel.request", events.CancelRequest{
				SchemaVersion: events.CancelRequestVersion,
				Source:        "context",
			})
			return eng.Stop(context.Background())
		}
	}
}

// reloadFromPath re-reads the config file at path and applies it via
// Engine.ReloadConfig. Empty path is a no-op (the engine was constructed
// from defaults; there is no source file to re-read).
func reloadFromPath(eng *engine.Engine, path string) error {
	if path == "" {
		return fmt.Errorf("no config path to reload from")
	}
	cfg, err := engine.LoadConfig(path)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	return eng.ReloadConfig(cfg)
}
