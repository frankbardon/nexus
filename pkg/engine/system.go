package engine

import (
	"os/exec"
	"runtime"
)

// SystemInfo provides platform details detected at engine startup.
// Available to all plugins via PluginContext.System.
type SystemInfo struct {
	OS   string // runtime.GOOS: "darwin", "linux", "windows", etc.
	Arch string // runtime.GOARCH: "amd64", "arm64", etc.

	// OpenCmd is the OS-native command for opening files/folders/URLs.
	// Empty if no suitable command was found.
	OpenCmd  string
	OpenArgs []string // extra args prepended before the target path
}

// DetectSystem builds a SystemInfo from the current runtime environment.
func DetectSystem() *SystemInfo {
	info := &SystemInfo{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}

	cmd, args := resolveOpenCmd(info.OS)
	info.OpenCmd = cmd
	info.OpenArgs = args

	return info
}

// HasOpen reports whether an OS-native open command is available.
func (s *SystemInfo) HasOpen() bool {
	return s.OpenCmd != ""
}

// resolveOpenCmd returns the open command and any extra args for the given OS.
func resolveOpenCmd(goos string) (string, []string) {
	switch goos {
	case "darwin":
		if path, err := exec.LookPath("open"); err == nil {
			return path, nil
		}
	case "linux":
		if path, err := exec.LookPath("xdg-open"); err == nil {
			return path, nil
		}
	case "windows":
		// "cmd /c start "" <path>" — empty title required when path has spaces.
		if path, err := exec.LookPath("cmd"); err == nil {
			return path, []string{"/c", "start", ""}
		}
	}
	return "", nil
}
