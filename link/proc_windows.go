//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// hideWindow stops a black window from flashing up for every FFmpeg started.
func hideWindow(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
