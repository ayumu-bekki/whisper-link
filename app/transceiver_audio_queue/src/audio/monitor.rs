use alsa::pcm::{Access, Format, HwParams, PCM};
use alsa::{Direction, ValueOr};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};
use tracing::{error, trace};

const SAMPLE_RATE: u32 = 48000;
const CHANNELS: u32 = 1;
const FRAMES_PER_PERIOD: u64 = 1024;

pub struct AudioMonitor {
    is_active: Arc<AtomicBool>,
}

impl AudioMonitor {
    /// バックグラウンドスレッドで音声入力を常時監視する。
    /// `is_active` が true の間、入力RMSがしきい値を超えている状態を保持する。
    pub fn start(
        device: &str,
        threshold_rms: u16,
        silence_duration: Duration,
    ) -> Result<Self, Box<dyn std::error::Error + Send + Sync>> {
        let is_active = Arc::new(AtomicBool::new(false));
        let is_active_clone = Arc::clone(&is_active);

        let device = device.to_string();

        std::thread::spawn(move || {
            if let Err(e) = monitor_loop(&device, threshold_rms, silence_duration, &is_active_clone) {
                error!("audio monitor error: {e}");
            }
        });

        Ok(Self { is_active })
    }

    pub fn is_active(&self) -> bool {
        self.is_active.load(Ordering::Relaxed)
    }
}

fn monitor_loop(
    device: &str,
    threshold_rms: u16,
    silence_duration: Duration,
    is_active: &AtomicBool,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let pcm = PCM::new(device, Direction::Capture, false)?;

    {
        let hwp = HwParams::any(&pcm)?;
        hwp.set_channels(CHANNELS)?;
        hwp.set_rate(SAMPLE_RATE, ValueOr::Nearest)?;
        hwp.set_format(Format::s16())?;
        hwp.set_access(Access::RWInterleaved)?;
        hwp.set_period_size(FRAMES_PER_PERIOD as i64, ValueOr::Nearest)?;
        pcm.hw_params(&hwp)?;
    }

    pcm.start()?;

    let io = pcm.io_i16()?;
    let mut buf = vec![0i16; FRAMES_PER_PERIOD as usize];
    let mut last_above_threshold: Option<Instant> = None;

    loop {
        match io.readi(&mut buf) {
            Ok(frames) if frames > 0 => {
                let rms = compute_rms(&buf[..frames]);
                trace!("input rms: {rms}");

                if rms >= threshold_rms {
                    last_above_threshold = Some(Instant::now());
                    is_active.store(true, Ordering::Relaxed);
                } else if let Some(last) = last_above_threshold {
                    if last.elapsed() >= silence_duration {
                        is_active.store(false, Ordering::Relaxed);
                        last_above_threshold = None;
                    }
                }
            }
            Ok(_) => {}
            Err(e) => {
                error!("alsa capture error: {e}");
                // バッファオーバーランなどのリカバリを試みる
                pcm.try_recover(e, false)?;
            }
        }
    }
}

fn compute_rms(samples: &[i16]) -> u16 {
    if samples.is_empty() {
        return 0;
    }
    let sum_sq: f64 = samples.iter().map(|&s| (s as f64) * (s as f64)).sum();
    let rms = (sum_sq / samples.len() as f64).sqrt();
    rms.min(u16::MAX as f64) as u16
}
