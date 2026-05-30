use alsa::pcm::{Access, Format, HwParams, PCM};
use alsa::{Direction, ValueOr};
use ogg::writing::PacketWriter;
use opus::Encoder;
use std::io::Cursor;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};
use tokio::sync::broadcast;
use tracing::{debug, error, info, warn};

const SAMPLE_RATE: u32 = 48000;
const CHANNELS: u32 = 1;
// 20ms フレーム @ 48kHz
const FRAME_SIZE: usize = 960;

/// マイク入力レベルを監視し、音声検知時に録音→Ogg Opusエンコード→ブロードキャストする。
/// `is_recording` フラグで現在録音中かどうかをコントローラから参照できる。
pub struct AudioRecorder {
    pub tx: broadcast::Sender<Vec<u8>>,
    is_recording: Arc<AtomicBool>,
}

impl AudioRecorder {
    pub fn start(
        device: &str,
        threshold_rms: u16,
        silence_duration: Duration,
        max_recording_duration: Duration,
    ) -> Result<Self, Box<dyn std::error::Error + Send + Sync>> {
        let (tx, _) = broadcast::channel::<Vec<u8>>(32);
        let tx_clone = tx.clone();
        let is_recording = Arc::new(AtomicBool::new(false));
        let is_recording_clone = Arc::clone(&is_recording);
        let device = device.to_string();

        std::thread::spawn(move || {
            if let Err(e) = record_loop(
                &device,
                threshold_rms,
                silence_duration,
                max_recording_duration,
                tx_clone,
                &is_recording_clone,
            ) {
                error!("audio recorder error: {e}");
            }
        });

        Ok(Self { tx, is_recording })
    }

    /// 現在マイク入力を録音中かどうか。
    /// コントローラはこのフラグを参照してキューの消費を制御する。
    pub fn is_recording(&self) -> bool {
        self.is_recording.load(Ordering::Relaxed)
    }

    pub fn subscribe(&self) -> broadcast::Receiver<Vec<u8>> {
        self.tx.subscribe()
    }
}

fn record_loop(
    device: &str,
    threshold_rms: u16,
    silence_duration: Duration,
    max_recording_duration: Duration,
    tx: broadcast::Sender<Vec<u8>>,
    is_recording: &AtomicBool,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let pcm = PCM::new(device, Direction::Capture, false)?;

    {
        let hwp = HwParams::any(&pcm)?;
        hwp.set_channels(CHANNELS)?;
        hwp.set_rate(SAMPLE_RATE, ValueOr::Nearest)?;
        hwp.set_format(Format::s16())?;
        hwp.set_access(Access::RWInterleaved)?;
        hwp.set_period_size(FRAME_SIZE as i64, ValueOr::Nearest)?;
        pcm.hw_params(&hwp)?;
    }

    pcm.start()?;
    info!("audio recorder started, monitoring input level");

    let io = pcm.io_i16()?;
    let mut pcm_buf = vec![0i16; FRAME_SIZE];

    // 録音バッファ (PCMサンプル列)
    let mut record_buf: Vec<i16> = Vec::new();
    // 録音状態
    let mut recording = false;
    // 最後にしきい値を超えた時刻
    let mut last_above_threshold: Option<Instant> = None;
    // 録音開始時刻
    let mut record_started_at: Option<Instant> = None;

    loop {
        let frames = match io.readi(&mut pcm_buf) {
            Ok(f) => f,
            Err(e) => {
                error!("alsa capture error: {e}");
                pcm.try_recover(e, false)?;
                continue;
            }
        };

        if frames == 0 {
            continue;
        }

        let samples = &pcm_buf[..frames];
        let rms = compute_rms(samples);

        if rms >= threshold_rms {
            last_above_threshold = Some(Instant::now());

            if !recording {
                // 録音開始
                recording = true;
                is_recording.store(true, Ordering::Relaxed);
                record_buf.clear();
                record_started_at = Some(Instant::now());
                info!("recording started (rms={rms})");
            }
        }

        if recording {
            record_buf.extend_from_slice(samples);

            // 最大録音時間を超えたら強制終了
            if let Some(started) = record_started_at {
                if started.elapsed() >= max_recording_duration {
                    warn!(
                        max_secs = max_recording_duration.as_secs(),
                        "max recording duration reached, flushing"
                    );
                    flush_recording(&record_buf, &tx);
                    recording = false;
                    is_recording.store(false, Ordering::Relaxed);
                    record_buf.clear();
                    last_above_threshold = None;
                    record_started_at = None;
                    continue;
                }
            }

            // しきい値を一定時間下回ったら録音終了
            let silence_elapsed = last_above_threshold
                .map(|t| t.elapsed() >= silence_duration)
                .unwrap_or(true);

            if silence_elapsed {
                info!(
                    samples = record_buf.len(),
                    duration_ms = record_buf.len() as u64 * 1000 / SAMPLE_RATE as u64,
                    "recording finished"
                );
                flush_recording(&record_buf, &tx);
                recording = false;
                is_recording.store(false, Ordering::Relaxed);
                record_buf.clear();
                last_above_threshold = None;
                record_started_at = None;
            }
        }
    }
}

/// 録音済みPCMをOgg Opusにエンコードしてブロードキャストに送る。
fn flush_recording(pcm_samples: &[i16], tx: &broadcast::Sender<Vec<u8>>) {
    match encode_to_ogg_opus(pcm_samples) {
        Ok(ogg_data) => {
            debug!("encoded ogg opus: {} bytes", ogg_data.len());
            if tx.send(ogg_data).is_err() {
                debug!("no receivers connected, dropping recorded audio");
            }
        }
        Err(e) => error!("ogg opus encode error: {e}"),
    }
}

/// PCMサンプル列をOgg Opusバイナリに変換する。
fn encode_to_ogg_opus(samples: &[i16]) -> Result<Vec<u8>, Box<dyn std::error::Error + Send + Sync>> {
    let mut encoder = Encoder::new(SAMPLE_RATE, opus::Channels::Mono, opus::Application::Voip)?;
    let serial: u32 = 0x57485052; // "WHPR"

    let mut buf = Cursor::new(Vec::new());
    let mut pw = PacketWriter::new(&mut buf);

    // OpusHead
    let mut head = Vec::new();
    head.extend_from_slice(b"OpusHead");
    head.push(1); // version
    head.push(1); // channel count (mono)
    head.extend_from_slice(&0u16.to_le_bytes()); // pre-skip
    head.extend_from_slice(&SAMPLE_RATE.to_le_bytes()); // input sample rate
    head.extend_from_slice(&0i16.to_le_bytes()); // output gain
    head.push(0); // channel mapping family
    pw.write_packet(head.into(), serial, ogg::writing::PacketWriteEndInfo::EndPage, 0)?;

    // OpusTags
    let mut tags = Vec::new();
    tags.extend_from_slice(b"OpusTags");
    let vendor = b"whisper-link";
    tags.extend_from_slice(&(vendor.len() as u32).to_le_bytes());
    tags.extend_from_slice(vendor);
    tags.extend_from_slice(&0u32.to_le_bytes());
    pw.write_packet(tags.into(), serial, ogg::writing::PacketWriteEndInfo::EndPage, 0)?;

    // 音声パケット: FRAME_SIZE サンプルずつエンコード
    let mut opus_buf = vec![0u8; 4096];
    let mut granule_pos: u64 = 0;
    let mut chunk_iter = samples.chunks(FRAME_SIZE);
    let mut pending: Option<Vec<u8>> = None;

    loop {
        let chunk = match chunk_iter.next() {
            Some(c) => c,
            None => {
                // 残りのパケットを EOS ページとして書き出す
                if let Some(pkt) = pending.take() {
                    pw.write_packet(
                        pkt.into(),
                        serial,
                        ogg::writing::PacketWriteEndInfo::EndStream,
                        granule_pos,
                    )?;
                }
                break;
            }
        };

        // 端数フレームはゼロパディング
        let encoded_len = if chunk.len() == FRAME_SIZE {
            encoder.encode(chunk, &mut opus_buf)?
        } else {
            let mut padded = vec![0i16; FRAME_SIZE];
            padded[..chunk.len()].copy_from_slice(chunk);
            encoder.encode(&padded, &mut opus_buf)?
        };

        granule_pos += FRAME_SIZE as u64;

        // 前のパケットを NormalPacket として書き出し、今のを pending に
        if let Some(prev) = pending.replace(opus_buf[..encoded_len].to_vec()) {
            pw.write_packet(
                prev.into(),
                serial,
                ogg::writing::PacketWriteEndInfo::NormalPacket,
                0,
            )?;
        }
    }

    Ok(buf.into_inner())
}

fn compute_rms(samples: &[i16]) -> u16 {
    if samples.is_empty() {
        return 0;
    }
    let sum_sq: f64 = samples.iter().map(|&s| (s as f64) * (s as f64)).sum();
    (sum_sq / samples.len() as f64).sqrt().min(u16::MAX as f64) as u16
}
