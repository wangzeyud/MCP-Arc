//go:build !windows

package transport

import "os/exec"

// hideWindow is a no-op on platforms that have no console-window concept.
func hideWindow(cmd *exec.Cmd) {}
