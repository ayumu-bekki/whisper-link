package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"

	hrabanopus "github.com/hraban/opus"
	"github.com/kazzmir/opus-go/ogg"
	"github.com/zeozeozeo/gomplerate"
	"google.golang.org/genai"
)

const (
	ttsInputSampleRate  = 24000 // Gemini TTS出力: 24kHz
	ttsOutputSampleRate = 48000 // Opusエンコード: 48kHz
	sampleRate          = ttsOutputSampleRate
	channels            = 1
	frameSize           = 960 // 20ms @ 48kHz
)

const defaultTTSModel = "gemini-3.1-flash-tts-preview"

const ttsPromptTemplate = `# AUDIO PROFILE: 無線オペレーターA
## "無線でも聞き取りやすく発声する熟練オペレーター"

## THE SCENE: 無線のテストの相手をしている
無線の初心者に対して、相手が話した内容と同じ内容を返答するテストオペレーションをしています。

### DIRECTOR'S NOTES
Style: このオペレーターは、無線の初心者に対してサポートをしています。
Pacing: FM変調がかかった状態でも聞き取りやすく的確に、少し遅いスピードで話します。
Accent: 日本国内で無線を運用していたので、コールサインを話す際には日本語の訛りがあります。

#### TRANSCRIPT
%s こちらS4CA。%s`

// TTSClient は Gemini TTS クライアントを保持する。
type TTSClient struct {
	client *genai.Client
	model  string
}

func NewTTSClient(ctx context.Context, apiKey, model string) (*TTSClient, error) {
	if model == "" {
		model = defaultTTSModel
	}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: apiKey})
	if err != nil {
		return nil, fmt.Errorf("genai.NewClient: %w", err)
	}
	return &TTSClient{client: client, model: model}, nil
}

// GenerateOggOpus はsender/messageからTTS音声を生成してOgg Opusで返す。
func (t *TTSClient) GenerateOggOpus(ctx context.Context, sender, message string) ([]byte, error) {
	prompt := fmt.Sprintf(ttsPromptTemplate, sender, message)

	resp, err := t.client.Models.GenerateContent(ctx, t.model,
		[]*genai.Content{
			genai.NewContentFromText(prompt, genai.RoleUser),
		},
		&genai.GenerateContentConfig{
			ResponseModalities: []string{"audio"},
			SpeechConfig: &genai.SpeechConfig{
				VoiceConfig: &genai.VoiceConfig{
					PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{
						VoiceName: "Achernar",
					},
				},
			},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("GenerateContent: %w", err)
	}

	if len(resp.Candidates) == 0 ||
		resp.Candidates[0].Content == nil ||
		len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("empty TTS response")
	}

	blob := resp.Candidates[0].Content.Parts[0].InlineData
	if blob == nil {
		return nil, fmt.Errorf("no inline audio data in TTS response")
	}

	pcmData := stripWAVHeader(blob.Data)

	pcm24k, err := parsePCM16(pcmData)
	if err != nil {
		return nil, fmt.Errorf("parsePCM16: %w", err)
	}

	pcm48k, err := resamplePCM(pcm24k, ttsInputSampleRate, ttsOutputSampleRate)
	if err != nil {
		return nil, fmt.Errorf("resamplePCM: %w", err)
	}
	log.Printf("[tts] resampled %d → %d samples (%.1fs @ 48kHz)",
		len(pcm24k), len(pcm48k), float64(len(pcm48k))/ttsOutputSampleRate)

	return encodePCMToOggOpus(pcm48k)
}

// stripWAVHeader はデータ先頭に "RIFF" マジックがある場合、"data" チャンクのペイロードを返す。
func stripWAVHeader(data []byte) []byte {
	if len(data) < 12 || !bytes.HasPrefix(data, []byte("RIFF")) {
		return data
	}
	pos := 12
	for pos+8 <= len(data) {
		chunkID := string(data[pos : pos+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		pos += 8
		if chunkID == "data" {
			end := pos + chunkSize
			if end > len(data) {
				end = len(data)
			}
			return data[pos:end]
		}
		pos += chunkSize
		if chunkSize%2 != 0 {
			pos++
		}
	}
	return data
}

// parsePCM16 はリトルエンディアン16bit PCMバイト列をint16スライスに変換する。
func parsePCM16(data []byte) ([]int16, error) {
	if len(data)%2 != 0 {
		return nil, fmt.Errorf("odd PCM byte length: %d", len(data))
	}
	samples := make([]int16, len(data)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(data[i*2 : i*2+2]))
	}
	return samples, nil
}

// resamplePCM はPCMをリサンプリングする。
func resamplePCM(src []int16, srcRate, dstRate int) ([]int16, error) {
	r, err := gomplerate.NewResampler(channels, srcRate, dstRate)
	if err != nil {
		return nil, fmt.Errorf("gomplerate.NewResampler: %w", err)
	}
	return r.ResampleInt16(src), nil
}

// encodePCMToOggOpus はPCMをOpusエンコードしてOgg Opusコンテナに格納する。
// エンコーダ: hraban/opus (libopus cgo)
// Oggコンテナ: kazzmir/opus-go/ogg
func encodePCMToOggOpus(pcm []int16) ([]byte, error) {
	const preSkip = 312 // Opus standard pre-skip @ 48kHz

	enc, err := hrabanopus.NewEncoder(ttsOutputSampleRate, channels, hrabanopus.AppAudio)
	if err != nil {
		return nil, fmt.Errorf("opus.NewEncoder: %w", err)
	}

	var buf bytes.Buffer
	pw := ogg.NewPacketWriter(&buf, 0x57485052) // "WHPR"

	head := ogg.OpusHead{
		Version:              1,
		Channels:             channels,
		PreSkip:              preSkip,
		InputSampleRate:      ttsOutputSampleRate,
		ChannelMappingFamily: 0,
	}
	headPkt, err := ogg.BuildOpusHeadPacket(head)
	if err != nil {
		return nil, fmt.Errorf("BuildOpusHeadPacket: %w", err)
	}
	if err := pw.WritePacket(headPkt, 0, true, false); err != nil {
		return nil, fmt.Errorf("write OpusHead: %w", err)
	}

	tags := ogg.OpusTags{Vendor: "whisper-link"}
	tagsPkt, err := ogg.BuildOpusTagsPacket(tags)
	if err != nil {
		return nil, fmt.Errorf("BuildOpusTagsPacket: %w", err)
	}
	if err := pw.WritePacket(tagsPkt, 0, false, false); err != nil {
		return nil, fmt.Errorf("write OpusTags: %w", err)
	}

	packet := make([]byte, 4096)
	var totalSamples uint64

	for i := 0; i+frameSize <= len(pcm); i += frameSize {
		n, err := enc.Encode(pcm[i:i+frameSize], packet)
		if err != nil {
			return nil, fmt.Errorf("opus.Encode: %w", err)
		}

		totalSamples += uint64(frameSize)
		granule := uint64(preSkip) + totalSamples
		isLast := i+frameSize+frameSize > len(pcm)

		if err := pw.WritePacket(packet[:n], granule, false, isLast); err != nil {
			return nil, fmt.Errorf("ogg.WritePacket: %w", err)
		}
	}

	if err := pw.Flush(); err != nil {
		return nil, fmt.Errorf("ogg.Flush: %w", err)
	}

	return buf.Bytes(), nil
}
