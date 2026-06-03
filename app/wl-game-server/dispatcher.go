package main

import (
	"context"
	"fmt"
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

// S4CQHandler はS4CQ宛メッセージをGoogle検索付きGeminiで回答し、TTSで音声化して返す。
type S4CQHandler struct {
	sendCh    chan<- []byte
	ttsClient *TTSClient
	processor *GeminiProcessor
}

func NewS4CQHandler(sendCh chan<- []byte, ttsClient *TTSClient, processor *GeminiProcessor) *S4CQHandler {
	return &S4CQHandler{sendCh: sendCh, ttsClient: ttsClient, processor: processor}
}

func (h *S4CQHandler) Handle(ctx context.Context, item TranscriptionItem, response string, audioData []byte) error {
	log.Printf("[S4CQ] sender=%s message=%q — asking Gemini", item.Sender, item.Message)

	answer, err := h.processor.Ask(ctx, item.Sender, item.Receiver, item.Message)
	if err != nil {
		log.Printf("[S4CQ] Ask error: %v", err)
		return nil
	}
	log.Printf("[S4CQ] answer: %s", answer)

	// 回答を冒頭呼び出し + 本文(句点)のチャンクに分割し、逐次 TTS して
	// できた順に送出する。最初の短い冒頭チャンクが先に届くことで、radio-bridge が
	// 全文の TTS 完了を待たずに再生を開始でき、初音までの体感レイテンシを短縮する。
	chunks := splitAnswerForTTS(answer)
	for i, chunk := range chunks {
		ttsPrompt := fmt.Sprintf(ttsPromptTemplateS4CQ, chunk)
		oggData, err := h.ttsClient.GenerateOggOpusFromPrompt(ctx, ttsPrompt)
		if err != nil {
			log.Printf("[S4CQ] TTS error (chunk %d/%d): %v", i+1, len(chunks), err)
			continue // 1 チャンクの失敗で全体を止めない
		}
		log.Printf("[S4CQ] TTS generated %d bytes (chunk %d/%d), sending", len(oggData), i+1, len(chunks))

		select {
		case h.sendCh <- oggData:
		case <-ctx.Done():
			return ctx.Err()
		}
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
