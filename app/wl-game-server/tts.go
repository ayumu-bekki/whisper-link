package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"time"

	hrabanopus "github.com/hraban/opus"
	"github.com/kazzmir/opus-go/ogg"
	"google.golang.org/genai"
)

const (
	sampleRate  = 24000 // Gemini TTS出力 / Opusエンコード: 24kHz
	channels    = 1
	frameSize   = 480   // 20ms @ 24kHz
	opusBitrate = 16000 // 16kbps
)

const defaultTTSModel = "gemini-3.1-flash-tts-preview"

const ttsPersona = `# VOICE CHARACTER: 無線オペレーターA (Despina)
## 基本プロフィール
- 話者: 日本人女性の熟練アマチュア無線オペレーター
- 声質: 明るく明瞭な女性の声。FM変調がかかった状態でも一語一語がはっきり聞き取れる
- 話速: やや速め。ただし子音・語尾を明確に発音し、早口でも聞き取りやすさを保つ
- 感情: 落ち着いた親しみやすさ。抑揚は最小限に抑え、チャンクをまたいでも同じトーンを維持する
- 語尾を伸ばさない。文末は平坦〜下がり調子で終わる

## 発音の特徴
- コールサイン (英数字の組み合わせ) は一文字ずつ日本語読みで発音する: S4CA → "エス ヨン シー エー"
- 「どうぞ」は無線用語として明確に発音し、その後は無音にする
- 助詞・助動詞を略さず丁寧に発音する

## 音響特性 (毎回一定に保つこと)
- Pitch: 一定 (変動なし)
- Tempo: 1.1x (やや速め、一定)
- Breathiness: minimal
- Vocal fry: none`

const ttsPromptTemplate = ttsPersona + `

## THE SCENE: 無線テストオペレーション
無線の初心者に対して、相手が話した内容と同じ内容を返答するテストオペレーションをしています。

#### TRANSCRIPT
%s。こちらS4CA。%s`

const ttsPromptTemplateS4CQ = ttsPersona + `

## THE SCENE: 無線テストオペレーション
無線の初心者から来た質問に対して返答するテストオペレーションをしています。

#### TRANSCRIPT
%s`

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

// GenerateOggOpus は sender/message から TTS 音声を生成して Ogg Opus で返す。
func (t *TTSClient) GenerateOggOpus(ctx context.Context, sender, message string) ([]byte, error) {
	return t.GenerateOggOpusFromPrompt(ctx, fmt.Sprintf(ttsPromptTemplate, sender, message))
}

// GenerateOggOpusFromPrompt は組み立て済みプロンプトから TTS 音声を生成して Ogg Opus で返す。
func (t *TTSClient) GenerateOggOpusFromPrompt(ctx context.Context, prompt string) ([]byte, error) {
	pcm24k, err := t.GeneratePCM24kFromPrompt(ctx, prompt)
	if err != nil {
		return nil, err
	}
	return encodePCMToOggOpus(pcm24k)
}

// GeneratePCM24kFromPrompt はプロンプトからTTS音声を生成し、24kHz mono の PCM(int16)で返す。
func (t *TTSClient) GeneratePCM24kFromPrompt(ctx context.Context, prompt string) ([]int16, error) {
	start := time.Now()
	resp, err := t.client.Models.GenerateContent(ctx, t.model,
		[]*genai.Content{
			genai.NewContentFromText(prompt, genai.RoleUser),
		},
		&genai.GenerateContentConfig{
			ResponseModalities: []string{"audio"},
			SpeechConfig: &genai.SpeechConfig{
				VoiceConfig: &genai.VoiceConfig{
					PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{
						VoiceName: "Despina",
					},
				},
			},
		},
	)
	log.Printf("[gemini] TTS latency: %v", time.Since(start))
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

	pcm, err := parsePCM16(pcmData)
	if err != nil {
		return nil, fmt.Errorf("parsePCM16: %w", err)
	}
	log.Printf("[tts] decoded %d samples (%.1fs @ 24kHz)", len(pcm), float64(len(pcm))/sampleRate)

	return pcm, nil
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

// encodePCMToOggOpus はPCMをOpusエンコードしてOgg Opusコンテナに格納する。
// エンコーダ: hraban/opus (libopus cgo)
// Oggコンテナ: kazzmir/opus-go/ogg
func encodePCMToOggOpus(pcm []int16) ([]byte, error) {
	const preSkip = 312 // Opus standard pre-skip

	enc, err := hrabanopus.NewEncoder(sampleRate, channels, hrabanopus.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("opus.NewEncoder: %w", err)
	}
	if err := enc.SetBitrate(opusBitrate); err != nil {
		return nil, fmt.Errorf("opus.SetBitrate: %w", err)
	}

	var buf bytes.Buffer
	pw := ogg.NewPacketWriter(&buf, 0x57485052) // "WHPR"

	head := ogg.OpusHead{
		Version:              1,
		Channels:             channels,
		PreSkip:              preSkip,
		InputSampleRate:      sampleRate,
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
