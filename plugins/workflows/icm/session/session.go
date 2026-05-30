// Package session owns the on-disk layout of a single ICM workflow run.
// Every artifact, sidecar, and run-state document the orchestrator
// produces is funneled through a Session value so the directory layout
// stays uniform across stage modes (plain / loop / fan-out / fan-out +
// loop).
//
// The package is a leaf: it imports only the standard library plus
// log/slog and the tiny icmtypes shared-value package. The plugin layer
// (plugin.go) handles ExpandPath on user input and passes already-
// absolute paths down to NewSession.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// reservedInputStage is the canonical name of the synthetic input stage
// written into every session root. Mirrors the workspace loader's
// reserved name so logical refs like "00_input/brief.md" resolve cleanly.
const reservedInputStage = "00_input"

// Session is a single run of a workflow. It owns a directory under the
// plugin's per-session data dir and tracks all artifacts the
// orchestrator produces.
type Session struct {
	// RunID is the unique identifier for this run; supplied by the
	// caller (the plugin generates these via engine.GenerateID()).
	RunID string

	// RootDir is the absolute path to <dataDir>/<runID>/.
	RootDir string

	// StartedAt is captured at session creation in UTC.
	StartedAt time.Time

	// stateMu serializes state.json updates so fan-out goroutines that
	// each transition a different stage/item still produce a coherent
	// on-disk RunState document.
	stateMu sync.Mutex

	logger *slog.Logger
}

// NewSession creates the session root directory and its .icm/ subfolder.
// dataDir is the plugin's per-session data dir (ctx.DataDir); runID is
// caller-supplied. The directory is created with 0o755 perms.
func NewSession(dataDir, runID string, logger *slog.Logger) (*Session, error) {
	if runID == "" {
		return nil, errors.New("session: run ID required")
	}
	if dataDir == "" {
		return nil, errors.New("session: data dir required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	root := filepath.Join(dataDir, runID)
	if err := os.MkdirAll(filepath.Join(root, ".icm"), 0o755); err != nil {
		return nil, fmt.Errorf("session: create %s: %w", root, err)
	}
	return &Session{
		RunID:     runID,
		RootDir:   root,
		StartedAt: time.Now().UTC(),
		logger:    logger,
	}, nil
}

// OpenSession opens an existing session root for read-only inspection.
// V1 has no resume — this exists so operators (and future tooling) can
// inspect prior runs without re-creating the root.
func OpenSession(dataDir, runID string, logger *slog.Logger) (*Session, error) {
	if runID == "" {
		return nil, errors.New("session: run ID required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	root := filepath.Join(dataDir, runID)
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("session %s: %w", runID, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("session %s: %s is not a directory", runID, root)
	}
	return &Session{
		RunID:     runID,
		RootDir:   root,
		StartedAt: info.ModTime().UTC(),
		logger:    logger,
	}, nil
}

// ---------------------------------------------------------------------------
// Path helpers
// ---------------------------------------------------------------------------

// StageDir returns the per-stage directory under the session, regardless
// of stage mode. The orchestrator creates this lazily on first write.
func (s *Session) StageDir(stageID string) string {
	return filepath.Join(s.RootDir, stageID)
}

// ArtifactPath returns the path where a plain (non-looping, non-fan-out)
// stage's artifact will be written. Also used for fan-out aggregates.
func (s *Session) ArtifactPath(stageID, filename string) string {
	return filepath.Join(s.StageDir(stageID), filename)
}

// IterationDir returns the directory for a single iteration of a looping
// stage. iter is 1-based and zero-padded to two digits in the on-disk
// name (iter_01, iter_02, ...); ResolveLogicalRef handles iter_NN
// directories with any digit width.
func (s *Session) IterationDir(stageID string, iter int) string {
	return filepath.Join(s.StageDir(stageID), fmt.Sprintf("iter_%02d", iter))
}

// IterationArtifactPath returns the artifact path for a specific
// iteration of a looping stage.
func (s *Session) IterationArtifactPath(stageID, filename string, iter int) string {
	return filepath.Join(s.IterationDir(stageID, iter), filename)
}

// ItemDir returns the per-item directory for a fan-out stage.
func (s *Session) ItemDir(stageID, itemID string) string {
	return filepath.Join(s.StageDir(stageID), "items", itemID)
}

// ItemArtifactPath returns the artifact path for a single fan-out item
// in a non-looping fan-out stage.
func (s *Session) ItemArtifactPath(stageID, itemID, filename string) string {
	return filepath.Join(s.ItemDir(stageID, itemID), filename)
}

// ItemIterationArtifactPath returns the artifact path for a specific
// iteration of a single fan-out item (fan-out + loop composition).
func (s *Session) ItemIterationArtifactPath(stageID, itemID, filename string, iter int) string {
	return filepath.Join(s.ItemDir(stageID, itemID), fmt.Sprintf("iter_%02d", iter), filename)
}

// AggregatePath is the downstream-facing artifact for a fan-out stage —
// always written at the plain stage path regardless of per-item layout.
func (s *Session) AggregatePath(stageID, filename string) string {
	return s.ArtifactPath(stageID, filename)
}

// SidecarPath returns the path of the .icm.json metadata sidecar for an
// artifact at the given path.
func SidecarPath(artifactPath string) string {
	return artifactPath + ".icm.json"
}

// ResolveLogicalRef turns an inputs.artifacts entry of the form
// "<stage_id>/<filename>" into an absolute path under this session.
// For looping stages the latest iteration wins; for fan-out stages the
// aggregate wins (which is the same as the plain stage path).
//
// Iteration directories are parsed as integers and sorted numerically:
// iter_9 < iter_10 < iter_99 < iter_100. (Naive string-sort breaks past
// iter_99; that's a known handoff bug this implementation avoids.)
func (s *Session) ResolveLogicalRef(ref string) (string, error) {
	parts := strings.SplitN(ref, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("invalid artifact ref %q", ref)
	}
	stageID, filename := parts[0], parts[1]

	plain := s.ArtifactPath(stageID, filename)
	if _, err := os.Stat(plain); err == nil {
		return plain, nil
	}

	dir := s.StageDir(stageID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("artifact ref %q: %w", ref, err)
	}

	type iterEntry struct {
		index int
		name  string
	}
	var iters []iterEntry
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "iter_") {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimPrefix(name, "iter_"))
		if err != nil {
			// Skip directories whose suffix isn't a clean integer.
			continue
		}
		iters = append(iters, iterEntry{index: idx, name: name})
	}
	if len(iters) == 0 {
		return "", fmt.Errorf("artifact ref %q: no plain artifact and no iter_ directories under %s", ref, dir)
	}
	sort.Slice(iters, func(i, j int) bool {
		return iters[i].index > iters[j].index
	})
	for _, it := range iters {
		candidate := filepath.Join(dir, it.name, filename)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("artifact ref %q: no iter_*/%s exists under %s", ref, filename, dir)
}

// ---------------------------------------------------------------------------
// Artifact writes
// ---------------------------------------------------------------------------

// WriteArtifact ensures the parent directory exists and writes content
// atomically via temp file + rename. The orchestrator funnels every
// artifact write through this so the disk layout is uniform.
func (s *Session) WriteArtifact(path string, content []byte) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("session: mkdir %s: %w", parent, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return fmt.Errorf("session: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("session: rename %s: %w", path, err)
	}
	return nil
}

// WriteSidecar writes the .icm.json metadata file next to an artifact.
// WrittenAt is stamped here if the caller left it zero.
func (s *Session) WriteSidecar(artifactPath string, meta ArtifactMeta) error {
	if meta.WrittenAt.IsZero() {
		meta.WrittenAt = time.Now().UTC()
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("session: marshal sidecar: %w", err)
	}
	return s.WriteArtifact(SidecarPath(artifactPath), data)
}

// WriteRunMeta writes <runID>/.icm/run.json. Intended to be called once
// at session creation by the plugin; existing run.json content is
// overwritten (the file is atomic at the filesystem level).
func (s *Session) WriteRunMeta(meta RunMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("session: marshal run meta: %w", err)
	}
	return s.WriteArtifact(filepath.Join(s.RootDir, ".icm", "run.json"), data)
}

// CopyInitialInputs copies each provided source path into
// <session>/00_input/, preserving the basename. Called once at run start
// by the orchestrator when the operator supplied an explicit input set.
func (s *Session) CopyInitialInputs(srcPaths []string) error {
	if len(srcPaths) == 0 {
		return nil
	}
	dst := s.StageDir(reservedInputStage)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("session: mkdir 00_input: %w", err)
	}
	for _, src := range srcPaths {
		if err := copyFile(src, filepath.Join(dst, filepath.Base(src))); err != nil {
			return fmt.Errorf("session: copy initial input %s: %w", src, err)
		}
	}
	return nil
}

// CopyInitialInputsFromDir copies every regular file from the workspace
// inputs/ directory into <session>/00_input/. Missing source directory
// is a silent no-op (the run may not need any inputs).
func (s *Session) CopyInitialInputsFromDir(workspaceInputsDir string) error {
	info, err := os.Stat(workspaceInputsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("session: stat workspace inputs: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("session: workspace inputs path %s is not a directory", workspaceInputsDir)
	}
	entries, err := os.ReadDir(workspaceInputsDir)
	if err != nil {
		return fmt.Errorf("session: read workspace inputs: %w", err)
	}
	dst := s.StageDir(reservedInputStage)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("session: mkdir 00_input: %w", err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		src := filepath.Join(workspaceInputsDir, e.Name())
		if err := copyFile(src, filepath.Join(dst, e.Name())); err != nil {
			return fmt.Errorf("session: copy %s: %w", src, err)
		}
	}
	return nil
}

// ClearStage removes <stageDir> in full. Used by capability b
// (human_gate: end → restart) and capability d (loop.on_exhausted:
// human_gate → restart) to wipe a stage's artifacts before a re-run.
// Missing stage dir is not an error.
func (s *Session) ClearStage(stageID string) error {
	dir := s.StageDir(stageID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("session: clear stage %s: %w", stageID, err)
	}
	return nil
}

// copyFile is a minimal byte-stream copy. The session package
// deliberately does not preserve mode bits — every artifact under the
// session lands at 0o644 via WriteArtifact's WriteFile.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
