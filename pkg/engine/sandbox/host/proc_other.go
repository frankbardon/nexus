//go:build !unix

package host

import "os/exec"

// killTreeOnCancel is a no-op off unix: exec.CommandContext's default
// cancellation kills the direct child only.
func killTreeOnCancel(*exec.Cmd) {}
