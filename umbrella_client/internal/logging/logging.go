package logging

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2/data/binding"
)

type LogsContainer struct {
	LogMu       sync.RWMutex
	Logs        []string
	LogChan     chan string
	StatusBind  binding.String
	UpdateCount uint64
}

func NewLogsContainer() *LogsContainer {
	lc := &LogsContainer{
		Logs:        make([]string, 0, 201),
		LogChan:     make(chan string, 1000), // Буфер для лавины логов
		StatusBind:  binding.NewString(),
		UpdateCount: 0,
	}
	go lc.processLogs()
	return lc
}

func (l *LogsContainer) processLogs() {
	const maxLogs = 200
	for msg := range l.LogChan {
		l.LogMu.Lock()
		// Добавляем метку времени
		timestampedMsg := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05.000"), msg)

		if len(l.Logs) < maxLogs {
			l.Logs = append(l.Logs, timestampedMsg)
		} else {
			// Сдвигаем все элементы влево, чтобы освободить место в конце
			copy(l.Logs, l.Logs[1:])
			l.Logs[maxLogs-1] = timestampedMsg
		}
		l.UpdateCount++

		// Обновление статуса
		if strings.Contains(msg, "client on") {
			if l.StatusBind != nil {
				l.StatusBind.Set("Status: Started")
			}
		}
		l.LogMu.Unlock()
	}
}

func (l *LogsContainer) AppendLog(msg string) {
	select {
	case l.LogChan <- msg:
	default:
		// Если канал переполнен, игнорируем лог, чтобы не блокировать сеть
	}
}

type LogWriter struct {
	LogsContainer *LogsContainer
}

func (w *LogWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" {
		w.LogsContainer.AppendLog(msg)
	}
	return len(p), nil
}
