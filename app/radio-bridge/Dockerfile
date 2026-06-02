FROM rust:1-slim-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    protobuf-compiler \
    libasound2-dev \
    libopus-dev \
    pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build
COPY proto/ proto/
COPY radio-bridge/ radio-bridge/

WORKDIR /build/radio-bridge
RUN cargo build --release

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    libasound2 \
    libopus0 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /build/radio-bridge/target/release/radio-bridge /usr/local/bin/radio-bridge

WORKDIR /app
CMD ["radio-bridge", "config.toml"]
