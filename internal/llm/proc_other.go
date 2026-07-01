//go:build !unix

package llm

import "os/exec"

// setProcessGroup is a no-op on platforms without POSIX process groups; the
// default CommandContext behaviour (kill the direct process) still applies.
func setProcessGroup(cmd *exec.Cmd) {}
