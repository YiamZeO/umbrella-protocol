#!/bin/bash

# Настройки
PROCESS_NAME="umbrella_server"
EXEC_COMMAND="./umbrella_server"
LOG_FILE="umbrella_server.log"
CHECK_INTERVAL=10

echo "[$(date)] Watchdog started for $PROCESS_NAME"

while true; do
    # Проверяем наличие процесса по имени
    if ! pgrep -l "$PROCESS_NAME" > /dev/null; then
        echo "[$(date)] $PROCESS_NAME not found! Restarting..."

        # Запуск сервера в фоне с перенаправлением логов
        nohup $EXEC_COMMAND > "$LOG_FILE" 2>&1 &

        echo "[$(date)] $PROCESS_NAME restarted with PID $!"
    fi

    # Ожидание перед следующей проверкой
    sleep $CHECK_INTERVAL
done
