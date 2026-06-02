package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
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
	cfg            GeminiConfig
	client         *genai.Client
	transcribePrompt string
	transcribeSchema *genai.Schema
}

func NewGeminiProcessor(ctx context.Context, cfg GeminiConfig) (*GeminiProcessor, error) {
	client, err := genai.NewClient(ctx, option.WithAPIKey(cfg.APIKey))
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
	p.client.Close()
}

func (p *GeminiProcessor) Transcribe(ctx context.Context, oggData []byte) (*TranscriptionResult, error) {
	model := p.client.GenerativeModel(p.cfg.TranscribeModel)
	model.ResponseMIMEType = "application/json"
	model.ResponseSchema = p.transcribeSchema

	resp, err := model.GenerateContent(ctx,
		genai.Text(p.transcribePrompt),
		genai.Blob{MIMEType: "audio/ogg", Data: oggData},
	)
	if err != nil {
		return nil, fmt.Errorf("GenerateContent: %w", err)
	}

	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil || len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("empty response from Gemini")
	}

	text, ok := resp.Candidates[0].Content.Parts[0].(genai.Text)
	if !ok {
		return nil, fmt.Errorf("unexpected response part type")
	}

	var result TranscriptionResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return nil, fmt.Errorf("json.Unmarshal: %w", err)
	}
	return &result, nil
}

func (p *GeminiProcessor) Reason(ctx context.Context, t *TranscriptionResult) (string, error) {
	// プレースホルダー: 将来 gemini-2.5-flash でゲーム応答生成
	return "OK", nil
}

// parseSchema は JSON バイト列を genai.Schema に変換する。
// genai.Schema の構造に合わせて再帰的にパースする。
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

	// propertyOrdering は genai.Schema では未サポートのため無視する

	return s, nil
}
