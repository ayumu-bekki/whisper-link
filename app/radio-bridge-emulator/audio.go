package main

import (
	"bytes"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/gordonklaus/portaudio"
	hrabanopus "github.com/hraban/opus"
	"github.com/kazzmir/opus-go/ogg"
)

const (
	alsaSampleRate = 48000 // portaudioデバイスは48kHz固定
	opusSampleRate = 24000 // Opusエンコード/デコードは24kHz
	opusBitrate    = 16000 // 16kbps
	channels       = 1
	alsaFrameSize  = 960 // 20ms @ 48kHz (portaudio入出力)
	opusFrameSize  = 480 // 20ms @ 24kHz (Opusエンコード)
)

// recordUntilSilence はマイク入力を監視し、RMS閾値を超えたら録音を開始し、
// 無音が続いたら録音を終了してOgg Opusデータを返す。
// maxDuration を超えた場合もその時点で終了する。
func recordUntilSilence(cfg AudioConfig) ([]byte, error) {
	in := make([]int16, alsaFrameSize)
	stream, err := portaudio.OpenDefaultStream(channels, 0, float64(alsaSampleRate), alsaFrameSize, &in)
	if err != nil {
		return nil, fmt.Errorf("portaudio open: %w", err)
	}
	defer stream.Close()

	if err := stream.Start(); err != nil {
		return nil, fmt.Errorf("portaudio start: %w", err)
	}
	defer stream.Stop()

	enc, err := hrabanopus.NewEncoder(opusSampleRate, channels, hrabanopus.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}
	if err := enc.SetBitrate(opusBitrate); err != nil {
		return nil, fmt.Errorf("opus set bitrate: %w", err)
	}

	threshold := cfg.InputThresholdRMS
	silenceDuration := time.Duration(cfg.InputSilenceMs) * time.Millisecond
	maxDuration := time.Duration(cfg.InputMaxRecordingSecs) * time.Second

	var opusFrames [][]byte
	var frameRMS []uint16
	recording := false
	var lastAboveThreshold time.Time
	var recordStarted time.Time

	log.Println("[recorder] monitoring input level...")

	for {
		if err := stream.Read(); err != nil {
			return nil, fmt.Errorf("portaudio read: %w", err)
		}

		// 48kHz → 24kHz ダウンサンプル (2サンプル平均)
		pcm48 := make([]int16, alsaFrameSize)
		copy(pcm48, in)
		pcm := make([]int16, opusFrameSize)
		for i := range pcm {
			pcm[i] = int16((int32(pcm48[i*2]) + int32(pcm48[i*2+1])) / 2)
		}
		rms := computeRMS(pcm)

		if rms >= threshold {
			lastAboveThreshold = time.Now()
			if !recording {
				recording = true
				recordStarted = time.Now()
				opusFrames = nil
				frameRMS = nil
				log.Printf("[recorder] recording started (rms=%d)", rms)
			}
		}

		if recording {
			opusBuf := make([]byte, 4096)
			n, err := enc.Encode(pcm, opusBuf)
			if err != nil {
				log.Printf("[recorder] encode error: %v", err)
				continue
			}
			opusFrames = append(opusFrames, opusBuf[:n])
			frameRMS = append(frameRMS, rms)

			if time.Since(recordStarted) >= maxDuration {
				log.Printf("[recorder] max duration reached, flushing")
				break
			}

			if !lastAboveThreshold.IsZero() && time.Since(lastAboveThreshold) >= silenceDuration {
				log.Printf("[recorder] silence detected, flushing (%d frames)", len(opusFrames))
				break
			}
		}
	}

	if len(opusFrames) == 0 {
		return nil, nil
	}

	// 末尾の無音フレームをトリミング。
	// 最後に閾値を超えたフレーム（=語尾）を探し、その後ろに TRAIL_FRAMES 分の余白を残す。
	const trailFrames = 1 // 20ms 余白
	lastSpeech := -1
	for i := len(opusFrames) - 1; i >= 0; i-- {
		if frameRMS[i] >= threshold {
			lastSpeech = i
			break
		}
	}
	keepFrames := len(opusFrames) // 全フレーム閾値未満なら削らない
	if lastSpeech >= 0 {
		keepFrames = lastSpeech + 1 + trailFrames
		if keepFrames > len(opusFrames) {
			keepFrames = len(opusFrames)
		}
	}
	trimmed := opusFrames[:keepFrames]
	log.Printf("[recorder] trimmed %d → %d frames", len(opusFrames), len(trimmed))

	// 最小録音時間チェック (opusFrameSize サンプル/フレーム @ opusSampleRate)
	durationMs := uint64(len(trimmed)) * uint64(opusFrameSize) * 1000 / uint64(opusSampleRate)
	minMs := cfg.InputMinRecordingMs
	if minMs == 0 {
		minMs = 800 // デフォルト値
	}
	if durationMs < minMs {
		log.Printf("[recorder] too short (%dms < %dms), discarding", durationMs, minMs)
		return nil, nil
	}

	return encodeToOggOpus(trimmed), nil
}

func encodeToOggOpus(frames [][]byte) []byte {
	const preSkip = 312 // Opus standard pre-skip

	var buf bytes.Buffer
	pw := ogg.NewPacketWriter(&buf, 0x57485052) // "WHPR"

	head := ogg.OpusHead{
		Version:              1,
		Channels:             channels,
		PreSkip:              preSkip,
		InputSampleRate:      opusSampleRate,
		ChannelMappingFamily: 0,
	}
	headPkt, _ := ogg.BuildOpusHeadPacket(head)
	pw.WritePacket(headPkt, 0, true, false)

	tags := ogg.OpusTags{Vendor: "radio-bridge-emulator"}
	tagsPkt, _ := ogg.BuildOpusTagsPacket(tags)
	pw.WritePacket(tagsPkt, 0, false, false)

	var totalSamples uint64
	for i, frame := range frames {
		totalSamples += uint64(opusFrameSize)
		granule := uint64(preSkip) + totalSamples
		isLast := i == len(frames)-1
		pw.WritePacket(frame, granule, false, isLast)
	}
	pw.Flush()
	return buf.Bytes()
}

// playOggOpus はOgg Opusデータをデコードしてデフォルト出力デバイスで再生する。
func playOggOpus(data []byte) error {
	pcmData, err := decodeOggOpus(data)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	log.Printf("[play] decoded %d samples (%.1f sec)", len(pcmData), float64(len(pcmData))/float64(opusSampleRate))
	if len(pcmData) == 0 {
		return nil
	}

	// 24kHz → 48kHz アップサンプル (各サンプルを2回繰り返す)
	upsampled := make([]int16, len(pcmData)*2)
	for i, s := range pcmData {
		upsampled[i*2] = s
		upsampled[i*2+1] = s
	}

	out := make([]int16, alsaFrameSize)
	stream, err := portaudio.OpenDefaultStream(0, channels, float64(alsaSampleRate), alsaFrameSize, &out)
	if err != nil {
		return fmt.Errorf("portaudio open: %w", err)
	}
	defer stream.Close()

	if err := stream.Start(); err != nil {
		return fmt.Errorf("portaudio start: %w", err)
	}
	defer stream.Stop()

	for i := 0; i+alsaFrameSize <= len(upsampled); i += alsaFrameSize {
		copy(out, upsampled[i:i+alsaFrameSize])
		if err := stream.Write(); err != nil {
			return fmt.Errorf("portaudio write: %w", err)
		}
	}
	return nil
}

func decodeOggOpus(data []byte) ([]int16, error) {
	dec, err := hrabanopus.NewDecoder(opusSampleRate, channels)
	if err != nil {
		return nil, fmt.Errorf("opus.NewDecoder: %w", err)
	}

	r := ogg.NewPacketReader(bytes.NewReader(data))

	var pcm []int16
	buf := make([]int16, opusFrameSize*10)
	headerPackets := 0
	for {
		pkt, err := r.ReadPacket()
		if err != nil {
			break
		}
		// 最初の2パケットはOpusHead / OpusTags
		if headerPackets < 2 {
			headerPackets++
			continue
		}
		n, err := dec.Decode(pkt.Data, buf)
		if err != nil {
			log.Printf("[decode] packet decode error: %v", err)
			continue
		}
		pcm = append(pcm, buf[:n]...)
	}
	return pcm, nil
}

func computeRMS(samples []int16) uint16 {
	if len(samples) == 0 {
		return 0
	}
	var sumSq float64
	for _, s := range samples {
		sumSq += float64(s) * float64(s)
	}
	rms := math.Sqrt(sumSq / float64(len(samples)))
	if rms > math.MaxUint16 {
		return math.MaxUint16
	}
	return uint16(rms)
}
