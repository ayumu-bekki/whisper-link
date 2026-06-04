package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"google.golang.org/genai"
)

type TranscriptionItem struct {
	Receiver string `json:"receiver"`
	Sender   string `json:"sender"`
	Message  string `json:"message"`
}

type TranscriptionResult struct {
	Items []TranscriptionItem `json:"item"`
}

type GeminiProcessor struct {
	cfg              GeminiConfig
	client           *genai.Client
	transcribePrompt string
	transcribeSchema *genai.Schema
}

func NewGeminiProcessor(ctx context.Context, cfg GeminiConfig) (*GeminiProcessor, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  cfg.APIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("genai.NewClient: %w", err)
	}

	prompt, err := os.ReadFile(cfg.TranscribePromptFile)
	if err != nil {
		return nil, fmt.Errorf("read prompt file %q: %w", cfg.TranscribePromptFile, err)
	}

	schemaBytes, err := os.ReadFile(cfg.TranscribeSchemaFile)
	if err != nil {
		return nil, fmt.Errorf("read schema file %q: %w", cfg.TranscribeSchemaFile, err)
	}
	schema, err := parseSchema(schemaBytes)
	if err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}

	return &GeminiProcessor{
		cfg:              cfg,
		client:           client,
		transcribePrompt: string(prompt),
		transcribeSchema: schema,
	}, nil
}

func (p *GeminiProcessor) Close() {
	// 新SDKの Client には Close メソッドがないためno-op
}

func (p *GeminiProcessor) Transcribe(ctx context.Context, oggData []byte) (*TranscriptionResult, error) {
	contents := []*genai.Content{
		genai.NewContentFromParts([]*genai.Part{
			genai.NewPartFromText(p.transcribePrompt),
			genai.NewPartFromBytes(oggData, "audio/ogg"),
		}, genai.RoleUser),
	}

	config := &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json",
		ResponseSchema:   p.transcribeSchema,
	}

	start := time.Now()
	resp, err := p.client.Models.GenerateContent(ctx, p.cfg.TranscribeModel, contents, config)
	log.Printf("[gemini] Transcribe latency: %v", time.Since(start))
	if err != nil {
		return nil, fmt.Errorf("GenerateContent: %w", err)
	}

	if resp == nil || len(resp.Candidates) == 0 ||
		resp.Candidates[0].Content == nil ||
		len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("empty response from Gemini")
	}

	text := resp.Candidates[0].Content.Parts[0].Text
	if text == "" {
		return nil, fmt.Errorf("empty response from Gemini")
	}

	var result TranscriptionResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return nil, fmt.Errorf("json.Unmarshal: %w", err)
	}
	return &result, nil
}

func (p *GeminiProcessor) Reason(ctx context.Context, t *TranscriptionResult) (string, error) {
	// プレースホルダー: 将来ゲーム応答生成
	return "OK", nil
}

const askSystemPromptTemplate = `あなたはアマチュア無線のオペレーターです。あなたのコールサインは%sです。
相手のコールサイン%sから質問を受けています。
以下のルールに必ず従ってください:
- 回答は日本語で、要点のみを1文で述べてください
- 最大60文字程度に収めてください（フォーマット部分を除く）
- マークダウン記法や箇条書きは使わず、プレーンテキストで回答してください
- 無線交信らしい口調で話してください
- コールサインは音声で読み上げられるため、1文字ずつ日本語のカタカナ読みに変換して出力してください
  - 数字は「ゼロ イチ ニー サン ヨン ゴー ロク ナナ ハチ キュー」
  - アルファベットは「エー ビー シー ディー イー エフ ジー エイチ アイ ジェイ ケー エル エム エヌ オー ピー キュー アール エス ティー ユー ブイ ダブリュー エックス ワイ ゼット」
  - 各読みの区切りには半角スペースを入れてください
  - 例: S4CQ → 「エス ヨン シー キュー」、S4AK → 「エス ヨン エー ケー」
- 以下のフォーマットを守ってください（コールサイン部分は上記の読み変換を適用すること）
  - <相手コールサインの読み>。こちら<自分コールサインの読み>。 <回答内容>。 どうぞ。`

// Ask は TranscribeModel で一問一答の回答を生成する（S4CQ ハンドラ用）。
func (p *GeminiProcessor) Ask(ctx context.Context, sender, receiver, question string) (string, error) {
	systemPrompt := fmt.Sprintf(askSystemPromptTemplate, receiver, sender)

	contents := []*genai.Content{
		genai.NewContentFromText(question, genai.RoleUser),
	}

	config := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(systemPrompt, genai.RoleUser),
	}

	start := time.Now()
	resp, err := p.client.Models.GenerateContent(ctx, p.cfg.TranscribeModel, contents, config)
	log.Printf("[gemini] Ask latency: %v", time.Since(start))
	if err != nil {
		return "", fmt.Errorf("GenerateContent (Ask): %w", err)
	}

	if resp == nil || len(resp.Candidates) == 0 ||
		resp.Candidates[0].Content == nil ||
		len(resp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty response from Gemini Ask")
	}

	text := resp.Candidates[0].Content.Parts[0].Text
	if text == "" {
		return "", fmt.Errorf("empty text from Gemini Ask")
	}

	return text, nil
}

// generateReplyPromptTemplate は GenerateReply で共有ログをラップするプロンプト。
// %s の順: 自分のコールサイン, 会話ログ(Render結果), 自分のコールサイン
const generateReplyPromptTemplate = `以下は無線交信のログです。あなたのコールサインは%sです。
各行は「[発言者] -> [対象] メッセージ内容」の形式です。
--- これまでの交信 ---
%s
--- ここまで ---
直前のあなた宛のメッセージに対し、%s として次の1交信を返してください。`

// GenerateReply は共有会話ログ全体を文脈に、persona(=応答するNPCのキャラ)で1ターン応答を生成する。
// genai.Chat を使わず毎回ワンショットの GenerateContent を呼ぶ。会話履歴は呼び出し側が
// ConversationLog.Render() で整形して history 引数として渡す。
func (p *GeminiProcessor) GenerateReply(ctx context.Context, persona Persona, selfCallsign, history string) (string, error) {
	systemPrompt := persona.SystemInstruction(selfCallsign)
	prompt := fmt.Sprintf(generateReplyPromptTemplate, selfCallsign, history, selfCallsign)

	contents := []*genai.Content{
		genai.NewContentFromText(prompt, genai.RoleUser),
	}
	config := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText(systemPrompt, genai.RoleUser),
	}

	start := time.Now()
	resp, err := p.client.Models.GenerateContent(ctx, p.cfg.ReasoningModel, contents, config)
	log.Printf("[gemini] GenerateReply latency: %v", time.Since(start))
	if err != nil {
		return "", fmt.Errorf("GenerateContent (GenerateReply): %w", err)
	}

	if resp == nil || len(resp.Candidates) == 0 ||
		resp.Candidates[0].Content == nil ||
		len(resp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty response from Gemini GenerateReply")
	}

	text := resp.Candidates[0].Content.Parts[0].Text
	if text == "" {
		return "", fmt.Errorf("empty text from Gemini GenerateReply")
	}

	return text, nil
}

// parseSchema は JSON バイト列を genai.Schema に変換する。
func parseSchema(data []byte) (*genai.Schema, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	return convertSchema(raw)
}

func convertSchema(raw map[string]any) (*genai.Schema, error) {
	s := &genai.Schema{}

	if t, ok := raw["type"].(string); ok {
		switch t {
		case "object":
			s.Type = genai.TypeObject
		case "array":
			s.Type = genai.TypeArray
		case "string":
			s.Type = genai.TypeString
		case "number":
			s.Type = genai.TypeNumber
		case "integer":
			s.Type = genai.TypeInteger
		case "boolean":
			s.Type = genai.TypeBoolean
		}
	}

	if props, ok := raw["properties"].(map[string]any); ok {
		s.Properties = make(map[string]*genai.Schema, len(props))
		for k, v := range props {
			vm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			child, err := convertSchema(vm)
			if err != nil {
				return nil, fmt.Errorf("property %q: %w", k, err)
			}
			s.Properties[k] = child
		}
	}

	if items, ok := raw["items"].(map[string]any); ok {
		child, err := convertSchema(items)
		if err != nil {
			return nil, fmt.Errorf("items: %w", err)
		}
		s.Items = child
	}

	if required, ok := raw["required"].([]any); ok {
		for _, r := range required {
			if rs, ok := r.(string); ok {
				s.Required = append(s.Required, rs)
			}
		}
	}

	return s, nil
}
