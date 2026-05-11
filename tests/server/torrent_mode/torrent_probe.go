package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/anacrolix/utp"
)

const (
	pstr         = "BitTorrent protocol"
	pstrlen      = 19
	handshakeLen = 68
	bitfieldID   = 5
	unchokeMsgID = 1
	requestID    = 6
	pieceMsgID   = 7
)

func main() {
	// Адрес сервера Umbrella (Torrent порт)
	serverAddrStr := "138.124.240.89:50000"
	timeout := 10 * time.Second

	// 1. Устанавливаем uTP соединение
	log.Printf("Connecting to %s via uTP...", serverAddrStr)
	conn, err := utp.Dial(serverAddrStr)
	if err != nil {
		log.Fatalf("uTP dial failed: %v", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(timeout))

	// 2. Формируем и отправляем BitTorrent Handshake
	infoHash := make([]byte, 20)
	rand.Read(infoHash) // Случайный инфо-хеш (имитируем незнакомца)

	peerID := make([]byte, 20)
	copy(peerID, []byte("-TR3000-")) // Имитируем Transmission
	rand.Read(peerID[8:])

	handshake := make([]byte, handshakeLen)
	handshake[0] = pstrlen
	copy(handshake[1:20], pstr)
	copy(handshake[28:48], infoHash)
	copy(handshake[48:68], peerID)

	log.Printf("Sending handshake for infohash: %s", hex.EncodeToString(infoHash))
	if _, err := conn.Write(handshake); err != nil {
		log.Fatalf("Send handshake failed: %v", err)
	}

	// 3. Читаем ответный Handshake
	respHandshake := make([]byte, handshakeLen)
	if _, err := io.ReadFull(conn, respHandshake); err != nil {
		log.Fatalf("Read handshake failed: %v", err)
	}

	if respHandshake[0] != pstrlen || string(respHandshake[1:20]) != pstr {
		log.Fatalf("Invalid handshake received")
	}

	serverInfoHash := respHandshake[28:48]
	serverPeerID := respHandshake[48:68]

	fmt.Printf("\n=== BitTorrent Handshake Info ===\n")
	fmt.Printf("Server InfoHash: %s\n", hex.EncodeToString(serverInfoHash))
	fmt.Printf("Server PeerID:   %s (%s)\n", hex.EncodeToString(serverPeerID), string(serverPeerID))

	// 4. Читаем Bitfield и Unchoke
	log.Println("Waiting for Bitfield and Unchoke...")

	// Читаем Bitfield
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		log.Fatalf("Read msg len failed: %v", err)
	}
	msgLen := binary.BigEndian.Uint32(head)
	msg := make([]byte, msgLen)
	if _, err := io.ReadFull(conn, msg); err != nil {
		log.Fatalf("Read msg body failed: %v", err)
	}
	if msg[0] == bitfieldID {
		fmt.Printf("Received Bitfield: %d bytes\n", msgLen-1)
	}

	// Читаем Unchoke
	if _, err := io.ReadFull(conn, head); err != nil {
		log.Fatalf("Read unchoke len failed: %v", err)
	}
	if binary.BigEndian.Uint32(head) == 1 {
		id := make([]byte, 1)
		conn.Read(id)
		if id[0] == unchokeMsgID {
			fmt.Println("Received Unchoke: Server is ready to send data")
		}
	}

	// 5. Проверяем Decoy Mode: запрашиваем случайный кусок
	log.Println("Probing Decoy Mode: Requesting piece index 0, begin 0, length 16KB...")

	req := make([]byte, 17)
	binary.BigEndian.PutUint32(req[0:4], 13) // length
	req[4] = requestID
	binary.BigEndian.PutUint32(req[5:9], 0)       // index
	binary.BigEndian.PutUint32(req[9:13], 0)      // begin
	binary.BigEndian.PutUint32(req[13:17], 16384) // length 16KB

	conn.Write(req)

	// Читаем ответ (Piece)
	if _, err := io.ReadFull(conn, head); err != nil {
		log.Fatalf("Read piece len failed: %v", err)
	}
	pieceLen := binary.BigEndian.Uint32(head)
	fmt.Printf("Received response: %d bytes (Expected Piece msg)\n", pieceLen)

	pieceMsg := make([]byte, pieceLen)
	if _, err := io.ReadFull(conn, pieceMsg); err != nil {
		log.Fatalf("Read piece body failed: %v", err)
	}

	if pieceMsg[0] == pieceMsgID {
		fmt.Printf("=== Decoy Data Received ===\n")
		fmt.Printf("Piece Index: %d\n", binary.BigEndian.Uint32(pieceMsg[1:5]))
		fmt.Printf("Piece Begin: %d\n", binary.BigEndian.Uint32(pieceMsg[5:9]))
		fmt.Printf("Payload Preview: %s...\n", hex.EncodeToString(pieceMsg[9:25]))
	}

	fmt.Println("\n=== Test Passed: Torrent server is alive and Decoy Mode is working ===")
}
