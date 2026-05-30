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

各コンポーネントの詳細なセットアップ手順はそれぞれのディレクトリのREADMEを参照。

- テストクライアント: [`app/transceiver_test_client/README.md`](app/transceiver_test_client/README.md)

### Raspberry Pi 5 (サーバー)

```bash
sudo apt install libasound2-dev pkg-config
cd app/transceiver_audio_queue
cargo build --release
./target/release/transceiver_audio_queue config.toml
```
