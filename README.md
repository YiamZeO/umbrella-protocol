
# Umbrella Protocol

SOCKS5 прокси поверх TCP:443 с [Reality](https://github.com/XTLS/REALITY) handshake и yamux мультиплексированием.

---

## Архитектура

```
[Приложение] → SOCKS5 (127.0.0.1:1080)
                    ↓
             [Umbrella клиент]
                    ↓
      TCP:443, TLS ClientHello = Chrome (utls)
      Reality auth в SessionId (ECDH + HKDF + AES-GCM)
      yamux — все SOCKS5 потоки поверх одного TLS-соединения
      Ротация сессии каждые 3–15 минут (crypto/rand)
                    ↓
             [Umbrella сервер]
      xtls/reality — аутентификация в ClientHello,
      неаутентифицированные запросы → fallback (cloudflare.com)
      yamux → handleTunnel → целевой хост
                    ↓
             [Целевой сайт]
```

### Уровень 1 — handshake (Reality)

Аутентификация встроена в TLS ClientHello, а не в прикладные данные:

1. Клиент запускает `BuildHandshakeState()` — ClientHello строится в памяти без отправки.
2. Клиент извлекает эфемерный x25519-ключ из KeyShare и вычисляет `sharedSecret = X25519(clientEphPriv, serverStaticPub)`.
3. `authKey = HKDF-SHA256(ikm=sharedSecret, salt=random[:20], info="REALITY")`.
4. Plaintext (16 байт) `[ver=0 | zeros | unix_time(4) | shortId(8)]` шифруется `AES-256-GCM(authKey, nonce=random[20:32])`, результат (32 байта) записывается в поле `SessionId`.
5. `MarshalClientHello()` → `Handshake()`.

Сервер (`xtls/reality`) делает то же самое независимо: если расшифровка `SessionId` валидна — соединение аутентифицировано. Иначе — прозрачный прокси к `--dest` (cloudflare.com). Сервер никогда не показывает свой собственный сертификат.

### Почему XTLS Vision не нужен

XTLS Vision был создан для решения проблемы **TLS-in-TLS**: в стандартном VLESS клиент сам устанавливает TLS-соединение с целевым сайтом и передаёт его через туннель — в результате по каналу клиент↔сервер проходит второй TLS ClientHello, что детектируется DPI по характерному паттерну.

В Umbrella эта проблема отсутствует архитектурно: клиент передаёт серверу только адрес назначения (`host:port`), а **сервер на VPS сам открывает TCP-соединение к целевому сайту**. По каналу клиент↔сервер идут уже зашифрованные данные приложения — без нового TLS handshake внутри туннеля. DPI видит один внешний TLS (Reality) и бинарный поток внутри, неотличимый от обычного HTTPS.

### Уровень 2 — мультиплексирование (yamux)

Все SOCKS5-потоки идут внутри одного TLS-соединения через yamux. Нет лавины отдельных TLS-handshake'ов при многопоточной загрузке.

### Уровень 3 — ротация сессии

Клиент ротирует TLS-соединение каждые **3–15 минут** случайным образом (`crypto/rand`). Активные потоки yamux дожидаются естественного завершения. Следующий SOCKS5-запрос прозрачно открывает новое соединение с новым handshake.

---

## Статус реализации

| Функция | Статус |
|---|---|
| Reality handshake (сервер: `xtls/reality`) | ✅ реализовано |
| Reality auth на клиенте (utls, ручной ECDH+HKDF+AES-GCM) | ✅ реализовано |
| Fallback к реальному сайту для неаутентифицированных запросов | ✅ реализовано (xtls/reality) |
| yamux мультиплексирование | ✅ реализовано |
| SOCKS5 (IPv4 / IPv6 / domain) | ✅ реализовано |
| Ротация сессии каждые 3–15 мин | ✅ реализовано |
| SmartShaper — поведенческий shaping трафика | ❌ не реализовано |
| UDP associate в SOCKS5 | ✅ реализовано |

---

## Сборка

Требуется Go 1.22+.

```bash
# Сервер
cd umbrella-protocol/server
go build -o umbrella-server .

# Клиент
cd umbrella-protocol/client
go build -o umbrella-client .
```

---

## Запуск

### Сервер (VPS)

```bash
# Первый запуск — сгенерировать ключи (сервер выведет их в лог):
./umbrella-server --port 443 --dest cloudflare.com:443

# Из лога запишите:
#   Public key (use as client --public-key): <base64>
#   Short ID (use as client --short-id):     <hex>

# Повторный запуск с сохранёнными ключами:
./umbrella-server \
  --port 443 \
  --private-key <base64_private_key> \
  --short-id <hex_short_id> \
  --dest cloudflare.com:443 \
  --server-names cloudflare.com
```

Флаги сервера:

| Флаг | По умолчанию | Описание |
|---|---|---|
| `--port` | `443` | Порт для входящих соединений |
| `--private-key` | генерируется | x25519 приватный ключ, base64 (32 байта) |
| `--short-id` | генерируется | Reality Short ID, hex до 16 символов |
| `--dest` | `cloudflare.com:443` | Fallback сайт для неаутентифицированных запросов |
| `--server-names` | hostname из `--dest` | Допустимые SNI через запятую |

### Клиент

```bash
./umbrella-client \
  --server vps.example.com:443 \
  --public-key <base64_public_key> \
  --short-id <hex_short_id> \
  --sni cloudflare.com \
  --listen 127.0.0.1:1080
```

Флаги клиента:

| Флаг | По умолчанию | Описание |
|---|---|---|
| `--server` | обязателен | Адрес сервера `host:port` |
| `--public-key` | обязателен | x25519 публичный ключ сервера, base64 |
| `--short-id` | обязателен | Reality Short ID, hex |
| `--sni` | `cloudflare.com` | SNI в TLS ClientHello |
| `--listen` | `127.0.0.1:1080` | Локальный SOCKS5 адрес |
| `--udp` | `true` | Включить UDP ASSOCIATE; `false` = только TCP |

### Настройка браузера / системы

После запуска клиента укажите SOCKS5-прокси `127.0.0.1:1080` в настройках браузера или системы. Весь TCP-трафик пойдёт через туннель.

---

## Что решает и что не решает

| Угроза | Решение |
|---|---|
| DPI детектирует VPN по TLS fingerprint | ✅ utls Chrome (HelloChrome_Auto) |
| DPI детектирует UUID/токен в прикладных данных | ✅ аутентификация только в ClientHello (Reality) |
| Активное зондирование сервера (GFW) | ✅ fallback к реальному сайту через xtls/reality |
| Корреляция по длительности TLS-соединения | ✅ ротация сессии 3–15 мин |
| Прозрачность для приложений без настройки | ❌ нужен SOCKS5 в приложении |
| DPI детектирует VPN по поведению потока | ❌ SmartShaper не реализован |

---

## TODO

### SmartShaper — поведенческий shaping

Сервер и клиент синхронизируют текущую «фазу» через отдельный yamux-поток (control channel). Сервер случайно выбирает фазу и её продолжительность, сообщает клиенту, оба применяют token-bucket throttling синхронно.

```go
type Phase struct {
    Name        string
    MinDuration time.Duration
    MaxDuration time.Duration
    DownMbps    float64
    UpMbps      float64
}

var Phases = []Phase{
    {"video",  6 * time.Second,  12 * time.Second, 50.0, 0.5},
    {"pause",  2 * time.Second,   5 * time.Second,  0.0, 0.0},
    {"web",    1 * time.Second,   3 * time.Second,  0.5, 0.1},
    {"ajax",   1 * time.Second,   2 * time.Second,  0.1, 0.1},
}
```

Детерминированного цикла нет — паттерн нерегулярен, как реальный браузерный трафик.

Library: `github.com/juju/ratelimit` уже присутствует в дереве зависимостей сервера как транзитивная зависимость `xtls/reality`.

---

## Использование

Ниже одни из сценариев использования Umbrella-протокола.

- Windows: запускаю `client`, использую Clash Verge в режиме TUN.
- Android: запускаю `client` через Termux, на устройстве включаю VPN через SocksDroid.
