package main

import (
	"io"
	"log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "radio-bridge-emulator/proto"
)

type transceiverServer struct {
	pb.UnimplementedTransceiverServiceServer
	cfg      AudioConfig
	recorder *recorder
}

func newTransceiverServer(cfg AudioConfig, rec *recorder) *transceiverServer {
	return &transceiverServer{cfg: cfg, recorder: rec}
}

// Connect は双方向ストリーミングRPC。
// クライアントから受信した音声をスピーカーで再生し、
// マイクで録音した音声をクライアントへ送信する。
func (s *transceiverServer) Connect(stream pb.TransceiverService_ConnectServer) error {
	ctx := stream.Context()
	log.Println("[grpc] client connected")

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
				if err := stream.Send(&pb.AudioChunk{OggOpusData: data}); err != nil {
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

		log.Printf("[grpc] received audio from client: %d bytes, playing...", len(chunk.OggOpusData))
		go func(data []byte) {
			if err := playOggOpus(data); err != nil {
				log.Printf("[play] error: %v", err)
			}
		}(chunk.OggOpusData)
	}
}
