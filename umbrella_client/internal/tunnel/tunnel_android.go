//go:build android
// +build android

package tunnel

import (
	"os"

	"umbrella_client/internal/logging"
)

func StartTunnelCore(path string, args string, logsContainer *logging.LogsContainer) (*os.Process, error) {
	return nil, nil
}

func StopTunnelCore(tunnelCoreProcess *os.Process) error {
	return nil
}
