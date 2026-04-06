//go:build !windows
package ui

import (
	"os"
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

func softKill(pid int) {
	// Отправляем SIGINT всей группе процессов (отрицательный PID)
	syscall.Kill(-pid, syscall.SIGINT)
}

func isGracefulExit(err error) bool {
	if err == nil {
		return true
	}
	// В Unix SIGINT обычно завершает процесс с сигналом 2
	if exitErr, ok := err.(*exec.ExitError); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return status.Signal() == os.Interrupt || status.Signal() == syscall.SIGINT
		}
	}
	return false
}
