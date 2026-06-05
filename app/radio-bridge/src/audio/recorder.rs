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

const ALSA_SAMPLE_RATE: u32 = 48000; // ALSAデバイスは48kHz固定
const OPUS_SAMPLE_RATE: u32 = 24000; // Opusエンコードは24kHz
const OPUS_BITRATE: i32 = 16000;     // 16kbps
const CAPTURE_CHANNELS: u32 = 2; // ALSAキャプチャはステレオ
const ENCODE_CHANNELS: u32 = 1;  // Opusエンコードはモノラル
// 20ms フレーム @ 24kHz
const FRAME_SIZE: usize = 480;

/// マイク入力レベルを監視し、音声検知時に録音→Ogg Opusエンコード→ブロードキャストする。
/// `is_recording` フラグで現在録音中かどうかをコントローラから参照できる。
pub struct AudioRecorder {
    tx: broadcast::Sender<Vec<u8>>,
    is_recording: Arc<AtomicBool>,
    is_transmitting: Arc<AtomicBool>,
    stop_recording: Arc<AtomicBool>,
}

impl AudioRecorder {
    pub fn start(
        device: &str,
        threshold_rms: u16,
        silence_duration: Duration,
        min_recording_duration: Duration,
        max_recording_duration: Duration,
    ) -> Result<Self, Box<dyn std::error::Error + Send + Sync>> {
        let (tx, _) = broadcast::channel::<Vec<u8>>(32);
        let tx_clone = tx.clone();
        let is_recording = Arc::new(AtomicBool::new(false));
        let is_recording_clone = Arc::clone(&is_recording);
        let is_transmitting = Arc::new(AtomicBool::new(false));
        let is_transmitting_clone = Arc::clone(&is_transmitting);
        let stop_recording = Arc::new(AtomicBool::new(false));
        let stop_recording_clone = Arc::clone(&stop_recording);
        let device = device.to_string();

        std::thread::spawn(move || {
            if let Err(e) = record_loop(
                &device,
                threshold_rms,
                silence_duration,
                min_recording_duration,
                max_recording_duration,
                tx_clone,
                &is_recording_clone,
                &is_transmitting_clone,
                &stop_recording_clone,
            ) {
                error!("audio recorder error: {e}");
            }
        });

        Ok(Self { tx, is_recording, is_transmitting, stop_recording })
    }

    /// 現在マイク入力を録音中かどうか。
    pub fn is_recording(&self) -> bool {
        self.is_recording.load(Ordering::Relaxed)
    }

    /// PTT送信中フラグをセット。送信中は新規録音を開始しない。
    pub fn set_transmitting(&self, transmitting: bool) {
        self.is_transmitting.store(transmitting, Ordering::Relaxed);
    }

    /// 進行中の録音を強制終了する。
    /// レコーダースレッドが次フレームでフラグを検知して is_recording を false にする。
    pub fn force_stop_recording(&self) {
        self.stop_recording.store(true, Ordering::Relaxed);
    }

    pub fn subscribe(&self) -> broadcast::Receiver<Vec<u8>> {
        self.tx.subscribe()
    }
}

fn record_loop(
    device: &str,
    threshold_rms: u16,
    silence_duration: Duration,
    min_recording_duration: Duration,
    max_recording_duration: Duration,
    tx: broadcast::Sender<Vec<u8>>,
    is_recording: &AtomicBool,
    is_transmitting: &AtomicBool,
    stop_recording: &AtomicBool,
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let pcm = PCM::new(device, Direction::Capture, false)?;

    {
        let hwp = HwParams::any(&pcm)?;
        hwp.set_channels(CAPTURE_CHANNELS)?;
        hwp.set_rate(ALSA_SAMPLE_RATE, ValueOr::Nearest)?;
        hwp.set_format(Format::s16())?;
        hwp.set_access(Access::RWInterleaved)?;
        // ALSA は 48kHz なのでフレームサイズも 48kHz 基準 (FRAME_SIZE の 2倍)
        hwp.set_period_size((FRAME_SIZE * 2) as i64, ValueOr::Nearest)?;
        pcm.hw_params(&hwp)?;
    }

    pcm.start()?;
    info!("audio recorder started, monitoring input level");

    let io = pcm.io_i16()?;
    // ステレオインターリーブ (L, R, L, R, ...) で受け取る。ALSAは48kHzなので2倍のフレームサイズ
    let mut pcm_buf = vec![0i16; FRAME_SIZE * 2 * CAPTURE_CHANNELS as usize];

    // 録音バッファ (PCMサンプル列)
    let mut record_buf: Vec<i16> = Vec::new();
    // フレームごとのRMS (末尾無音トリミング用、record_buf を FRAME_SIZE 区切りにしたときの各区間のRMS)
    let mut frame_rms: Vec<u16> = Vec::new();
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

        // 強制終了フラグが立っていたら録音状態をリセット
        if stop_recording.load(Ordering::Relaxed) {
            stop_recording.store(false, Ordering::Relaxed);
            if recording {
                recording = false;
                is_recording.store(false, Ordering::Relaxed);
                record_buf.clear();
                frame_rms.clear();
                last_above_threshold = None;
                record_started_at = None;
            }
            continue;
        }

        // ステレオ→モノラル: L/R を平均してダウンミックス
        let stereo = &pcm_buf[..frames * CAPTURE_CHANNELS as usize];
        let mono_48k: Vec<i16> = stereo
            .chunks(CAPTURE_CHANNELS as usize)
            .map(|ch| ((ch[0] as i32 + ch[1] as i32) / 2) as i16)
            .collect();
        // 48kHz → 24kHz ダウンサンプル (2サンプル平均)
        let samples: Vec<i16> = mono_48k
            .chunks(2)
            .map(|c| if c.len() == 2 { ((c[0] as i32 + c[1] as i32) / 2) as i16 } else { c[0] })
            .collect();
        let samples = samples.as_slice();
        let rms = compute_rms(samples);

        if rms >= threshold_rms && !is_transmitting.load(Ordering::Relaxed) {
            last_above_threshold = Some(Instant::now());

            if !recording {
                recording = true;
                is_recording.store(true, Ordering::Relaxed);
                record_buf.clear();
                frame_rms.clear();
                record_started_at = Some(Instant::now());
                info!("recording started (rms={rms})");
            }
        }

        if recording {
            record_buf.extend_from_slice(samples);
            frame_rms.push(rms);

            // 最大録音時間を超えたら強制終了
            if let Some(started) = record_started_at {
                if started.elapsed() >= max_recording_duration {
                    warn!(
                        max_secs = max_recording_duration.as_secs(),
                        "max recording duration reached, flushing"
                    );
                    let duration_ms = record_buf.len() as u64 * 1000 / OPUS_SAMPLE_RATE as u64;
                    if Duration::from_millis(duration_ms) >= min_recording_duration {
                        // 最大時間到達時はまだ発話中の可能性が高いのでトリミングしない
                        flush_recording(&record_buf, &tx);
                    } else {
                        debug!("recording too short ({duration_ms}ms < {}ms), discarding", min_recording_duration.as_millis());
                    }
                    recording = false;
                    is_recording.store(false, Ordering::Relaxed);
                    record_buf.clear();
                    frame_rms.clear();
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
                // 末尾の無音フレームをトリミング（20ms余白を1フレーム残す）
                let trimmed = trim_trailing_silence(&record_buf, &frame_rms, threshold_rms);
                let duration_ms = trimmed.len() as u64 * 1000 / OPUS_SAMPLE_RATE as u64;
                info!(
                    samples = trimmed.len(),
                    trimmed_from = record_buf.len(),
                    duration_ms,
                    "recording finished"
                );
                if Duration::from_millis(duration_ms) >= min_recording_duration {
                    flush_recording(trimmed, &tx);
                } else {
                    debug!("recording too short ({duration_ms}ms < {}ms), discarding", min_recording_duration.as_millis());
                }
                recording = false;
                is_recording.store(false, Ordering::Relaxed);
                record_buf.clear();
                frame_rms.clear();
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
    debug_assert_eq!(ENCODE_CHANNELS, 1, "encode_to_ogg_opus expects mono input");
    let mut encoder = Encoder::new(OPUS_SAMPLE_RATE, opus::Channels::Mono, opus::Application::Voip)?;
    encoder.set_bitrate(opus::Bitrate::Bits(OPUS_BITRATE))?;
    let serial: u32 = 0x57485052; // "WHPR"

    let mut buf = Cursor::new(Vec::new());
    let mut pw = PacketWriter::new(&mut buf);

    // OpusHead
    let mut head = Vec::new();
    head.extend_from_slice(b"OpusHead");
    head.push(1); // version
    head.push(1); // channel count (mono)
    head.extend_from_slice(&0u16.to_le_bytes()); // pre-skip
    head.extend_from_slice(&OPUS_SAMPLE_RATE.to_le_bytes()); // input sample rate
    head.extend_from_slice(&0i16.to_le_bytes()); // output gain
    head.push(0); // channel mapping family
    pw.write_packet(head, serial, ogg::writing::PacketWriteEndInfo::EndPage, 0)?;

    // OpusTags
    let mut tags = Vec::new();
    tags.extend_from_slice(b"OpusTags");
    let vendor = b"whisper-link";
    tags.extend_from_slice(&(vendor.len() as u32).to_le_bytes());
    tags.extend_from_slice(vendor);
    tags.extend_from_slice(&0u32.to_le_bytes());
    pw.write_packet(tags, serial, ogg::writing::PacketWriteEndInfo::EndPage, 0)?;

    // 音声パケット: FRAME_SIZE サンプルずつエンコード
    let mut opus_buf = vec![0u8; 4096];
    let mut granule_pos: u64 = 0;
    let mut chunk_iter = samples.chunks(FRAME_SIZE);
    let mut pending: Option<Vec<u8>> = None;

    loop {
        let chunk = match chunk_iter.next() {
            Some(c) => c,
            None => {
                if let Some(pkt) = pending.take() {
                    pw.write_packet(
                        pkt,
                        serial,
                        ogg::writing::PacketWriteEndInfo::EndStream,
                        granule_pos,
                    )?;
                }
                break;
            }
        };

        let encoded_len = if chunk.len() == FRAME_SIZE {
            encoder.encode(chunk, &mut opus_buf)?
        } else {
            let mut padded = vec![0i16; FRAME_SIZE];
            padded[..chunk.len()].copy_from_slice(chunk);
            encoder.encode(&padded, &mut opus_buf)?
        };

        granule_pos += FRAME_SIZE as u64;

        if let Some(prev) = pending.replace(opus_buf[..encoded_len].to_vec()) {
            pw.write_packet(
                prev,
                serial,
                ogg::writing::PacketWriteEndInfo::NormalPacket,
                0,
            )?;
        }
    }

    Ok(buf.into_inner())
}

/// 録音末尾の無音区間（RMSが閾値未満のフレーム）をトリミングする。
/// 発話直後の不自然な切れを防ぐため、末尾に TRAIL_FRAMES 分の余白を残す。
/// `frame_rms` は `record_buf` を FRAME_SIZE 区切りにした各区間のRMS。
fn trim_trailing_silence<'a>(record_buf: &'a [i16], frame_rms: &[u16], threshold_rms: u16) -> &'a [i16] {
    // 20ms 余白（= 1 フレーム）を残す
    const TRAIL_FRAMES: usize = 1;

    if frame_rms.is_empty() {
        return record_buf;
    }

    // 末尾から、閾値未満のフレームを TRAIL_FRAMES を超えない範囲で削る
    let mut last = frame_rms.len() - 1;
    while last > TRAIL_FRAMES && frame_rms[last] < threshold_rms {
        last -= 1;
    }

    // フレーム数 last+1 に対応するサンプル数（最終フレームは端数になりうるので buf 長で上限）
    let keep_samples = ((last + 1) * FRAME_SIZE).min(record_buf.len());
    &record_buf[..keep_samples]
}

fn compute_rms(samples: &[i16]) -> u16 {
    if samples.is_empty() {
        return 0;
    }
    let sum_sq: f64 = samples.iter().map(|&s| (s as f64) * (s as f64)).sum();
    (sum_sq / samples.len() as f64).sqrt().min(u16::MAX as f64) as u16
}
