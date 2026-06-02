package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/hraban/opus"
	"google.golang.org/genai"
)

const (
	ttsInputSampleRate  = 24000 // Gemini TTS出力: 24kHz
	ttsOutputSampleRate = 48000 // Opusエンコード: 48kHz
	sampleRate          = ttsOutputSampleRate
	channels            = 1
	frameSize           = 960 // 20ms @ 48kHz
)

const ttsModel = "gemini-2.5-flash-preview-tts"

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
}

func NewTTSClient(ctx context.Context, apiKey string) (*TTSClient, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: apiKey})
	if err != nil {
		return nil, fmt.Errorf("genai.NewClient: %w", err)
	}
	return &TTSClient{client: client}, nil
}

// GenerateOggOpus はsender/messageからTTS音声を生成してOgg Opusで返す。
func (t *TTSClient) GenerateOggOpus(ctx context.Context, sender, message string) ([]byte, error) {
	prompt := fmt.Sprintf(ttsPromptTemplate, sender, message)

	resp, err := t.client.Models.GenerateContent(ctx, ttsModel,
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

	// WAVヘッダが付いている場合はスキップしてPCMデータのみ取り出す
	pcmData := stripWAVHeader(blob.Data)

	pcm24k, err := parsePCM16(pcmData)
	if err != nil {
		return nil, fmt.Errorf("parsePCM16: %w", err)
	}

	// 24kHz → 48kHz アップサンプリング (線形補間)
	pcm48k := upsample2x(pcm24k)

	return encodePCMToOggOpus(pcm48k)
}

// stripWAVHeader はデータ先頭に "RIFF" マジックがある場合WAVヘッダをスキップする。
func stripWAVHeader(data []byte) []byte {
	if len(data) > 44 && bytes.HasPrefix(data, []byte("RIFF")) {
		return data[44:]
	}
	return data
}

// parsePCM16 はリトルエンディアン16bitPCMバイト列をint16スライスに変換する。
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

// upsample2x は24kHz PCMを48kHzに線形補間でアップサンプリングする。
func upsample2x(src []int16) []int16 {
	if len(src) == 0 {
		return nil
	}
	dst := make([]int16, len(src)*2)
	for i, s := range src {
		dst[i*2] = s
		if i+1 < len(src) {
			dst[i*2+1] = int16((int32(s) + int32(src[i+1])) / 2)
		} else {
			dst[i*2+1] = s
		}
	}
	return dst
}

// encodePCMToOggOpus はPCMサンプル列をOpusエンコードしてOgg Opusコンテナに格納する。
func encodePCMToOggOpus(pcm []int16) ([]byte, error) {
	enc, err := opus.NewEncoder(ttsOutputSampleRate, channels, opus.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("opus.NewEncoder: %w", err)
	}

	w := newOggWriter()
	w.writePage(buildOpusHead(), 0, true, false)
	w.writePage(buildOpusTags(), 0, false, false)

	granule := uint64(0)
	opusBuf := make([]byte, 4096)
	for i := 0; i+frameSize <= len(pcm); i += frameSize {
		n, err := enc.Encode(pcm[i:i+frameSize], opusBuf)
		if err != nil {
			return nil, fmt.Errorf("opus.Encode: %w", err)
		}
		granule += uint64(frameSize)
		w.writeAudioPacket(opusBuf[:n], granule)
	}
	w.flush()
	return w.bytes(), nil
}
