package main

import (
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
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&decoyURLs); err != nil {
		return err
	}

	if len(decoyURLs) == 0 {
		return fmt.Errorf("no URLs found in %s", filePath)
	}
	return nil
}

// runDecoyTraffic запускает генерацию фонового трафика через существующую yamux сессию
func runDecoyTraffic(sess *yamux.Session) {
	if len(decoyURLs) == 0 {
		// Пытаемся загрузить из стандартного пути, если еще не загружено
		if err := loadDecoyURLs("decoy_reqs.json"); err != nil {
			log.Printf("Decoy: failed to load URLs: %v", err)
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
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		Timeout: 15 * time.Second,
	}

	for {
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
					log.Printf("Decoy: failed req: %v", err)
					return
				}
				// Важно закрыть тело, даже если оно пустое, чтобы освободить стрим
				resp.Body.Close()
			}(url)
		}

		// Ждем одну секунду до следующей пачки запросов
		time.Sleep(time.Second)
	}
}
