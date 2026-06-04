package main

import (
	"context"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// WSServer は WebSocket 接続を受け付け、セッションを管理する。
type WSServer struct {
	callsigns *CallsignManager
	registry  *SessionRegistry
	processor *GeminiProcessor
	ttsClient *TTSClient
	sendCh    chan<- outgoingAudio
	sharedLog   *ConversationLog
	scenario  Scenario
	ctx       context.Context
}

func NewWSServer(
	callsigns *CallsignManager,
	registry *SessionRegistry,
	processor *GeminiProcessor,
	ttsClient *TTSClient,
	sendCh chan<- outgoingAudio,
	sharedLog *ConversationLog,
	scenario Scenario,
) *WSServer {
	return &WSServer{
		callsigns: callsigns,
		registry:  registry,
		processor: processor,
		ttsClient: ttsClient,
		sendCh:    sendCh,
		sharedLog:   sharedLog,
		scenario:  scenario,
	}
}

// ServeHTTP は http.Handler として WebSocket アップグレードを処理する。
func (s *WSServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS] upgrade error: %v", err)
		return
	}
	session := newWSSession(conn, s.callsigns, s.registry, s.processor, s.ttsClient, s.sendCh, s.sharedLog, s.scenario)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[WS] session panic: %v", rec)
			}
		}()
		session.run(s.ctx)
	}()
}

// Run は HTTP サーバーを起動し、ctx がキャンセルされたら停止する。
func (s *WSServer) Run(ctx context.Context, addr string) error {
	s.ctx = ctx
	mux := http.NewServeMux()
	mux.Handle("/ws", s)

	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	log.Printf("[WS] listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
