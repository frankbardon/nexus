package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/allplugins"
)

func main() {
	// Subcommand dispatch: if the first positional arg is a known
	// subcommand, route to it. Otherwise fall through to the default
	// run-the-config flow so existing invocations stay unchanged.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "ingest":
			os.Exit(runIngest(os.Args[2:]))
		}
	}

	configPath := flag.String("config", "nexus.yaml", "path to config file")
	recallSession := flag.String("recall", "", "session ID to recall and resume")
	flag.Parse()

	// When recalling, load config from the session's snapshot.
	effectiveConfig := *configPath
	if *recallSession != "" {
		snapshotPath, err := engine.SessionConfigSnapshotPath(*configPath, *recallSession)
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

	// Run handles SIGINT/SIGTERM internally; embedders call Boot/Stop directly.
	if err := eng.Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "engine error: %v\n", err)
		os.Exit(1)
	}
}
