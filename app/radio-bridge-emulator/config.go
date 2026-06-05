package main

import "github.com/BurntSushi/toml"

type Config struct {
	Server ServerConfig `toml:"server"`
	Audio  AudioConfig  `toml:"audio"`
}

type ServerConfig struct {
	ListenAddr string `toml:"listen_addr"`
}

type AudioConfig struct {
	InputThresholdRMS     uint16 `toml:"input_threshold_rms"`
	InputSilenceMs        uint64 `toml:"input_silence_ms"`
	InputMinRecordingMs   uint64 `toml:"input_min_recording_ms"`
	InputMaxRecordingSecs uint64 `toml:"input_max_recording_secs"`
}

func LoadConfig(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
