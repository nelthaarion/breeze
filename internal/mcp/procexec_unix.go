//go:build unix

package mcp

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in a process group of its own and makes cancellation
// kill the whole group.
//
// The group matters because the child is rarely the thing that is hung. `go test`
// starts the compiled test binary as a grandchild; killing `go` alone leaves that binary
// running, holding the output pipe, and the caller waiting on it. Signalling the
// negative pid reaches every process the child started.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			// The group is already gone, or could not be signalled: fall back to the
			// child itself, which reports os.ErrProcessDone if it had exited.
			return cmd.Process.Kill()
		}
		return nil
	}
}
