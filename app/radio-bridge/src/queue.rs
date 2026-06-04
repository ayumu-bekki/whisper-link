use std::collections::{HashMap, HashSet, VecDeque};
use std::time::Duration;
use tracing::warn;

const OPUS_SAMPLE_RATE: u32 = 48000;

/// クローズ済み stream_id を覚えておく最大件数。
/// これを超えると古いものから忘れる (際限なく溜めない)。
const MAX_CLOSED_STREAMS: usize = 16;

/// AudioChunk の再生まとまりを示すステータス。
/// proto の `StreamStatus` と対応する (handler で変換)。queue/controller を
/// gRPC 層に依存させないため、ここに独立した型として定義する。
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StreamStatus {
    /// 単発完結再生。1 チャンク = 1 再生サイクル。
    /// (UNKNOWN/未設定は無効として handler で破棄されるため、ここには来ない。)
    Oneshot,
    /// 連続再生ストリームの開始チャンク。
    Start,
    /// 連続再生ストリームの途中チャンク。
    Continue,
    /// 連続再生ストリームの終了チャンク。
    End,
}

#[derive(Debug, Clone)]
pub struct AudioEntry {
    pub ogg_opus_data: Vec<u8>,
    pub duration: Duration,
    pub status: StreamStatus,
}

/// 受信音声を「再生単位 (stream_id)」ごとにまとめて保持するキュー。
///
/// - `order`: 再生する stream_id の順序 (FIFO)。同一 stream_id は 1 回だけ並ぶ。
/// - `streams`: stream_id → その stream のチャンク列 (到着順)。
///
/// 「現在再生中のストリーム」は常に `order` の先頭。controller はその先頭ストリームの
/// チャンクだけを取り出して連続再生するため、別 stream_id のチャンクが後から届いても
/// 別キー (`streams`) に溜まるだけで、再生中のストリームに割り込むことはない。
///
/// ONESHOT は handler 側で内部一意 ID を stream_id として振り、1 チャンクのストリームと
/// して同じ仕組みに載せる (queue は stream_id を一律に扱う)。
pub struct AudioQueue {
    order: VecDeque<String>,
    streams: HashMap<String, VecDeque<AudioEntry>>,
    /// 同時に保持できる stream 数の上限 (= 旧 max_queue_size)。
    max_streams: usize,
    max_duration: Duration,
    /// END 到達 / タイムアウトで閉じた stream_id。以後同じ id の push は破棄する。
    closed_order: VecDeque<String>,
    closed_set: HashSet<String>,
}

impl AudioQueue {
    pub fn new(max_streams: usize, max_duration_secs: u64) -> Self {
        Self {
            order: VecDeque::new(),
            streams: HashMap::new(),
            max_streams,
            max_duration: Duration::from_secs(max_duration_secs),
            closed_order: VecDeque::new(),
            closed_set: HashSet::new(),
        }
    }

    /// チャンクを stream_id 配下に積む。
    /// Ok(position) = その stream のチャンク数 (1始まり)
    /// Err(QueueError) = 拒否理由
    ///
    /// stream_id は呼び出し側で必ず非空にすること (ONESHOT も内部 ID を振る)。
    pub fn push(
        &mut self,
        ogg_opus_data: Vec<u8>,
        status: StreamStatus,
        stream_id: String,
    ) -> Result<usize, QueueError> {
        // クローズ済みストリームの遅延チャンクは破棄する。
        if self.closed_set.contains(&stream_id) {
            return Err(QueueError::StreamClosed(stream_id));
        }

        // duration 上限はチャンク単位でチェックする。
        let duration = parse_ogg_opus_duration(&ogg_opus_data)
            .map_err(QueueError::ParseError)?;
        if duration > self.max_duration {
            warn!(
                duration_secs = duration.as_secs_f32(),
                max_secs = self.max_duration.as_secs(),
                "audio too long, rejecting"
            );
            return Err(QueueError::TooLong(duration));
        }

        let is_new_stream = !self.streams.contains_key(&stream_id);
        // 新規ストリームは max_streams 上限でチェック (= 同時に溜められるストリーム数)。
        if is_new_stream && self.order.len() >= self.max_streams {
            return Err(QueueError::QueueFull(self.max_streams));
        }

        let entry = AudioEntry { ogg_opus_data, duration, status };
        if is_new_stream {
            self.order.push_back(stream_id.clone());
            let mut chunks = VecDeque::new();
            chunks.push_back(entry);
            self.streams.insert(stream_id, chunks);
            Ok(1)
        } else {
            let chunks = self.streams.get_mut(&stream_id).unwrap();
            chunks.push_back(entry);
            Ok(chunks.len())
        }
    }

    /// 現在再生中のストリーム (order 先頭) の stream_id。
    pub fn current_stream(&self) -> Option<&str> {
        self.order.front().map(|s| s.as_str())
    }

    /// 現在のストリームに未再生チャンクが残っているか。
    pub fn current_has_chunk(&self) -> bool {
        match self.order.front() {
            Some(id) => self.streams.get(id).map(|c| !c.is_empty()).unwrap_or(false),
            None => false,
        }
    }

    /// 現在のストリームのチャンクを 1 つ取り出す。
    /// ストリームが無い / 空のときは None。
    pub fn pop_current(&mut self) -> Option<AudioEntry> {
        let id = self.order.front()?.clone();
        let chunks = self.streams.get_mut(&id)?;
        chunks.pop_front()
    }

    /// 現在のストリームを再生完了として閉じる。
    /// order 先頭から外し、`streams` から削除し、closed に記録する
    /// (以後その stream_id の push は破棄される)。
    pub fn finish_current(&mut self) {
        if let Some(id) = self.order.pop_front() {
            self.streams.remove(&id);
            self.mark_closed(&id);
        }
    }

    /// 再生対象のストリームがまだあるか (order が空でない)。
    pub fn has_stream(&self) -> bool {
        !self.order.is_empty()
    }

    /// closed 集合に stream_id を記録する (直近 MAX_CLOSED_STREAMS 件のみ保持)。
    fn mark_closed(&mut self, stream_id: &str) {
        if stream_id.is_empty() || self.closed_set.contains(stream_id) {
            return;
        }
        self.closed_set.insert(stream_id.to_string());
        self.closed_order.push_back(stream_id.to_string());
        while self.closed_order.len() > MAX_CLOSED_STREAMS {
            if let Some(old) = self.closed_order.pop_front() {
                self.closed_set.remove(&old);
            }
        }
    }
}

#[derive(Debug, thiserror::Error)]
pub enum QueueError {
    #[error("audio too long: {0:.1?} (max 30s)")]
    TooLong(Duration),
    #[error("too many streams queued (max {0})")]
    QueueFull(usize),
    #[error("failed to parse ogg opus: {0}")]
    ParseError(String),
    #[error("stream already closed: {0}")]
    StreamClosed(String),
}

/// Ogg Opusストリームの再生時間をgranule_positionからパースする。
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn closed_stream_push_is_discarded() {
        let mut q = AudioQueue::new(10, 30);
        // ストリームを閉じる: まず push して finish_current で閉じる。
        // (ParseError を避けるためここでは closed_set を直接使う代わりに mark_closed 経由。)
        q.mark_closed("abc");
        let err = q
            .push(vec![0, 1, 2], StreamStatus::Continue, "abc".to_string())
            .unwrap_err();
        assert!(matches!(err, QueueError::StreamClosed(id) if id == "abc"));
    }

    #[test]
    fn separate_streams_do_not_interleave() {
        // 不正 ogg なので push は ParseError になるが、ここでは順序管理だけ確認したいので
        // duration パースを通すダミーを使えない。代わりに order 操作を直接検証する。
        let mut q = AudioQueue::new(10, 30);
        // streams/order を直接組み立てて pop_current の挙動を確認。
        q.order.push_back("s1".to_string());
        q.order.push_back("s2".to_string());
        let mut c1 = VecDeque::new();
        c1.push_back(AudioEntry { ogg_opus_data: vec![1], duration: Duration::ZERO, status: StreamStatus::Start });
        c1.push_back(AudioEntry { ogg_opus_data: vec![2], duration: Duration::ZERO, status: StreamStatus::End });
        q.streams.insert("s1".to_string(), c1);
        let mut c2 = VecDeque::new();
        c2.push_back(AudioEntry { ogg_opus_data: vec![3], duration: Duration::ZERO, status: StreamStatus::Oneshot });
        q.streams.insert("s2".to_string(), c2);

        // 現在のストリームは s1。s1 のチャンクだけが順に取れる。
        assert_eq!(q.current_stream(), Some("s1"));
        assert_eq!(q.pop_current().unwrap().ogg_opus_data, vec![1]);
        assert_eq!(q.pop_current().unwrap().ogg_opus_data, vec![2]);
        assert!(!q.current_has_chunk());
        // s1 を閉じると次は s2。
        q.finish_current();
        assert_eq!(q.current_stream(), Some("s2"));
        assert_eq!(q.pop_current().unwrap().ogg_opus_data, vec![3]);
    }

    #[test]
    fn closed_streams_are_bounded() {
        let mut q = AudioQueue::new(10, 30);
        for i in 0..(MAX_CLOSED_STREAMS + 5) {
            q.mark_closed(&format!("s{i}"));
        }
        assert_eq!(q.closed_order.len(), MAX_CLOSED_STREAMS);
        assert_eq!(q.closed_set.len(), MAX_CLOSED_STREAMS);
        assert!(!q.closed_set.contains("s0"));
        assert!(q.closed_set.contains(&format!("s{}", MAX_CLOSED_STREAMS + 4)));
    }
}
