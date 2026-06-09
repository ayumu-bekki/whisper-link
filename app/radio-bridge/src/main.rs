mod audio;
mod config;
mod controller;
mod grpc;
mod queue;

use crate::audio::recorder::AudioRecorder;
use crate::config::Config;
use crate::controller::Controller;
use crate::grpc::handler::{TransceiverHandler, TransceiverServiceServer};
use crate::queue::AudioQueue;
use std::path::Path;
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tonic::transport::Server;
use tracing::info;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::from_default_env()
                .add_directive("radio_bridge=debug".parse()?),
        )
        .init();

    let config_path = std::env::args()
        .nth(1)
        .unwrap_or_else(|| "config.toml".to_string());

    let config = Config::load(Path::new(&config_path))?;
    info!("loaded config from {config_path}");
    info!("listening on {}", config.server.listen_addr);

    let queue = Arc::new(Mutex::new(AudioQueue::new(
        config.queue.max_queue_size,
        config.queue.max_audio_duration_secs,
    )));

    let dump_ogg_dir = config.audio.dump_ogg_enabled
        .then(|| std::path::PathBuf::from(&config.audio.dump_ogg_dir));
    let recorder = Arc::new(
        AudioRecorder::start(
            &config.audio.input_device,
            config.audio.input_threshold_rms,
            Duration::from_millis(config.audio.input_silence_ms),
            Duration::from_millis(config.audio.input_min_recording_ms),
            Duration::from_secs(config.audio.input_max_recording_secs),
            dump_ogg_dir,
        )?,
    );
    info!("audio recorder started on device: {}", config.audio.input_device);

    let handler = TransceiverHandler::new(Arc::clone(&queue), Arc::clone(&recorder));
    let addr = config.server.listen_addr.parse()?;

    let controller = Controller::new(Arc::clone(&queue), Arc::clone(&recorder), config);
    let controller_task = tokio::spawn(async move {
        if let Err(e) = controller.run().await {
            tracing::error!("controller error: {e}");
        }
    });

    info!("gRPC server starting on {addr}");
    let grpc_task = tokio::spawn(async move {
        Server::builder()
            .add_service(TransceiverServiceServer::new(handler))
            .serve(addr)
            .await
            .expect("gRPC server error");
    });

    tokio::select! {
        _ = controller_task => tracing::error!("controller task exited unexpectedly"),
        _ = grpc_task => tracing::error!("gRPC server task exited unexpectedly"),
    }

    Ok(())
}
