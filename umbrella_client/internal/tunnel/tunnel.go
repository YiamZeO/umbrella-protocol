//go:build !android
// +build !android

package tunnel

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"umbrella_client/internal/logging"
)

// var tunnelCoreProcess *os.Process

func StartTunnelCore(path string, args string, logsContainer *logging.LogsContainer) (*os.Process, error) {
	if path == "" {
		return nil, fmt.Errorf("tunnel core path is empty")
	}

	execName := filepath.Base(path)
	execDir := filepath.Dir(path)

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command(path, strings.Fields(args)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			HideWindow: true,
		}
	} else {
		cmd = exec.Command(path, strings.Fields(args)...)
	}
	cmd.Dir = execDir

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start tunnel core: %w", err)
	}

	tunnelCoreProcess := cmd.Process
	logsContainer.AppendLog("[System] Tunnel core started: " + execName)
	return tunnelCoreProcess, nil
}

func StopTunnelCore(tunnelCoreProcess *os.Process) error {
	if tunnelCoreProcess == nil {
		return nil
	}

	if runtime.GOOS == "windows" {
		if err := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(tunnelCoreProcess.Pid)).Run(); err != nil {
			time.Sleep(200 * time.Millisecond)
			if err := tunnelCoreProcess.Kill(); err != nil {
				return err
			}
		}
	} else {
		if err := tunnelCoreProcess.Signal(syscall.SIGTERM); err != nil {
			time.Sleep(500 * time.Millisecond)
			if tunnelCoreProcess.Signal(syscall.SIGQUIT) != nil {
				time.Sleep(200 * time.Millisecond)
				if err := tunnelCoreProcess.Kill(); err != nil {
					return err
				}
			}
		}
	}

	return nil
}
