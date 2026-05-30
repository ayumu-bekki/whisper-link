use crate::audio::recorder::AudioRecorder;
use crate::queue::{AudioQueue, QueueError};
use std::sync::{Arc, Mutex};
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tonic::{Request, Response, Status, Streaming};
use tracing::{info, warn, error};

pub mod proto {
    tonic::include_proto!("transceiver");
}

pub use proto::transceiver_service_server::{TransceiverService, TransceiverServiceServer};
pub use proto::AudioChunk;

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

        // レコーダーのブロードキャストを購読
        let mut recorder_rx = self.recorder.subscribe();

        // サーバー→クライアント: レコーダーからの音声を転送するタスク
        let tx_recorder = tx.clone();
        let peer_recv = peer.clone();
        tokio::spawn(async move {
            loop {
                match recorder_rx.recv().await {
                    Ok(ogg_data) => {
                        if tx_recorder
                            .send(Ok(AudioChunk { ogg_opus_data: ogg_data }))
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
        });

        // クライアント→サーバー: 受信した音声をキューに積むタスク
        tokio::spawn(async move {
            loop {
                match inbound.message().await {
                    Ok(Some(chunk)) => {
                        let bytes = chunk.ogg_opus_data.len();
                        let result = {
                            let mut q = queue.lock().unwrap();
                            q.push(chunk.ogg_opus_data)
                        };
                        match result {
                            Ok(pos) => {
                                info!(peer = %peer, bytes, position = pos, "audio queued");
                            }
                            Err(QueueError::TooLong(d)) => {
                                warn!(
                                    peer = %peer,
                                    duration_secs = d.as_secs_f32(),
                                    "audio rejected: too long"
                                );
                            }
                            Err(QueueError::QueueFull(max)) => {
                                warn!(peer = %peer, max, "audio rejected: queue full");
                            }
                            Err(QueueError::ParseError(msg)) => {
                                warn!(peer = %peer, %msg, "audio rejected: parse error");
                            }
                        }
                    }
                    Ok(None) => {
                        info!(peer = %peer, "client closed send stream");
                        break;
                    }
                    Err(e) => {
                        error!(peer = %peer, "inbound stream error: {e}");
                        break;
                    }
                }
            }
        });

        Ok(Response::new(ReceiverStream::new(rx)))
    }
}
