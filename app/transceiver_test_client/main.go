package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gordonklaus/portaudio"
	"github.com/hraban/opus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pb "transceiver_test_client/proto"
)

const (
	sampleRate    = 48000
	channels      = 1
	frameSize     = 960  // 20ms @ 48kHz
	framesPerPage = 10   // Oggページあたりのフレーム数 (200ms)
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <host> <port>\n", os.Args[0])
		os.Exit(1)
	}
	addr := fmt.Sprintf("%s:%s", os.Args[1], os.Args[2])

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("failed to connect: %v", err)
	}
	defer conn.Close()

	client := pb.NewTransceiverServiceClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down...")
		cancel()
	}()

	stream, err := client.Connect(ctx)
	if err != nil {
		log.Fatalf("failed to connect stream: %v", err)
	}
	log.Printf("connected to %s", addr)
	log.Println("hold [SPACE] to transmit, Ctrl+C to quit")

	portaudio.Initialize()
	defer portaudio.Terminate()

	// サーバーからの受信→再生
	go receiveAndPlay(stream)

	// スペースキー入力監視→録音→送信
	if err := recordAndSend(ctx, stream); err != nil && err != context.Canceled {
		log.Printf("recordAndSend error: %v", err)
	}

	stream.CloseSend()
}

// receiveAndPlay はサーバーから届くOgg Opusをデコードして再生する。
func receiveAndPlay(stream grpc.BidiStreamingClient[pb.AudioChunk, pb.AudioChunk]) {
	portaudio.Initialize()
	defer portaudio.Terminate()

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			log.Println("server closed stream")
			return
		}
		if err != nil {
			log.Printf("recv error: %v", err)
			return
		}

		go playOggOpus(chunk.OggOpusData)
	}
}

// playOggOpus はOgg Opusバイナリをデコードしてポートオーディオで再生する。
func playOggOpus(data []byte) {
	pcmData, err := decodeOggOpus(data)
	if err != nil {
		log.Printf("decode error: %v", err)
		return
	}
	if len(pcmData) == 0 {
		return
	}

	out := make([]int16, frameSize)
	stream, err := portaudio.OpenDefaultStream(0, channels, float64(sampleRate), frameSize, &out)
	if err != nil {
		log.Printf("portaudio open error: %v", err)
		return
	}
	defer stream.Close()
	stream.Start()
	defer stream.Stop()

	for i := 0; i+frameSize <= len(pcmData); i += frameSize {
		copy(out, pcmData[i:i+frameSize])
		if err := stream.Write(); err != nil {
			log.Printf("portaudio write error: %v", err)
			return
		}
	}
}

// decodeOggOpus はOgg Opusバイナリ全体をデコードしてPCMサンプル列を返す。
func decodeOggOpus(data []byte) ([]int16, error) {
	stream, err := opus.NewStream(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("opus.NewStream: %w", err)
	}
	defer stream.Close()

	var pcm []int16
	buf := make([]int16, frameSize*10)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			pcm = append(pcm, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return pcm, nil
}

// recordAndSend はターミナルのスペースキー押下を監視し、
// 押している間録音してOgg Opusとして送信する。
func recordAndSend(ctx context.Context, stream grpc.BidiStreamingClient[pb.AudioChunk, pb.AudioChunk]) error {
	// ターミナルをrawモードに設定してキー入力を即取得
	oldState, err := makeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("makeRaw: %w", err)
	}
	defer restoreTerminal(int(os.Stdin.Fd()), oldState)

	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppVoIP)
	if err != nil {
		return fmt.Errorf("opus encoder: %w", err)
	}

	keyState := make(chan bool, 8)

	// キー読み取りgoroutine
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				return
			}
			switch buf[0] {
			case ' ':
				keyState <- true
			case 0x03: // Ctrl+C
				return
			default:
				// スペース以外が来たら離したとみなす
				keyState <- false
			}
		}
	}()

	in := make([]int16, frameSize)
	paStream, err := portaudio.OpenDefaultStream(channels, 0, float64(sampleRate), frameSize, &in)
	if err != nil {
		return fmt.Errorf("portaudio input: %w", err)
	}
	defer paStream.Close()

	pressing := false
	var recordBuf []int16
	var opusFrames [][]byte

	log.Println("ready. hold [SPACE] to talk")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pressed := <-keyState:
			if pressed && !pressing {
				// 録音開始
				pressing = true
				recordBuf = nil
				opusFrames = nil
				paStream.Start()
				log.Print("● recording...")
			} else if !pressed && pressing {
				// 録音終了→エンコード→送信
				pressing = false
				paStream.Stop()
				log.Println(" done")

				ogg, err := encodePCMToOggOpus(enc, recordBuf, opusFrames)
				if err != nil {
					log.Printf("encode error: %v", err)
					continue
				}
				if err := stream.Send(&pb.AudioChunk{OggOpusData: ogg}); err != nil {
					return fmt.Errorf("send: %w", err)
				}
				log.Printf("sent %d bytes", len(ogg))
			}
		default:
			if pressing {
				if err := paStream.Read(); err != nil {
					log.Printf("portaudio read error: %v", err)
					continue
				}
				pcmCopy := make([]int16, frameSize)
				copy(pcmCopy, in)
				recordBuf = append(recordBuf, pcmCopy...)

				opusBuf := make([]byte, 4096)
				n, err := enc.Encode(pcmCopy, opusBuf)
				if err != nil {
					log.Printf("opus encode error: %v", err)
					continue
				}
				opusFrames = append(opusFrames, opusBuf[:n])
			}
		}
	}
}

// encodePCMToOggOpus はOpusフレーム列をOgg Opusコンテナにまとめて返す。
func encodePCMToOggOpus(enc *opus.Encoder, _ []int16, frames [][]byte) ([]byte, error) {
	if len(frames) == 0 {
		return nil, fmt.Errorf("no frames recorded")
	}

	w := newOggWriter()

	// OpusHead
	head := buildOpusHead()
	w.writePage(head, 0, true, false)

	// OpusTags
	tags := buildOpusTags()
	w.writePage(tags, 0, false, false)

	// 音声パケット
	granule := uint64(0)
	for _, frame := range frames {
		granule += uint64(frameSize)
		w.writeAudioPacket(frame, granule)
	}
	w.flush()

	return w.bytes(), nil
}
