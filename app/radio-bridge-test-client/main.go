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
	"time"

	"github.com/gordonklaus/portaudio"
	"github.com/hraban/opus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pb "radio-bridge-test-client/proto"
)

const (
	sampleRate = 48000
	channels   = 1
	frameSize  = 960 // 20ms @ 48kHz
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

	// OS シグナル (SIGINT/SIGTERM) でキャンセル
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		printLine("shutting down...")
		cancel()
	}()

	stream, err := client.Connect(ctx)
	if err != nil {
		log.Fatalf("failed to connect stream: %v", err)
	}
	printLine(fmt.Sprintf("connected to %s", addr))

	if err := portaudio.Initialize(); err != nil {
		log.Fatalf("portaudio init: %v", err)
	}
	defer portaudio.Terminate()

	// サーバーからの受信→再生
	go receiveAndPlay(ctx, stream)

	// スペースキー入力監視→録音→送信
	if err := recordAndSend(ctx, cancel, stream); err != nil && err != context.Canceled {
		printLine(fmt.Sprintf("recordAndSend error: %v", err))
	}

	stream.CloseSend()
}

// receiveAndPlay はサーバーから届くOgg Opusをデコードして再生する。
func receiveAndPlay(ctx context.Context, stream grpc.BidiStreamingClient[pb.AudioChunk, pb.AudioChunk]) {
	for {
		chunk, err := stream.Recv()
		if err == io.EOF || ctx.Err() != nil {
			return
		}
		if err != nil {
			printLine(fmt.Sprintf("[recv] error: %v", err))
			return
		}
		printLine(fmt.Sprintf("[recv] received %d bytes, playing...", len(chunk.OggOpusData)))
		go playOggOpus(chunk.OggOpusData)
	}
}

// playOggOpus はOgg Opusバイナリをデコードしてポートオーディオで再生する。
func playOggOpus(data []byte) {
	pcmData, err := decodeOggOpus(data)
	if err != nil {
		printLine(fmt.Sprintf("[play] decode error: %v", err))
		return
	}
	if len(pcmData) == 0 {
		printLine("[play] decoded 0 samples, skipping")
		return
	}

	out := make([]int16, frameSize)
	paStream, err := portaudio.OpenDefaultStream(0, channels, float64(sampleRate), frameSize, &out)
	if err != nil {
		printLine(fmt.Sprintf("[play] portaudio open error: %v", err))
		return
	}
	defer paStream.Close()

	if err := paStream.Start(); err != nil {
		printLine(fmt.Sprintf("[play] portaudio start error: %v", err))
		return
	}
	defer paStream.Stop()

	for i := 0; i+frameSize <= len(pcmData); i += frameSize {
		copy(out, pcmData[i:i+frameSize])
		if err := paStream.Write(); err != nil {
			printLine(fmt.Sprintf("[play] portaudio write error: %v", err))
			return
		}
	}
	printLine(fmt.Sprintf("[play] playback done (%d samples)", len(pcmData)))
}

// decodeOggOpus はOgg Opusバイナリ全体をデコードしてPCMサンプル列を返す。
func decodeOggOpus(data []byte) ([]int16, error) {
	s, err := opus.NewStream(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("opus.NewStream: %w", err)
	}
	defer s.Close()

	var pcm []int16
	buf := make([]int16, frameSize*10)
	for {
		n, err := s.Read(buf)
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

// recordAndSend はスペースキーのトグル操作で録音・送信を制御する。
// スペースを押すと録音開始、もう一度押すと録音停止して送信する。
func recordAndSend(ctx context.Context, cancel context.CancelFunc, stream grpc.BidiStreamingClient[pb.AudioChunk, pb.AudioChunk]) error {
	oldState, err := makeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("makeRaw: %w", err)
	}
	defer restoreTerminal(int(os.Stdin.Fd()), oldState)

	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppVoIP)
	if err != nil {
		return fmt.Errorf("opus encoder: %w", err)
	}

	in := make([]int16, frameSize)
	paStream, err := portaudio.OpenDefaultStream(channels, 0, float64(sampleRate), frameSize, &in)
	if err != nil {
		return fmt.Errorf("portaudio input: %w", err)
	}
	defer paStream.Close()

	// キーイベント: true=スペース押下, false=Ctrl+C
	keyEvent := make(chan bool, 4)
	go readKeys(keyEvent, cancel)

	recording := false
	var opusFrames [][]byte
	var recordStart time.Time
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	printLine("ready. press [SPACE] to start/stop recording, Ctrl+C to quit")

	for {
		select {
		case <-ctx.Done():
			if recording {
				paStream.Stop()
			}
			return ctx.Err()

		case <-keyEvent:
			// スペースキーでトグル
			if !recording {
				// 録音開始
				recording = true
				opusFrames = nil
				recordStart = time.Now()
				if err := paStream.Start(); err != nil {
					printLine(fmt.Sprintf("[rec] portaudio start error: %v", err))
					recording = false
					continue
				}
				printLine("[REC] recording...")

			} else {
				// 録音停止
				recording = false
				paStream.Stop()
				elapsed := time.Since(recordStart)
				printLine(fmt.Sprintf("[STOP] recorded %.1fs (%d frames)", elapsed.Seconds(), len(opusFrames)))

				if len(opusFrames) == 0 {
					printLine("[send] no frames, skipping")
					continue
				}

				ogg, err := encodePCMToOggOpus(opusFrames)
				if err != nil {
					printLine(fmt.Sprintf("[send] encode error: %v", err))
					continue
				}

				printLine(fmt.Sprintf("[send] sending %d bytes...", len(ogg)))
				if err := stream.Send(&pb.AudioChunk{
					OggOpusData: ogg,
					Status:      pb.StreamStatus_ONESHOT,
				}); err != nil {
					return fmt.Errorf("[send] stream send error: %w", err)
				}
				printLine("[send] sent OK")
				fmt.Println("ready. press [SPACE] to start/stop recording, Ctrl+C to quit")
			}

		case <-ticker.C:
			// 録音中は20msごとにPCMを読み取ってOpusエンコード
			if !recording {
				continue
			}
			if err := paStream.Read(); err != nil {
				printLine(fmt.Sprintf("[rec] portaudio read error: %v", err))
				continue
			}
			pcmCopy := make([]int16, frameSize)
			copy(pcmCopy, in)

			opusBuf := make([]byte, 4096)
			n, err := enc.Encode(pcmCopy, opusBuf)
			if err != nil {
				printLine(fmt.Sprintf("[rec] opus encode error: %v", err))
				continue
			}
			opusFrames = append(opusFrames, opusBuf[:n])

			elapsed := time.Since(recordStart)
			printStatus(fmt.Sprintf("[REC] %.1fs (%d frames)", elapsed.Seconds(), len(opusFrames)))
		}
	}
}

// readKeys はstdinを1バイトずつ読んでキーイベントを送る。
// スペース → keyEvent に true を送る
// Ctrl+C   → cancel を呼ぶ
func readKeys(keyEvent chan<- bool, cancel context.CancelFunc) {
	buf := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return
		}
		switch buf[0] {
		case ' ':
			// 連打を避けるため非ブロッキング送信
			select {
			case keyEvent <- true:
			default:
			}
		case 0x03: // Ctrl+C
			cancel()
			return
		}
	}
}

// encodePCMToOggOpus はOpusフレーム列をOgg Opusコンテナにまとめて返す。
func encodePCMToOggOpus(frames [][]byte) ([]byte, error) {
	if len(frames) == 0 {
		return nil, fmt.Errorf("no frames recorded")
	}

	w := newOggWriter()
	w.writePage(buildOpusHead(), 0, true, false)
	w.writePage(buildOpusTags(), 0, false, false)

	granule := uint64(0)
	for _, frame := range frames {
		granule += uint64(frameSize)
		w.writeAudioPacket(frame, granule)
	}
	w.flush()

	return w.bytes(), nil
}

// printLine は現在行をクリアして1行出力する。
func printLine(s string) {
	fmt.Printf("\r\033[2K%s\n", s)
}

// printStatus は現在行をクリアしてステータスを上書き表示する (改行しない)。
// 録音中のリアルタイム表示に使う。
func printStatus(s string) {
	fmt.Printf("\r\033[2K%s", s)
}
