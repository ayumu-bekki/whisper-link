use crate::audio::player::{AudioPlayer, decode_ogg_opus, write_pcm_to_alsa};
use crate::audio::recorder::AudioRecorder;
use crate::config::Config;
use crate::queue::{AudioQueue, StreamStatus};
use rppal::gpio::{Gpio, OutputPin};
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tokio::time::sleep;
use tracing::{debug, error, info, warn};

#[derive(Debug, Clone, PartialEq)]
pub enum State {
    Idle,
    PttOn,
    Playing,
    PttOff,
    Cooldown,
    Listening,
}

impl std::fmt::Display for State {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            State::Idle => write!(f, "IDLE"),
            State::PttOn => write!(f, "PTT_ON"),
            State::Playing => write!(f, "PLAYING"),
            State::PttOff => write!(f, "PTT_OFF"),
            State::Cooldown => write!(f, "COOLDOWN"),
            State::Listening => write!(f, "LISTENING"),
        }
    }
}

pub struct Controller {
    queue: Arc<Mutex<AudioQueue>>,
    recorder: Arc<AudioRecorder>,
    config: Config,
}

impl Controller {
    pub fn new(queue: Arc<Mutex<AudioQueue>>, recorder: Arc<AudioRecorder>, config: Config) -> Self {
        Self { queue, recorder, config }
    }

    pub async fn run(self) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
        let gpio = Gpio::new()?;
        let mut ptt_pin = gpio.get(self.config.gpio.ptt_pin)?.into_output();
        ptt_pin.set_low();

        let player = AudioPlayer::new(&self.config.audio.output_device);

        let mut state = State::Idle;
        info!("controller started, state: {state}");

        loop {
            match state {
                State::Idle => {
                    state = self.handle_idle().await;
                }
                State::PttOn => {
                    state = self.handle_ptt_on(&mut ptt_pin).await;
                }
                State::Playing => {
                    state = self.handle_playing(&player).await;
                }
                State::PttOff => {
                    state = self.handle_ptt_off(&mut ptt_pin).await;
                }
                State::Cooldown => {
                    state = self.handle_cooldown().await;
                }
                State::Listening => {
                    state = self.handle_listening().await;
                }
            }
        }
    }

    async fn handle_idle(&self) -> State {
        // キューにアイテムがある間、録音終了を最大この時間待つ
        const RECORDING_WAIT_TIMEOUT_MS: u64 = 3000;
        let mut recording_wait_ms: u64 = 0;

        loop {
            let has_queue = {
                let q = self.queue.lock().unwrap();
                q.has_stream()
            };

            if has_queue {
                if self.recorder.is_recording() {
                    if recording_wait_ms >= RECORDING_WAIT_TIMEOUT_MS {
                        warn!(
                            "mic recording did not stop after {}ms, forcing stop",
                            recording_wait_ms
                        );
                        self.recorder.force_stop_recording();
                    } else {
                        debug!("queue has items but mic is recording, waiting...");
                        recording_wait_ms += 100;
                        sleep(Duration::from_millis(100)).await;
                        continue;
                    }
                }
                info!("state: IDLE -> PTT_ON");
                return State::PttOn;
            }

            recording_wait_ms = 0;
            sleep(Duration::from_millis(100)).await;
        }
    }

    async fn handle_ptt_on(&self, ptt_pin: &mut OutputPin) -> State {
        self.recorder.set_transmitting(true);
        ptt_pin.set_high();
        info!("GPIO PTT=HIGH");
        sleep(Duration::from_millis(self.config.timing.ptt_on_delay_ms)).await;
        info!("state: PTT_ON -> PLAYING");
        State::Playing
    }

    /// ストリームのチャンクを全て収集してからPCMを結合し、1回のALSA書き込みで再生する。
    ///
    /// チャンクごとに ALSA を open/close すると先頭クリック音と無音ギャップが生じる。
    /// decode は async コンテキストで高速に行い、ALSA 書き込みだけを spawn_blocking に渡す。
    async fn handle_playing(&self, player: &AudioPlayer) -> State {
        // 連続再生中に現ストリームのチャンクが尽きたとき、後続をこの時間だけ待つ。
        const STREAM_TIMEOUT_MS: u64 = 2000;
        const POLL_INTERVAL_MS: u64 = 100;

        let stream_id = {
            let q = self.queue.lock().unwrap();
            match q.current_stream() {
                Some(id) => id.to_string(),
                None => {
                    warn!("PLAYING state but no stream queued, going to PTT_OFF");
                    return State::PttOff;
                }
            }
        };
        info!(%stream_id, "stream playback started");

        // ストリーム全体の 24kHz PCM を積み上げる。terminal chunk が来たら一括再生。
        let mut all_pcm: Vec<i16> = Vec::new();

        loop {
            let entry = {
                let mut q = self.queue.lock().unwrap();
                q.pop_current()
            };

            if let Some(entry) = entry {
                let is_terminal = matches!(
                    entry.status,
                    StreamStatus::End | StreamStatus::Oneshot
                );

                // デコードは軽量な同期処理なので async コンテキストで直接実行する。
                match decode_ogg_opus(&entry.ogg_opus_data) {
                    Ok(pcm) => {
                        info!(
                            chunk_samples = pcm.len(),
                            duration_secs = pcm.len() as f32 / 24000.0,
                            status = ?entry.status,
                            "decoded audio chunk"
                        );
                        all_pcm.extend(pcm);
                    }
                    Err(e) => error!("decode error: {e}"),
                }

                if is_terminal {
                    info!(%stream_id, total_samples = all_pcm.len(), "stream complete, playing");
                    self.play_pcm(player, all_pcm).await;
                    self.queue.lock().unwrap().finish_current();
                    return State::PttOff;
                }
                continue;
            }

            // 現ストリームのチャンクが一時的に尽きた。STREAM_TIMEOUT_MS だけ後続を待つ。
            let mut waited = 0;
            let mut resumed = false;
            while waited < STREAM_TIMEOUT_MS {
                sleep(Duration::from_millis(POLL_INTERVAL_MS)).await;
                waited += POLL_INTERVAL_MS;
                let has_chunk = {
                    let q = self.queue.lock().unwrap();
                    q.current_has_chunk()
                };
                if has_chunk {
                    resumed = true;
                    break;
                }
            }

            if resumed {
                continue;
            }

            // タイムアウト: 収集済み PCM をそのまま再生して終了。
            info!(%stream_id, total_samples = all_pcm.len(), "stream timeout, playing accumulated");
            self.play_pcm(player, all_pcm).await;
            self.queue.lock().unwrap().finish_current();
            return State::PttOff;
        }
    }

    /// 24kHz mono PCM を1回のALSA open/write/drain/close で再生する。
    async fn play_pcm(&self, player: &AudioPlayer, pcm: Vec<i16>) {
        if pcm.is_empty() {
            warn!("play_pcm: empty PCM, skipping");
            return;
        }
        let device = player.device.clone();
        let result = tokio::task::spawn_blocking(move || write_pcm_to_alsa(&device, &pcm)).await;

        match result {
            Ok(Ok(())) => info!("playback complete"),
            Ok(Err(e)) => error!("playback error: {e}"),
            Err(e) => error!("spawn_blocking error: {e}"),
        }
    }

    async fn handle_ptt_off(&self, ptt_pin: &mut OutputPin) -> State {
        sleep(Duration::from_millis(self.config.timing.ptt_off_delay_ms)).await;
        ptt_pin.set_low();
        self.recorder.set_transmitting(false);
        info!("GPIO PTT=LOW");
        info!("state: PTT_OFF -> COOLDOWN");
        State::Cooldown
    }

    async fn handle_cooldown(&self) -> State {
        sleep(Duration::from_millis(self.config.timing.cooldown_ms)).await;
        info!("cooldown complete");

        if self.recorder.is_recording() {
            info!("state: COOLDOWN -> LISTENING (mic recording detected)");
            return State::Listening;
        }

        let has_queue = {
            let q = self.queue.lock().unwrap();
            q.has_stream()
        };

        if has_queue {
            info!("state: COOLDOWN -> PTT_ON");
            State::PttOn
        } else {
            info!("state: COOLDOWN -> IDLE");
            State::Idle
        }
    }

    async fn handle_listening(&self) -> State {
        const RECORDING_WAIT_TIMEOUT_MS: u64 = 3000;
        let mut recording_wait_ms: u64 = 0;

        loop {
            if !self.recorder.is_recording() {
                info!("mic recording finished");
                let has_queue = {
                    let q = self.queue.lock().unwrap();
                    q.has_stream()
                };
                if has_queue {
                    info!("state: LISTENING -> PTT_ON");
                    return State::PttOn;
                } else {
                    info!("state: LISTENING -> IDLE");
                    return State::Idle;
                }
            }

            recording_wait_ms += 100;
            if recording_wait_ms >= RECORDING_WAIT_TIMEOUT_MS {
                warn!(
                    "mic recording did not stop after {}ms, forcing stop",
                    recording_wait_ms
                );
                self.recorder.force_stop_recording();
                recording_wait_ms = 0;
            }

            sleep(Duration::from_millis(100)).await;
        }
    }
}
