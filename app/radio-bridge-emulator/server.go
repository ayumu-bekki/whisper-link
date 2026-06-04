package main

import (
	"fmt"
	"io"
	"log"
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "radio-bridge-emulator/proto"
)

type transceiverServer struct {
	pb.UnimplementedTransceiverServiceServer
	cfg      AudioConfig
	recorder *recorder
	player   *player
}

func newTransceiverServer(cfg AudioConfig, rec *recorder, pl *player) *transceiverServer {
	return &transceiverServer{cfg: cfg, recorder: rec, player: pl}
}

// Connect は双方向ストリーミングRPC。
// クライアントから受信した音声をスピーカーで再生し、
// マイクで録音した音声をクライアントへ送信する。
func (s *transceiverServer) Connect(stream pb.TransceiverService_ConnectServer) error {
	ctx := stream.Context()
	log.Println("[grpc] client connected")

	// ONESHOT チャンクに振る接続スコープの連番。
	// peer は取れないため接続識別は不要; カウンタだけで内部 ID の一意性を確保する。
	var oneshotCounter atomic.Uint64

	// マイク録音 → クライアント送信
	sub := s.recorder.subscribe()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case data, ok := <-sub:
				if !ok {
					return
				}
				log.Printf("[grpc] sending audio to client: %d bytes", len(data))
				// マイク受信音声は単発再生扱い。
				if err := stream.Send(&pb.AudioChunk{
					OggOpusData: data,
					Status:      pb.StreamStatus_ONESHOT,
				}); err != nil {
					log.Printf("[grpc] send error: %v", err)
					return
				}
			}
		}
	}()

	// クライアント → 受信して再生
	for {
		chunk, err := stream.Recv()
		if err == io.EOF || ctx.Err() != nil {
			log.Println("[grpc] client disconnected")
			return nil
		}
		if err != nil {
			st, _ := status.FromError(err)
			if st.Code() == codes.Canceled {
				log.Println("[grpc] client disconnected")
				return nil
			}
			log.Printf("[grpc] recv error: %v", err)
			return err
		}

		log.Printf("[grpc] received audio from client: %d bytes (status=%s stream_id=%s)",
			len(chunk.OggOpusData), chunk.Status, chunk.StreamId)

		// ONESHOT は stream_id を内部生成する (空文字列をキーにしない)。
		// stream 系 (START/CONTINUE/END) で stream_id が空なら破棄する。
		var streamID string
		switch chunk.Status {
		case pb.StreamStatus_ONESHOT:
			n := oneshotCounter.Add(1)
			streamID = fmt.Sprintf("__oneshot_%d", n)
		case pb.StreamStatus_START, pb.StreamStatus_CONTINUE, pb.StreamStatus_END:
			if chunk.StreamId == "" {
				log.Printf("[grpc] audio discarded: stream_id is empty for stream chunk (status=%s)", chunk.Status)
				continue
			}
			streamID = chunk.StreamId
		default:
			// UNKNOWN(0) / 未知値は player.push 内でも破棄されるが、ここで先に弾く。
			log.Printf("[grpc] audio discarded: invalid status (raw=%d)", int32(chunk.Status))
			continue
		}

		s.player.push(chunk.OggOpusData, chunk.Status, streamID)
	}
}
