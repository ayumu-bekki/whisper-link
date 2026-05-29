use crate::queue::{AudioQueue, QueueError};
use std::sync::{Arc, Mutex};
use tonic::{Request, Response, Status};
use tracing::{info, warn};

pub mod proto {
    tonic::include_proto!("transceiver");
}

pub use proto::transceiver_service_server::{TransceiverService, TransceiverServiceServer};
pub use proto::{AudioChunk, SendAudioResponse};

pub struct TransceiverHandler {
    queue: Arc<Mutex<AudioQueue>>,
}

impl TransceiverHandler {
    pub fn new(queue: Arc<Mutex<AudioQueue>>) -> Self {
        Self { queue }
    }
}

#[tonic::async_trait]
impl TransceiverService for TransceiverHandler {
    async fn send_audio(
        &self,
        request: Request<AudioChunk>,
    ) -> Result<Response<SendAudioResponse>, Status> {
        let chunk = request.into_inner();
        let request_id = if chunk.request_id.is_empty() {
            uuid_simple()
        } else {
            chunk.request_id.clone()
        };

        info!(request_id = %request_id, bytes = chunk.ogg_opus_data.len(), "received audio");

        let result = {
            let mut q = self.queue.lock().map_err(|_| Status::internal("queue lock poisoned"))?;
            q.push(request_id.clone(), chunk.ogg_opus_data)
        };

        match result {
            Ok(position) => {
                info!(request_id = %request_id, position, "audio queued");
                Ok(Response::new(SendAudioResponse {
                    request_id,
                    message: format!("queued (position: {position})"),
                }))
            }
            Err(QueueError::TooLong(duration)) => {
                warn!(
                    request_id = %request_id,
                    duration_secs = duration.as_secs_f32(),
                    "audio rejected: too long"
                );
                Err(Status::invalid_argument(format!(
                    "audio too long ({:.1}s, max 30s)",
                    duration.as_secs_f32()
                )))
            }
            Err(QueueError::QueueFull(max)) => {
                warn!(request_id = %request_id, max, "audio rejected: queue full");
                Err(Status::resource_exhausted(format!(
                    "queue is full (max {max})"
                )))
            }
            Err(QueueError::ParseError(msg)) => {
                warn!(request_id = %request_id, %msg, "audio rejected: parse error");
                Err(Status::invalid_argument(format!(
                    "failed to parse ogg opus: {msg}"
                )))
            }
        }
    }
}

/// 簡易UUID生成 (外部クレートなし)
fn uuid_simple() -> String {
    use std::time::{SystemTime, UNIX_EPOCH};
    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .subsec_nanos();
    format!("req-{nanos:08x}")
}
