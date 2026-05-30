# whisper-link

トランシーバーをRaspberry Pi 5で制御し、gRPC経由で音声を送受信するシステム。

## 構成

| ディレクトリ | 内容 |
|---|---|
| `app/transceiver_audio_queue/` | Rustサーバー (Raspberry Pi 5) |
| `app/transceiver_test_client/` | Goテストクライアント (macOS) |
| `app/proto/` | Protocol Buffers定義 |
| `pcb/` | KiCad PCB設計ファイル |

---

## セットアップ

### macOS (テストクライアント)

MacPortsで以下をインストールする。

```bash
sudo port install protobuf pkgconfig portaudio libopus
```

| パッケージ | 用途 |
|---|---|
| `protobuf` | `protoc` コンパイラ (protoファイルからGoコード生成) |
| `pkgconfig` | CライブラリのビルドフラグをGoに渡す |
| `portaudio` | macOSの音声入出力 |
| `libopus` | Opusコーデック |

#### ビルド

```bash
cd app/transceiver_test_client
PKG_CONFIG_PATH="/opt/local/lib/pkgconfig" go build -o transceiver_test_client .
```

#### 実行

```bash
./transceiver_test_client <host> <port>
# 例: ./transceiver_test_client 192.168.1.10 50051
```

- スペースキーを押している間録音し、離すとサーバーへ送信
- サーバーからの音声は随時受信して再生
- Ctrl+C で終了

#### protoファイルを変更した場合

```bash
cd app/transceiver_test_client
go generate
```

---

### Raspberry Pi 5 (サーバー)

```bash
sudo apt install libasound2-dev pkg-config
```

#### ビルド

```bash
cd app/transceiver_audio_queue
cargo build --release
```

#### 実行

```bash
./target/release/transceiver_audio_queue config.toml
```
