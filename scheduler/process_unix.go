//go:build !windows

package main

import (
	"syscall"
)

func setProcessGroup(attr *syscall.SysProcAttr) {
	attr.Setpgid = true
}

func killProcessGroup(pid int) {
	syscall.Kill(-pid, syscall.SIGKILL)
}

// venvPython returns the venv python binary path for the current platform.
func venvPython() string {
	return ".venv/bin/python3"
}
