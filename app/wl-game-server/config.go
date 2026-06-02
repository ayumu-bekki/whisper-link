package main

import (
	"github.com/BurntSushi/toml"
)

type Config struct {
	RadioBridge RadioBridgeConfig `toml:"radio_bridge"`
	Gemini      GeminiConfig      `toml:"gemini"`
	Log         LogConfig         `toml:"log"`
	WebSocket   WebSocketConfig   `toml:"websocket"`
}

type WebSocketConfig struct {
	ListenAddr string `toml:"listen_addr"`
}

type RadioBridgeConfig struct {
	Host                  string `toml:"host"`
	Port                  int    `toml:"port"`
	ReconnectIntervalSecs int    `toml:"reconnect_interval_secs"`
	MaxReconnectAttempts  int    `toml:"max_reconnect_attempts"`
}

type GeminiConfig struct {
	APIKey                string `toml:"api_key"`
	TranscribeModel       string `toml:"transcribe_model"`
	ReasoningModel        string `toml:"reasoning_model"`
	TTSModel              string `toml:"tts_model"`
	TranscribePromptFile  string `toml:"transcribe_prompt_file"`
	TranscribeSchemaFile  string `toml:"transcribe_schema_file"`
}

type LogConfig struct {
	Level string `toml:"level"`
}

func LoadConfig(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
