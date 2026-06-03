package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type wsCommand struct {
	Type string `json:"type"`
}

type wsResponse struct {
	Type     string `json:"type"`
	Callsign string `json:"callsign,omitempty"`
	Error    string `json:"error,omitempty"`
}

func main() {
	addr := flag.String("addr", "ws://192.168.100.18:8080/ws", "WebSocket サーバーアドレス")
	flag.Parse()

	log.Printf("接続先: %s", *addr)

	conn, _, err := websocket.DefaultDialer.Dial(*addr, nil)
	if err != nil {
		log.Fatalf("接続失敗: %v", err)
	}
	defer conn.Close()

	log.Println("接続成功。コマンドを入力してください。")
	printHelp()

	// 受信ゴルーチン
	recvDone := make(chan struct{})
	go func() {
		defer close(recvDone)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
					log.Printf("受信エラー: %v", err)
				} else {
					log.Println("サーバーとの接続が閉じられました。")
				}
				return
			}
			var resp wsResponse
			if err := json.Unmarshal(msg, &resp); err != nil {
				fmt.Printf("<< (raw) %s\n", msg)
				continue
			}
			switch resp.Type {
			case "login":
				fmt.Printf("<< [login] コールサイン: %s\n", resp.Callsign)
			case "error":
				fmt.Printf("<< [error] %s\n", resp.Error)
			default:
				fmt.Printf("<< [%s] %s\n", resp.Type, string(msg))
			}
		}
	}()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		switch line {
		case "login", "l":
			send(conn, wsCommand{Type: "login"})

		case "quit", "q", "exit":
			conn.WriteMessage(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"),
			)
			select {
			case <-recvDone:
			case <-time.After(2 * time.Second):
			}
			return

		case "help", "h", "?":
			printHelp()

		default:
			// 生JSONをそのまま送信
			if strings.HasPrefix(line, "{") {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(line)); err != nil {
					log.Printf("送信エラー: %v", err)
				}
				fmt.Printf(">> %s\n", line)
			} else {
				fmt.Println("不明なコマンドです。help で使い方を確認してください。")
			}
		}

		select {
		case <-recvDone:
			return
		default:
		}
	}

	// EOF (Ctrl+D) の場合も正常クローズ
	conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"),
	)
	select {
	case <-recvDone:
	case <-time.After(2 * time.Second):
	}
}

func send(conn *websocket.Conn, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("marshal エラー: %v", err)
		return
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("送信エラー: %v", err)
		return
	}
	fmt.Printf(">> %s\n", data)
}

func printHelp() {
	fmt.Println("コマンド一覧:")
	fmt.Println("  login (l)    コールサインを発行する")
	fmt.Println("  {\"type\":...} 生JSONを送信する")
	fmt.Println("  quit  (q)    接続を閉じて終了する")
	fmt.Println("  help  (h)    このヘルプを表示する")
}
