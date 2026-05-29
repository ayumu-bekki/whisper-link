use std::collections::VecDeque;
use std::time::Duration;
use tracing::warn;

const OPUS_SAMPLE_RATE: u32 = 48000;

#[derive(Debug, Clone)]
pub struct AudioEntry {
    pub request_id: String,
    pub ogg_opus_data: Vec<u8>,
    pub duration: Duration,
}

pub struct AudioQueue {
    inner: VecDeque<AudioEntry>,
    max_size: usize,
    max_duration: Duration,
}

impl AudioQueue {
    pub fn new(max_size: usize, max_duration_secs: u64) -> Self {
        Self {
            inner: VecDeque::new(),
            max_size,
            max_duration: Duration::from_secs(max_duration_secs),
        }
    }

    /// キューへの追加を試みる。
    /// Ok(position) = キュー内の位置 (1始まり)
    /// Err(QueueError) = 拒否理由
    pub fn push(&mut self, request_id: String, ogg_opus_data: Vec<u8>) -> Result<usize, QueueError> {
        let duration = parse_ogg_opus_duration(&ogg_opus_data)
            .map_err(|e| QueueError::ParseError(e))?;

        if duration > self.max_duration {
            warn!(
                request_id = %request_id,
                duration_secs = duration.as_secs_f32(),
                max_secs = self.max_duration.as_secs(),
                "audio too long, rejecting"
            );
            return Err(QueueError::TooLong(duration));
        }

        if self.inner.len() >= self.max_size {
            return Err(QueueError::QueueFull(self.max_size));
        }

        self.inner.push_back(AudioEntry {
            request_id,
            ogg_opus_data,
            duration,
        });

        Ok(self.inner.len())
    }

    pub fn pop(&mut self) -> Option<AudioEntry> {
        self.inner.pop_front()
    }

    #[allow(dead_code)]
    pub fn peek(&self) -> Option<&AudioEntry> {
        self.inner.front()
    }

    #[allow(dead_code)]
    pub fn len(&self) -> usize {
        self.inner.len()
    }

    pub fn is_empty(&self) -> bool {
        self.inner.is_empty()
    }
}

#[derive(Debug, thiserror::Error)]
pub enum QueueError {
    #[error("audio too long: {0:.1?} (max 30s)")]
    TooLong(Duration),
    #[error("queue is full (max {0})")]
    QueueFull(usize),
    #[error("failed to parse ogg opus: {0}")]
    ParseError(String),
}

/// Ogg Opusストリームの再生時間をパースする。
/// Opusはページヘッダのgranule_positionから計算できる。
/// granule_position は 48kHz サンプル数で表現される。
fn parse_ogg_opus_duration(data: &[u8]) -> Result<Duration, String> {
    let mut cursor = std::io::Cursor::new(data);
    let mut reader = ogg::reading::PacketReader::new(&mut cursor);

    let mut last_granule: Option<u64> = None;

    loop {
        match reader.read_packet() {
            Ok(Some(packet)) => {
                if packet.absgp_page() != 0 {
                    last_granule = Some(packet.absgp_page());
                }
            }
            Ok(None) => break,
            Err(e) => return Err(format!("{e}")),
        }
    }

    let granule = last_granule.ok_or_else(|| "no granule position found".to_string())?;
    let duration = Duration::from_secs_f64(granule as f64 / OPUS_SAMPLE_RATE as f64);
    Ok(duration)
}
