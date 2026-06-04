package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pb "wl-game-server/gen"
)

type BridgeClient struct {
	cfg        RadioBridgeConfig
	processor  *GeminiProcessor
	dispatcher *Dispatcher
	sendCh     chan outgoingAudio
}

func NewBridgeClient(cfg RadioBridgeConfig, processor *GeminiProcessor, dispatcher *Dispatcher, sendCh chan outgoingAudio) *BridgeClient {
	return &BridgeClient{
		cfg:        cfg,
		processor:  processor,
		dispatcher: dispatcher,
		sendCh:     sendCh,
	}
}

func (c *BridgeClient) Run(ctx context.Context) error {
	attempts := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err := c.connect(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[bridge] connection error: %v", err)
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		attempts++
		if c.cfg.MaxReconnectAttempts > 0 && attempts >= c.cfg.MaxReconnectAttempts {
			return fmt.Errorf("max reconnect attempts (%d) reached", c.cfg.MaxReconnectAttempts)
		}

		interval := time.Duration(c.cfg.ReconnectIntervalSecs) * time.Second
		log.Printf("[bridge] reconnecting in %s (attempt %d)...", interval, attempts+1)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (c *BridgeClient) connect(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", c.cfg.Host, c.cfg.Port)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("grpc.NewClient: %w", err)
	}
	defer conn.Close()

	client := pb.NewTransceiverServiceClient(conn)
	stream, err := client.Connect(ctx)
	if err != nil {
		return fmt.Errorf("client.Connect: %w", err)
	}
	log.Printf("[bridge] connected to %s", addr)

	// このセッション専用のキャンセルで send goroutine を管理する
	sendCtx, cancelSend := context.WithCancel(ctx)
	defer cancelSend()

	// sendCh に積まれた音声データを stream.Send で送信する goroutine
	go func() {
		for {
			select {
			case out := <-c.sendCh:
				if err := stream.Send(&pb.AudioChunk{
					OggOpusData: out.Data,
					Status:      out.Status,
					StreamId:    out.StreamID,
				}); err != nil {
					log.Printf("[bridge] stream.Send error: %v", err)
					return
				}
				log.Printf("[bridge] sent audio: %d bytes (status=%s stream_id=%s)",
					len(out.Data), out.Status, out.StreamID)
			case <-sendCtx.Done():
				return
			}
		}
	}()

	// 受信チャンクをシリアルに処理する（並列Transcribe/TTSによるキュー溢れを防ぐ）
	audioCh := make(chan []byte, 1)
	go func() {
		for {
			select {
			case data := <-audioCh:
				c.handleAudio(ctx, data)
			case <-sendCtx.Done():
				return
			}
		}
	}()

	for {
		chunk, err := stream.Recv()
		if err == io.EOF || ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stream.Recv: %w", err)
		}

		log.Printf("[bridge] received audio chunk: %d bytes", len(chunk.OggOpusData))
		select {
		case audioCh <- chunk.OggOpusData:
		case <-ctx.Done():
			return nil
		}
	}
}

func (c *BridgeClient) handleAudio(ctx context.Context, data []byte) {
	result, err := c.processor.Transcribe(ctx, data)
	if err != nil {
		log.Printf("[audio] transcribe error: %v", err)
		return
	}
	for _, item := range result.Items {
		log.Printf("[audio] transcribed: sender=%s receiver=%s message=%q",
			item.Sender, item.Receiver, item.Message)
	}

	response, err := c.processor.Reason(ctx, result)
	if err != nil {
		log.Printf("[audio] reason error: %v", err)
		return
	}

	if err := c.dispatcher.Dispatch(ctx, result, response, data); err != nil {
		log.Printf("[audio] dispatch error: %v", err)
	}
}
