use crate::audio::player::AudioPlayer;
use crate::audio::recorder::AudioRecorder;
use crate::config::Config;
use crate::queue::AudioQueue;
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
        loop {
            let has_queue = {
                let q = self.queue.lock().unwrap();
                !q.is_empty()
            };

            if has_queue {
                if self.recorder.is_recording() {
                    debug!("queue has items but mic is recording, waiting...");
                    sleep(Duration::from_millis(100)).await;
                    continue;
                }
                info!("state: IDLE -> PTT_ON");
                return State::PttOn;
            }

            sleep(Duration::from_millis(100)).await;
        }
    }

    async fn handle_ptt_on(&self, ptt_pin: &mut OutputPin) -> State {
        ptt_pin.set_high();
        info!("GPIO PTT=HIGH");
        sleep(Duration::from_millis(self.config.timing.ptt_on_delay_ms)).await;
        info!("state: PTT_ON -> PLAYING");
        State::Playing
    }

    async fn handle_playing(&self, player: &AudioPlayer) -> State {
        let entry = {
            let mut q = self.queue.lock().unwrap();
            q.pop()
        };

        match entry {
            None => {
                warn!("PLAYING state but queue is empty, going to PTT_OFF");
                State::PttOff
            }
            Some(entry) => {
                info!(
                    duration_secs = entry.duration.as_secs_f32(),
                    "playing audio"
                );
                let data = entry.ogg_opus_data.clone();
                let player_device = player.device.clone();
                let result = tokio::task::spawn_blocking(move || {
                    let p = AudioPlayer::new(&player_device);
                    p.play_blocking(&data)
                })
                .await;

                match result {
                    Ok(Ok(())) => info!("playback complete"),
                    Ok(Err(e)) => error!("playback error: {e}"),
                    Err(e) => error!("spawn_blocking error: {e}"),
                }

                info!("state: PLAYING -> PTT_OFF");
                State::PttOff
            }
        }
    }

    async fn handle_ptt_off(&self, ptt_pin: &mut OutputPin) -> State {
        sleep(Duration::from_millis(self.config.timing.ptt_off_delay_ms)).await;
        ptt_pin.set_low();
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
            !q.is_empty()
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
        loop {
            if !self.recorder.is_recording() {
                info!("mic recording finished");
                let has_queue = {
                    let q = self.queue.lock().unwrap();
                    !q.is_empty()
                };
                if has_queue {
                    info!("state: LISTENING -> PTT_ON");
                    return State::PttOn;
                } else {
                    info!("state: LISTENING -> IDLE");
                    return State::Idle;
                }
            }
            sleep(Duration::from_millis(100)).await;
        }
    }
}
