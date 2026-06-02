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
	sampleRate = 48000
	channels   = 1
	frameSize  = 960 // 20ms @ 48kHz
)

// recordUntilSilence はマイク入力を監視し、RMS閾値を超えたら録音を開始し、
// 無音が続いたら録音を終了してOgg Opusデータを返す。
// maxDuration を超えた場合もその時点で終了する。
func recordUntilSilence(cfg AudioConfig) ([]byte, error) {
	in := make([]int16, frameSize)
	stream, err := portaudio.OpenDefaultStream(channels, 0, float64(sampleRate), frameSize, &in)
	if err != nil {
		return nil, fmt.Errorf("portaudio open: %w", err)
	}
	defer stream.Close()

	if err := stream.Start(); err != nil {
		return nil, fmt.Errorf("portaudio start: %w", err)
	}
	defer stream.Stop()

	enc, err := hrabanopus.NewEncoder(sampleRate, channels, hrabanopus.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}

	threshold := cfg.InputThresholdRMS
	silenceDuration := time.Duration(cfg.InputSilenceMs) * time.Millisecond
	maxDuration := time.Duration(cfg.InputMaxRecordingSecs) * time.Second

	var opusFrames [][]byte
	recording := false
	var lastAboveThreshold time.Time
	var recordStarted time.Time

	log.Println("[recorder] monitoring input level...")

	for {
		if err := stream.Read(); err != nil {
			return nil, fmt.Errorf("portaudio read: %w", err)
		}

		pcm := make([]int16, frameSize)
		copy(pcm, in)
		rms := computeRMS(pcm)

		if rms >= threshold {
			lastAboveThreshold = time.Now()
			if !recording {
				recording = true
				recordStarted = time.Now()
				opusFrames = nil
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
	return encodeToOggOpus(opusFrames), nil
}

func encodeToOggOpus(frames [][]byte) []byte {
	const preSkip = 312 // Opus standard pre-skip @ 48kHz

	var buf bytes.Buffer
	pw := ogg.NewPacketWriter(&buf, 0x57485052) // "WHPR"

	head := ogg.OpusHead{
		Version:              1,
		Channels:             channels,
		PreSkip:              preSkip,
		InputSampleRate:      sampleRate,
		ChannelMappingFamily: 0,
	}
	headPkt, _ := ogg.BuildOpusHeadPacket(head)
	pw.WritePacket(headPkt, 0, true, false)

	tags := ogg.OpusTags{Vendor: "radio-bridge-emulator"}
	tagsPkt, _ := ogg.BuildOpusTagsPacket(tags)
	pw.WritePacket(tagsPkt, 0, false, false)

	var totalSamples uint64
	for i, frame := range frames {
		totalSamples += uint64(frameSize)
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
	log.Printf("[play] decoded %d samples (%.1f sec)", len(pcmData), float64(len(pcmData))/float64(sampleRate))
	if len(pcmData) == 0 {
		return nil
	}

	out := make([]int16, frameSize)
	stream, err := portaudio.OpenDefaultStream(0, channels, float64(sampleRate), frameSize, &out)
	if err != nil {
		return fmt.Errorf("portaudio open: %w", err)
	}
	defer stream.Close()

	if err := stream.Start(); err != nil {
		return fmt.Errorf("portaudio start: %w", err)
	}
	defer stream.Stop()

	for i := 0; i+frameSize <= len(pcmData); i += frameSize {
		copy(out, pcmData[i:i+frameSize])
		if err := stream.Write(); err != nil {
			return fmt.Errorf("portaudio write: %w", err)
		}
	}
	return nil
}

func decodeOggOpus(data []byte) ([]int16, error) {
	s, err := hrabanopus.NewStream(bytes.NewReader(data))
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
		if err != nil {
			break
		}
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
