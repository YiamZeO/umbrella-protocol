package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/hashicorp/yamux"
)

var (
	decoyURLs []string
)

func init() {
	// Инициализируем генератор случайных чисел
	rand.Seed(time.Now().UnixNano())
}

// loadDecoyURLs загружает список URL из файла
func loadDecoyURLs(filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	return ParseDecoyURLs(data)
}

func ParseDecoyURLs(data []byte) error {
	if err := json.Unmarshal(data, &decoyURLs); err != nil {
		return err
	}

	if len(decoyURLs) == 0 {
		return fmt.Errorf("no URLs found")
	}
	return nil
}

// runDecoyTraffic запускает генерацию фонового трафика через существующую yamux сессию
func runDecoyTraffic(ctx context.Context, sess *yamux.Session) {
	if len(decoyURLs) == 0 {
		// Пытаемся загрузить из стандартного пути, если еще не загружено
		// В случае Android это может не сработать, если файл не извлечен,
		// поэтому лучше полагаться на ParseDecoyURLs(cfg.DecoyData) в Start()
		if err := loadDecoyURLs("decoy_reqs.json"); err != nil {
			log.Printf("[ERR] Decoy: failed to load URLs: %v", err)
			return
		}
	}

	log.Printf("Decoy: starting background traffic for session %p", sess)
	defer log.Printf("Decoy: stopping background traffic for session %p", sess)

	// Создаем кастомный HTTP клиент, который использует наш протокол туннелирования
	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				if sess.IsClosed() {
					return nil, fmt.Errorf("session closed")
				}
				host, portStr, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				p, _ := strconv.Atoi(portStr)
				if p == 443 {
					return openVisionStream(sess, host, uint16(p))
				}
				return openStream(sess, host, uint16(p))
			},
			MaxIdleConns:          0,
			DisableKeepAlives:     true,
			TLSHandshakeTimeout:   30 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		Timeout: 30 * time.Second,
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if sess.IsClosed() {
			return
		}

		// Выбираем количество запросов в эту секунду (от 1 до 5)
		numRequests := rand.Intn(5) + 1

		for i := 0; i < numRequests; i++ {
			// Выбираем случайный URL
			url := decoyURLs[rand.Intn(len(decoyURLs))]

			// Выполняем запрос в отдельной горутине, чтобы не блокировать цикл
			go func(u string) {
				// Используем HEAD вместо GET, чтобы генерировать качественный шум (TLS handshake + headers)
				// но не выкачивать тело страницы. Это экономит трафик, сохраняя маскировку.
				req, err := http.NewRequest("HEAD", u, nil)
				if err != nil {
					return
				}
				// Эмулируем браузер
				req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
				req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")

				resp, err := client.Do(req)
				if err != nil {
					// Ошибки здесь нормальны (таймауты, блокировки и т.д.)
					log.Printf("[ERR] Decoy: failed req: %v", err)
					return
				}
				// Важно закрыть тело, даже если оно пустое, чтобы освободить стрим
				resp.Body.Close()
			}(url)
		}

		// Ждем одну секунду до следующей пачки запросов
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return
		}
	}
}
