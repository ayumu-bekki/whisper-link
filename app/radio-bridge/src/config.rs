use serde::Deserialize;
use std::path::Path;

#[derive(Debug, Deserialize, Clone)]
pub struct Config {
    pub server: ServerConfig,
    pub gpio: GpioConfig,
    pub audio: AudioConfig,
    pub timing: TimingConfig,
    pub queue: QueueConfig,
}

#[derive(Debug, Deserialize, Clone)]
pub struct ServerConfig {
    pub listen_addr: String,
}

#[derive(Debug, Deserialize, Clone)]
pub struct GpioConfig {
    pub ptt_pin: u8,
}

#[derive(Debug, Deserialize, Clone)]
pub struct AudioConfig {
    pub output_device: String,
    pub input_device: String,
    pub input_threshold_rms: u16,
    pub input_silence_ms: u64,
    pub input_max_recording_secs: u64,
}

#[derive(Debug, Deserialize, Clone)]
pub struct TimingConfig {
    pub ptt_on_delay_ms: u64,
    pub ptt_off_delay_ms: u64,
    pub cooldown_ms: u64,
}

#[derive(Debug, Deserialize, Clone)]
pub struct QueueConfig {
    pub max_audio_duration_secs: u64,
    pub max_queue_size: usize,
}

impl Config {
    pub fn load(path: &Path) -> Result<Self, Box<dyn std::error::Error + Send + Sync>> {
        let content = std::fs::read_to_string(path)?;
        let config: Config = toml::from_str(&content)?;
        Ok(config)
    }
}
