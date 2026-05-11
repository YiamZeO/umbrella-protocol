# Инструкция по сборке и развертыванию Umbrella

## 🛠️ Сборка компонентов

Требуется Go 1.25+.

### 1. Umbrella Server (Linux)
Сборка выполняется для архитектуры Linux x64:
```bash
cd umbrella_server
$env:GOOS="linux"; $env:GOARCH="amd64"; go build -o umbrella_server ./cmd/server
```

### 2. Umbrella Client (Windows)
```bash
cd umbrella_client
go build -ldflags="-H windowsgui" -o umbrella-client.exe ./cmd/ui
```

### 3. Umbrella Client (Android)
Для сборки под Android требуется установленный `fyne-cross` и Android SDK:
```bash
fyne-cross android -app-id com.umbrella.client ./cmd/ui
```
*Примечание:* См. [android_fix.md](./android_fix.md) для корректной работы в фоне.

---

## 🚀 Развертывание

### Сервер (VPS)
1. Скопируйте бинарный файл `umbrella_server` на ваш сервер.
2. Запустите его первый раз для автоматической генерации файла конфигурации и ключей:
   ```bash
   chmod +x umbrella_server
   ./umbrella_server
   ```
3. Сервер выведет в лог сгенерированные `Private Key`, `Public Key` и `Short ID`. Сохраните их!
4. Отредактируйте созданный `config.yaml` для выбора нужного протокола и вставьте свои ключи.

### Клиент (Windows)
1. Запустите `umbrella-client.exe`.
2. Перейдите в **Settings -> Config**.
3. Заполните параметры сервера, используя данные, полученные при запуске сервера.
4. Выберите желаемый режим работы (`mode`).

---

## ⚙️ Конфигурация

Параметры конфигурации зависят от выбранного протокола маскировки. Подробное описание всех полей для каждого режима находится в соответствующих файлах документации:

- **XTLS**: [Подробности в xtls.md](./protocols/xtls.md)
- **Hysteria 2**: [Подробности в hysteria.md](./protocols/hysteria.md)
- **Torrent**: [Подробности в torrent.md](./protocols/torrent.md)

---

## 📚 Справочник конфигурации
Доступны полные схемы всех параметров:
- **[Схема конфигурации клиента](./client_config.md)**
- **[Схема конфигурации сервера](./server_config.md)**


