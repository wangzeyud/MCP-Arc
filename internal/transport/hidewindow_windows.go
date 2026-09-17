//go:build windows

package transport

import (
	"os/exec"
	"syscall"
)

// hideWindow keeps a spawned upstream from opening its own console window.
//
// A windowless mcp-arc (built with -H=windowsgui) has no console of its own, so
// a child console app such as node.exe would otherwise pop up a black window
// that looks like a stray terminal to end users. CREATE_NO_WINDOW + HideWindow
// start the child with no visible window while its stdin/stdout pipes keep
// working normally.
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
}
