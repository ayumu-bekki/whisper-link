use crate::audio::recorder::AudioRecorder;
use crate::queue::{AudioQueue, QueueError, StreamStatus};
use std::sync::{Arc, Mutex, atomic::{AtomicU64, Ordering}};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;
use tokio_stream::wrappers::ReceiverStream;
use tonic::{Request, Response, Status, Streaming};
use tracing::{info, warn, error};

pub mod proto {
    tonic::include_proto!("transceiver");
}

pub use proto::transceiver_service_server::{TransceiverService, TransceiverServiceServer};
pub use proto::AudioChunk;

/// proto の status (i32) を queue 層の StreamStatus に変換する。
/// UNKNOWN(0) / 未知値は無効として None を返し、呼び出し側で破棄する
/// (送信側は ONESHOT/START/CONTINUE/END を明示する必要がある)。
fn map_status(raw: i32) -> Option<StreamStatus> {
    match proto::StreamStatus::try_from(raw) {
        Ok(proto::StreamStatus::Oneshot) => Some(StreamStatus::Oneshot),
        Ok(proto::StreamStatus::Start) => Some(StreamStatus::Start),
        Ok(proto::StreamStatus::Continue) => Some(StreamStatus::Continue),
        Ok(proto::StreamStatus::End) => Some(StreamStatus::End),
        // UNKNOWN / 未知値は無効。
        _ => None,
    }
}

pub struct TransceiverHandler {
    queue: Arc<Mutex<AudioQueue>>,
    recorder: Arc<AudioRecorder>,
}

impl TransceiverHandler {
    pub fn new(queue: Arc<Mutex<AudioQueue>>, recorder: Arc<AudioRecorder>) -> Self {
        Self { queue, recorder }
    }
}

#[tonic::async_trait]
impl TransceiverService for TransceiverHandler {
    type ConnectStream = ReceiverStream<Result<AudioChunk, Status>>;

    async fn connect(
        &self,
        request: Request<Streaming<AudioChunk>>,
    ) -> Result<Response<Self::ConnectStream>, Status> {
        let peer = request
            .remote_addr()
            .map(|a| a.to_string())
            .unwrap_or_else(|| "unknown".to_string());
        info!(peer = %peer, "client connected");

        let mut inbound = request.into_inner();
        let queue = Arc::clone(&self.queue);

        // サーバー→クライアント送信チャンネル
        let (tx, rx) = mpsc::channel::<Result<AudioChunk, Status>>(32);

        // ONESHOT チャンクに振る接続スコープの連番。
        // uuid 依存を避けるため peer + カウンタで内部 ID を生成する。
        let oneshot_counter = Arc::new(AtomicU64::new(0));

        // レコーダーのブロードキャストを購読
        let mut recorder_rx = self.recorder.subscribe();

        // どちらかのタスクが終了したらもう一方もキャンセルする
        let cancel = CancellationToken::new();

        // サーバー→クライアント: レコーダーからの音声を転送するタスク
        let tx_recorder = tx.clone();
        let peer_recv = peer.clone();
        let cancel_recv = cancel.clone();
        tokio::spawn(async move {
            loop {
                tokio::select! {
                    result = recorder_rx.recv() => {
                        match result {
                            Ok(ogg_data) => {
                                if tx_recorder
                                    .send(Ok(AudioChunk {
                                        ogg_opus_data: ogg_data,
                                        // マイク受信音声は単発再生扱い。
                                        status: proto::StreamStatus::Oneshot as i32,
                                        stream_id: String::new(),
                                    }))
                                    .await
                                    .is_err()
                                {
                                    info!(peer = %peer_recv, "client disconnected (recorder stream)");
                                    break;
                                }
                            }
                            Err(tokio::sync::broadcast::error::RecvError::Lagged(n)) => {
                                warn!(peer = %peer_recv, skipped = n, "recorder broadcast lagged");
                            }
                            Err(tokio::sync::broadcast::error::RecvError::Closed) => {
                                error!(peer = %peer_recv, "recorder broadcast closed");
                                break;
                            }
                        }
                    }
                    _ = cancel_recv.cancelled() => {
                        break;
                    }
                }
            }
            cancel_recv.cancel();
        });

        // クライアント→サーバー: 受信した音声をキューに積むタスク
        let cancel_send = cancel.clone();
        let peer_send = peer.clone();
        tokio::spawn(async move {
            loop {
                tokio::select! {
                    msg = inbound.message() => {
                        match msg {
                            Ok(Some(chunk)) => {
                                let bytes = chunk.ogg_opus_data.len();
                                let status = match map_status(chunk.status) {
                                    Some(s) => s,
                                    None => {
                                        // UNKNOWN / 未知値は無効タイプとして破棄。
                                        warn!(
                                            peer = %peer_send,
                                            raw_status = chunk.status,
                                            "audio discarded: invalid status (UNKNOWN)"
                                        );
                                        continue;
                                    }
                                };
                                // ONESHOT は stream_id を内部生成する (空文字列をキーにしない)。
                                // stream 系 (START/CONTINUE/END) で stream_id が空なら破棄する。
                                let stream_id = match status {
                                    StreamStatus::Oneshot => {
                                        let n = oneshot_counter.fetch_add(1, Ordering::Relaxed);
                                        format!("__oneshot_{peer_send}_{n}")
                                    }
                                    _ => {
                                        if chunk.stream_id.is_empty() {
                                            warn!(
                                                peer = %peer_send,
                                                ?status,
                                                "audio discarded: stream_id is empty for stream chunk"
                                            );
                                            continue;
                                        }
                                        chunk.stream_id
                                    }
                                };
                                let result = {
                                    let mut q = queue.lock().unwrap();
                                    q.push(chunk.ogg_opus_data, status, stream_id)
                                };
                                match result {
                                    Ok(pos) => {
                                        info!(peer = %peer_send, bytes, position = pos, ?status, "audio queued");
                                    }
                                    Err(QueueError::TooLong(d)) => {
                                        warn!(
                                            peer = %peer_send,
                                            duration_secs = d.as_secs_f32(),
                                            "audio rejected: too long"
                                        );
                                    }
                                    Err(QueueError::QueueFull(max)) => {
                                        warn!(peer = %peer_send, max, "audio rejected: queue full");
                                    }
                                    Err(QueueError::ParseError(msg)) => {
                                        warn!(peer = %peer_send, %msg, "audio rejected: parse error");
                                    }
                                    Err(QueueError::StreamClosed(id)) => {
                                        warn!(peer = %peer_send, stream_id = %id, "audio discarded: stream closed");
                                    }
                                }
                            }
                            Ok(None) => {
                                info!(peer = %peer_send, "client closed send stream");
                                break;
                            }
                            Err(e) => {
                                error!(peer = %peer_send, "inbound stream error: {e}");
                                break;
                            }
                        }
                    }
                    _ = cancel_send.cancelled() => {
                        break;
                    }
                }
            }
            cancel_send.cancel();
        });

        Ok(Response::new(ReceiverStream::new(rx)))
    }
}
