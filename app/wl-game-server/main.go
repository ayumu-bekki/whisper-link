package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
)

const defaultWSListenAddr = ":8080"

func main() {
	configPath := "config.toml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("LoadConfig: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down...")
		cancel()
	}()

	processor, err := NewGeminiProcessor(ctx, cfg.Gemini)
	if err != nil {
		log.Fatalf("NewGeminiProcessor: %v", err)
	}
	defer processor.Close()

	ttsClient, err := NewTTSClient(ctx, cfg.Gemini.APIKey, cfg.Gemini.TTSModel)
	if err != nil {
		log.Fatalf("NewTTSClient: %v", err)
	}

	sendCh := make(chan outgoingAudio, 16)

	registry := NewSessionRegistry()

	// 全接続で共有する会話ログ（max=0: 無制限）と NPC ペルソナのシナリオ。
	sharedLog := NewConversationLog(0)
	scenario := defaultScenario()

	dispatcher := NewDispatcher(registry, sharedLog)
	dispatcher.Register("S4CE", NewEchoHandler(sendCh))
	dispatcher.Register("S4CA", NewS4CAHandler(sendCh, ttsClient))
	dispatcher.Register("S4CQ", NewS4CQHandler(sendCh, ttsClient, processor))

	callsigns := NewCallsignManager("S4")

	wsAddr := cfg.WebSocket.ListenAddr
	if wsAddr == "" {
		wsAddr = defaultWSListenAddr
	}
	wsServer := NewWSServer(callsigns, registry, processor, ttsClient, sendCh, sharedLog, scenario)
	go func() {
		if err := wsServer.Run(ctx, wsAddr); err != nil {
			log.Printf("WSServer.Run: %v", err)
		}
	}()

	client := NewBridgeClient(cfg.RadioBridge, processor, dispatcher, sendCh)

	if err := client.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("BridgeClient.Run: %v", err)
	}
}
