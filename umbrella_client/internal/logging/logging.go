package logging

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2/data/binding"
)

type LogsContainer struct {
	LogMu               sync.Mutex
	AllLogs             string
	LogBind, StatusBind binding.String
}

func (l *LogsContainer) AppendLog(msg string) {
	l.LogMu.Lock()
	defer l.LogMu.Unlock()

	// Add timestamp to all log messages
	timestampedMsg := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05.000"), msg)
	l.AllLogs += timestampedMsg + "\n"

	trimmed := strings.TrimRight(l.AllLogs, "\n")
	lines := strings.Split(trimmed, "\n")
	if len(lines) > 200 {
		l.AllLogs = strings.Join(lines[len(lines)-200:], "\n") + "\n"
	}

	// Update the binding which will trigger the display update
	if l.LogBind != nil {
		l.LogBind.Set(l.AllLogs)
	}
	// If the client listener started successfully it logs "Umbrella/REALITY client on ...".
	// Detect that message and update the status to "Started".
	if strings.Contains(msg, "Umbrella/REALITY client on") {
		if l.StatusBind != nil {
			l.StatusBind.Set("Status: Started")
		}
	}
}

type LogWriter struct {
	LogsContainer *LogsContainer
}

func (w *LogWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		// Use a goroutine to avoid potential deadlocks
		go w.LogsContainer.AppendLog(msg)
	}
	return len(p), nil
}
