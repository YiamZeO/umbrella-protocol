//go:build windows
package ui

import (
	"os/exec"
	"syscall"
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procGenerateConsoleCtrlEvent = kernel32.NewProc("GenerateConsoleCtrlEvent")
)

const (
	CTRL_BREAK_EVENT = 1
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

func softKill(pid int) {
	procGenerateConsoleCtrlEvent.Call(uintptr(CTRL_BREAK_EVENT), uintptr(pid))
}

func isGracefulExit(err error) bool {
	if err == nil {
		return true
	}
	// 0xc000013a - код завершения Windows для процесса, закрытого через Ctrl+C
	if exitErr, ok := err.(*exec.ExitError); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return uint32(status.ExitStatus()) == 0xc000013a
		}
	}
	return false
}
