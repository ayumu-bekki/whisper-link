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
	registry *SessionRegistry // nil 許容: 未注入時は従来の静的ハンドラのみ
}

func NewDispatcher(registry *SessionRegistry) *Dispatcher {
	return &Dispatcher{
		handlers: make(map[string]Handler),
		fallback: &SystemHandler{},
		registry: registry,
	}
}

func (d *Dispatcher) Register(callsign string, handler Handler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[callsign] = handler
}

// Dispatch は TranscriptionResult の各 item を receiver に対応するハンドラへ渡す。
// まず SessionRegistry で動的払い出しCS宛てを優先解決し、次に静的ハンドラ、最後に fallback。
func (d *Dispatcher) Dispatch(ctx context.Context, result *TranscriptionResult, response string, audioData []byte) error {
	for _, item := range result.Items {
		// 動的払い出しCS宛て: WSセッションのチャットへ流す
		if d.registry != nil {
			if sess, ok := d.registry.Lookup(item.Receiver); ok {
				if err := sess.HandleMessage(ctx, item); err != nil {
					return err
				}
				continue
			}
		}

		// 静的ハンドラ (S4CE/S4CA/S4CQ 等) or fallback
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

	// 回答を冒頭呼び出し + 本文(句点)のチャンクに分割し、全チャンクを並列に TTS 生成する。
	// 各チャンクの生成を同時に走らせることで全体の生成時間を短縮しつつ、結果の PCM は
	// チャンク順に連結して単一の Ogg Opus にまとめ、1 パケットとして送出する
	// (radio-bridge に他プロセスの音声が割り込むのを防ぐため。詳細は streamTTSChunks)。
	chunks := splitAnswerForTTS(answer)
	return streamTTSChunks(ctx, h.ttsClient, h.sendCh, chunks, "[S4CQ]")
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
