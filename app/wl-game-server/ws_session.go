package main

import (
	"encoding/json"
	"log"

	"github.com/gorilla/websocket"
	"context"
)

// クライアントから送られるコマンドの共通構造
type wsCommand struct {
	Type string `json:"type"`
}

// "login" コマンド
type wsLoginCommand struct {
	Type string `json:"type"`
}

// サーバーからクライアントへのレスポンス
type wsResponse struct {
	Type     string `json:"type"`
	Callsign string `json:"callsign,omitempty"`
	Error    string `json:"error,omitempty"`
}

// wsSession は1つの WebSocket 接続のセッション状態を保持する。
type wsSession struct {
	conn      *websocket.Conn
	callsigns *CallsignManager
	callsign  string // 発行済みコールサイン ("" = 未ログイン)
}

func newWSSession(conn *websocket.Conn, callsigns *CallsignManager) *wsSession {
	return &wsSession{conn: conn, callsigns: callsigns}
}

func (s *wsSession) run(ctx context.Context) {
	defer func() {
		if s.callsign != "" {
			s.callsigns.Release(s.callsign)
			log.Printf("[WS] callsign released: %s", s.callsign)
		}
		s.conn.Close()
	}()

	for {
		_, msg, err := s.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("[WS] read error: %v", err)
			}
			return
		}

		var cmd wsCommand
		if err := json.Unmarshal(msg, &cmd); err != nil {
			s.sendError("invalid JSON")
			continue
		}

		switch cmd.Type {
		case "login":
			s.handleLogin()
		default:
			s.sendError("unknown command type: " + cmd.Type)
		}

		// ctx がキャンセルされたら終了
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func (s *wsSession) handleLogin() {
	if s.callsign != "" {
		// すでにログイン済み: 現在のコールサインをそのまま返す
		s.sendJSON(wsResponse{Type: "login", Callsign: s.callsign})
		return
	}

	cs, ok := s.callsigns.Issue()
	if !ok {
		s.sendError("no callsign available")
		return
	}
	s.callsign = cs
	log.Printf("[WS] callsign issued: %s", cs)
	s.sendJSON(wsResponse{Type: "login", Callsign: cs})
}

func (s *wsSession) sendError(msg string) {
	s.sendJSON(wsResponse{Type: "error", Error: msg})
}

func (s *wsSession) sendJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("[WS] marshal error: %v", err)
		return
	}
	if err := s.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("[WS] write error: %v", err)
	}
}
