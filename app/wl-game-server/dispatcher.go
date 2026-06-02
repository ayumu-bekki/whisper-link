package main

import (
	"context"
	"log"
	"sync"
)

type Handler interface {
	Handle(ctx context.Context, item TranscriptionItem, response string, audioData []byte) error
}

type Dispatcher struct {
	handlers map[string]Handler
	mu       sync.RWMutex
	fallback Handler
}

func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		handlers: make(map[string]Handler),
		fallback: &SystemHandler{},
	}
}

func (d *Dispatcher) Register(callsign string, handler Handler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[callsign] = handler
}

// Dispatch は TranscriptionResult の各 item を receiver に対応するハンドラへ渡す。
// 登録されていない receiver は fallback (SystemHandler) で処理する。
func (d *Dispatcher) Dispatch(ctx context.Context, result *TranscriptionResult, response string, audioData []byte) error {
	for _, item := range result.Items {
		d.mu.RLock()
		h, ok := d.handlers[item.Receiver]
		d.mu.RUnlock()

		if !ok {
			h = d.fallback
		}
		if err := h.Handle(ctx, item, response, audioData); err != nil {
			return err
		}
	}
	return nil
}

type SystemHandler struct{}

func (h *SystemHandler) Handle(ctx context.Context, item TranscriptionItem, response string, audioData []byte) error {
	log.Printf("[SYSTEM] sender=%s receiver=%s message=%q response=%q",
		item.Sender, item.Receiver, item.Message, response)
	return nil
}

// S4CAHandler はS4CA宛メッセージをGemini TTSで音声化してradio-bridgeへ送り返す。
type S4CAHandler struct {
	sendCh    chan<- []byte
	ttsClient *TTSClient
}

func NewS4CAHandler(sendCh chan<- []byte, ttsClient *TTSClient) *S4CAHandler {
	return &S4CAHandler{sendCh: sendCh, ttsClient: ttsClient}
}

func (h *S4CAHandler) Handle(ctx context.Context, item TranscriptionItem, response string, audioData []byte) error {
	log.Printf("[S4CA] sender=%s message=%q — generating TTS", item.Sender, item.Message)
	oggData, err := h.ttsClient.GenerateOggOpus(ctx, item.Sender, item.Message)
	if err != nil {
		log.Printf("[S4CA] TTS error: %v", err)
		return nil // エラーでも処理継続
	}
	log.Printf("[S4CA] TTS generated %d bytes, sending", len(oggData))
	select {
	case h.sendCh <- oggData:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// EchoHandler は受信した音声データをそのまま radio-bridge へ送り返す。
type EchoHandler struct {
	sendCh chan<- []byte
}

func NewEchoHandler(sendCh chan<- []byte) *EchoHandler {
	return &EchoHandler{sendCh: sendCh}
}

func (h *EchoHandler) Handle(ctx context.Context, item TranscriptionItem, response string, audioData []byte) error {
	log.Printf("[ECHO] sender=%s receiver=%s message=%q — echoing %d bytes",
		item.Sender, item.Receiver, item.Message, len(audioData))
	select {
	case h.sendCh <- audioData:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}
