package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/frankbardon/nexus/pkg/engine"
	"github.com/frankbardon/nexus/pkg/engine/journal"
)

// runSession is the "nexus session" subcommand multiplexer. Routes to a
// per-action handler — the actions are deliberately tiny so the entry
// point stays a fan-out switch rather than a flag soup.
func runSession(args []string) int {
	if len(args) == 0 {
		sessionUsage()
		return 2
	}
	action, rest := args[0], args[1:]
	switch action {
	case "inspect":
		return runSessionInspect(rest)
	case "rewind":
		return runSessionRewind(rest)
	case "restore":
		return runSessionRestore(rest)
	case "archives":
		return runSessionArchives(rest)
	default:
		sessionUsage()
		fmt.Fprintf(os.Stderr, "\nerror: unknown action %q\n", action)
		return 2
	}
}

func sessionUsage() {
	fmt.Fprintln(os.Stderr, `usage: nexus session <action> [flags] <session-id>

actions:
  inspect    Print the session's journal as a timeline.
  rewind     Archive the journal and truncate to a target seq.
  restore    Swap the live journal for a previously archived snapshot.
  archives   List archive snapshots for the session.

each action accepts:
  --config <path>   path to nexus config (default: nexus.yaml)`)
}

func runSessionInspect(args []string) int {
	fs := flag.NewFlagSet("session inspect", flag.ExitOnError)
	configPath := fs.String("config", "nexus.yaml", "path to config file")
	limit := fs.Int("limit", 0, "print at most this many events (0 = all)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: nexus session inspect [--limit=N] <session-id>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	sessionID := fs.Arg(0)

	dir, err := resolveJournalDir(*configPath, sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	r, err := journal.Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open journal: %v\n", err)
		return 1
	}
	count := 0
	iterErr := r.Iter(func(env journal.Envelope) bool {
		count++
		marker := ""
		if env.SideEffect {
			marker = "*"
		}
		if env.Vetoed {
			marker = "!"
		}
		fmt.Printf("%6d  %s  %-22s %s%s\n", env.Seq, env.Ts.UTC().Format("15:04:05.000"), env.Type, marker, env.EventID)
		return *limit == 0 || count < *limit
	})
	if iterErr != nil {
		fmt.Fprintf(os.Stderr, "iter: %v\n", iterErr)
		return 1
	}
	fmt.Fprintf(os.Stderr, "\n%d events shown.\n", count)
	return 0
}

func runSessionRewind(args []string) int {
	fs := flag.NewFlagSet("session rewind", flag.ExitOnError)
	configPath := fs.String("config", "nexus.yaml", "path to config file")
	toSeq := fs.Uint64("to-seq", 0, "highest seq to keep (inclusive); required")
	yes := fs.Bool("yes", false, "skip confirmation prompt")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: nexus session rewind --to-seq=N <session-id>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *toSeq == 0 {
		fs.Usage()
		return 2
	}
	sessionID := fs.Arg(0)
	if !*yes {
		fmt.Fprintf(os.Stderr, "this will archive the live journal for %q and truncate to seq %d.\nset --yes to proceed.\n", sessionID, *toSeq)
		return 1
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	res, err := engine.RewindSession(cfg, sessionID, *toSeq)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rewind: %v\n", err)
		return 1
	}
	fmt.Printf("rewound %s to seq %d (%d events kept, %d archived) → archive=%s\n",
		sessionID, res.TruncatedSeq, res.EventsKept, res.EventsArchived, res.ArchiveName)
	return 0
}

func runSessionRestore(args []string) int {
	fs := flag.NewFlagSet("session restore", flag.ExitOnError)
	configPath := fs.String("config", "nexus.yaml", "path to config file")
	archiveName := fs.String("from-archive", "", "archive directory name to restore from; required")
	yes := fs.Bool("yes", false, "skip confirmation prompt")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: nexus session restore --from-archive=NAME <session-id>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *archiveName == "" {
		fs.Usage()
		return 2
	}
	sessionID := fs.Arg(0)
	if !*yes {
		fmt.Fprintf(os.Stderr, "this will rotate the live journal for %q and replace it with archive %q.\nset --yes to proceed.\n", sessionID, *archiveName)
		return 1
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if err := engine.RestoreSession(cfg, sessionID, *archiveName); err != nil {
		fmt.Fprintf(os.Stderr, "restore: %v\n", err)
		return 1
	}
	fmt.Printf("restored %s from archive %s\n", sessionID, *archiveName)
	return 0
}

func runSessionArchives(args []string) int {
	fs := flag.NewFlagSet("session archives", flag.ExitOnError)
	configPath := fs.String("config", "nexus.yaml", "path to config file")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: nexus session archives <session-id>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	sessionID := fs.Arg(0)
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	names, err := engine.ListSessionArchives(cfg, sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "archives: %v\n", err)
		return 1
	}
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "no archives.")
		return 0
	}
	for _, n := range names {
		fmt.Println(n)
	}
	return 0
}

func loadConfig(path string) (engine.Config, error) {
	cfg, err := engine.LoadConfig(engine.ExpandPath(path))
	if err != nil {
		return engine.Config{}, err
	}
	return *cfg, nil
}

func resolveJournalDir(configPath, sessionID string) (string, error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return "", err
	}
	root := engine.ExpandPath(cfg.Core.Sessions.Root)
	if root == "" {
		return "", fmt.Errorf("sessions.root not configured")
	}
	if strings.ContainsAny(sessionID, "/\\") {
		return "", fmt.Errorf("invalid session id %q", sessionID)
	}
	return filepath.Join(root, sessionID, "journal"), nil
}
