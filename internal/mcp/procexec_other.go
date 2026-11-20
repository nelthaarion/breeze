//go:build !unix

package mcp

import "os/exec"

// setProcessGroup is a no-op where there are no Unix process groups. A timeout still
// returns promptly, because prepareChild sets cmd.WaitDelay; what it cannot do here is
// kill a grandchild that outlives the process it started.
func setProcessGroup(cmd *exec.Cmd) {}
