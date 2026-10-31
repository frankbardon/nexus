//go:build unix

package host

import (
	"os/exec"
	"syscall"
)

// killTreeOnCancel runs the command in its own process group and makes
// context cancellation SIGKILL the whole group. Without it, cancelling the
// context kills only `sh`; a grandchild (`sleep`, a pipeline stage, a
// backgrounded job) is reparented and keeps running after the tool call is
// over.
func killTreeOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid targets the process group the child leads.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
