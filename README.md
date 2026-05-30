# whisper-link

トランシーバーをRaspberry Pi 5で制御し、gRPC経由で音声を送受信するシステム。

## 構成

| ディレクトリ | 内容 |
|---|---|
| `app/radio-bridge/` | Rustサーバー (Raspberry Pi 5) |
| `app/radio-bridge-test-client/` | Goテストクライアント (macOS) |
| `app/proto/` | Protocol Buffers定義 |
| `pcb/` | KiCad PCB設計ファイル |

---

## セットアップ

各コンポーネントの詳細なセットアップ手順はそれぞれのディレクトリのREADMEを参照。

- テストクライアント: [`app/radio-bridge-test-client/README.md`](app/radio-bridge-test-client/README.md)

### Raspberry Pi 5 (サーバー)

```bash
sudo apt install libasound2-dev pkg-config
cd app/radio-bridge
cargo build --release
./target/release/radio-bridge config.toml
```
