use alsa::pcm::{Access, Format, HwParams, PCM};
use alsa::{Direction, ValueOr};
use ogg::writing::PacketWriter;
use opus::Encoder;
use std::io::Cursor;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
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
        dump_ogg_dir: Option<PathBuf>,
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
                dump_ogg_dir,
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
    dump_ogg_dir: Option<PathBuf>,
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
                    let duration_ms = record_buf.len() as u64 * 1000 / OPUS_SAMPLE_RATE as u64;
                    if Duration::from_millis(duration_ms) >= min_recording_duration {
                        flush_recording(&record_buf, dump_ogg_dir.as_deref(), &tx);
                    } else {
                        debug!("recording too short ({duration_ms}ms < {}ms), discarding", min_recording_duration.as_millis());
                    }
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
                let duration_ms = record_buf.len() as u64 * 1000 / OPUS_SAMPLE_RATE as u64;
                info!(samples = record_buf.len(), duration_ms, "recording finished");
                if Duration::from_millis(duration_ms) >= min_recording_duration {
                    flush_recording(&record_buf, dump_ogg_dir.as_deref(), &tx);
                } else {
                    debug!("recording too short ({duration_ms}ms < {}ms), discarding", min_recording_duration.as_millis());
                }
                recording = false;
                is_recording.store(false, Ordering::Relaxed);
                record_buf.clear();
                last_above_threshold = None;
                record_started_at = None;
            }
        }
    }
}

static DUMP_COUNTER: AtomicU64 = AtomicU64::new(0);

/// 録音済みPCMをOgg Opusにエンコードしてブロードキャストに送る。
/// `dump_dir` が Some のときはエンコード結果を連番ファイルとして保存する。
fn flush_recording(pcm_samples: &[i16], dump_dir: Option<&std::path::Path>, tx: &broadcast::Sender<Vec<u8>>) {
    match encode_to_ogg_opus(pcm_samples) {
        Ok(ogg_data) => {
            debug!("encoded ogg opus: {} bytes", ogg_data.len());
            if let Some(dir) = dump_dir {
                let n = DUMP_COUNTER.fetch_add(1, Ordering::Relaxed);
                let path = dir.join(format!("rec_{n:05}.ogg"));
                match std::fs::create_dir_all(dir) {
                    Ok(()) => {
                        if let Err(e) = std::fs::write(&path, &ogg_data) {
                            warn!(error = %e, "failed to dump Ogg");
                        } else {
                            info!(path = %path.display(), "dumped Ogg Opus");
                        }
                    }
                    Err(e) => warn!(error = %e, "failed to create dump dir"),
                }
            }
            if tx.send(ogg_data).is_err() {
                debug!("no receivers connected, dropping recorded audio");
            }
        }
        Err(e) => error!("ogg opus encode error: {e}"),
    }
}

/// PCMサンプル列をOgg Opusバイナリに変換する。
///
/// Ogg の構造は radio-bridge-emulator (audio.go の encodeToOggOpus) と一致させる。
/// すなわち pre-skip=312 を宣言し、各音声パケットに累積グラニュール
/// (pre_skip + 累積サンプル数) を付与し、最後のパケットで EndStream を立てる。
/// 中間パケットのグラニュールを 0 にしてしまうとデコーダがストリーム長を誤認し
/// 末尾(や先頭)を切り詰めるため、エミュレーター同様パケット毎に正しい値を入れる。
fn encode_to_ogg_opus(samples: &[i16]) -> Result<Vec<u8>, Box<dyn std::error::Error + Send + Sync>> {
    debug_assert_eq!(ENCODE_CHANNELS, 1, "encode_to_ogg_opus expects mono input");
    const PRE_SKIP: u16 = 312; // Opus standard pre-skip (emulator と一致)

    // 入力振幅正規化: Mac ハードウェア AGC の代替。
    // クリッピング (max=0dBFS) が Opus VoIP の AGC/VAD を誤動作させ、音声中間部を無音化する。
    // ピークが閾値を超えていればスケールダウンし、VoIP エンコーダに適切なレベルを渡す。
    const NORMALIZE_PEAK: f32 = 24576.0; // -2.5 dBFS
    let max_abs = samples.iter().map(|&s| s.unsigned_abs() as f32).fold(0.0_f32, f32::max);
    let normalized_buf: Vec<i16>;
    let samples: &[i16] = if max_abs > NORMALIZE_PEAK {
        let scale = NORMALIZE_PEAK / max_abs;
        normalized_buf = samples.iter().map(|&s| (s as f32 * scale) as i16).collect();
        &normalized_buf
    } else {
        samples
    };

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
    head.extend_from_slice(&PRE_SKIP.to_le_bytes()); // pre-skip
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

    // 音声パケット: FRAME_SIZE サンプルずつエンコードし、各パケットに累積グラニュールを付ける。
    // 最後のパケットは EndStream (EOS) を立てる。ページ分割は行わない。
    //
    // Rust の ogg クレートは granule をそのまま Ogg ページヘッダに書く (48kHz 単位で解釈される)。
    // kazzmir (Go) は内部で ×2 変換するため 480 を渡すが、ここでは 960 を直接渡す。
    // 480 を渡すと OGG の再生時間が WAV の半分になり、デコーダーが途中で打ち切る。
    const GRANULE_PER_FRAME: u64 = FRAME_SIZE as u64 * (ALSA_SAMPLE_RATE as u64 / OPUS_SAMPLE_RATE as u64); // 480 * 2 = 960
    let mut opus_buf = vec![0u8; 4096];
    let chunks: Vec<&[i16]> = samples.chunks(FRAME_SIZE).collect();
    let n = chunks.len();
    let mut total_samples: u64 = 0;

    for (i, chunk) in chunks.iter().enumerate() {
        let encoded_len = if chunk.len() == FRAME_SIZE {
            encoder.encode(chunk, &mut opus_buf)?
        } else {
            let mut padded = vec![0i16; FRAME_SIZE];
            padded[..chunk.len()].copy_from_slice(chunk);
            encoder.encode(&padded, &mut opus_buf)?
        };

        total_samples += GRANULE_PER_FRAME;
        let granule = PRE_SKIP as u64 + total_samples;
        let info = if i == n - 1 {
            ogg::writing::PacketWriteEndInfo::EndStream
        } else {
            ogg::writing::PacketWriteEndInfo::NormalPacket
        };
        pw.write_packet(opus_buf[..encoded_len].to_vec(), serial, info, granule)?;
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
