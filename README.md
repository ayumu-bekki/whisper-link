# whisper-link

トランシーバーをRaspberry Pi 5で制御し、gRPC経由で音声を送受信するシステム。

## 構成

| ディレクトリ | 内容 |
|---|---|
| `app/radio-bridge/` | Rustサーバー (Raspberry Pi 5) |
| `app/radio-bridge-emulator/` | radio-bridge エミュレーター (macOS) |
| `app/radio-bridge-test-client/` | Goテストクライアント (macOS) |
| `app/wl-game-server/` | GoゲームサーバーGemini連携 (macOS/Linux) |
| `app/proto/` | Protocol Buffers定義 |
| `pcb/` | KiCad PCB設計ファイル |

---

## セットアップ

各コンポーネントの詳細なセットアップ手順はそれぞれのディレクトリのREADMEを参照。

- radio-bridge: [`app/radio-bridge/README.md`](app/radio-bridge/README.md)
- テストクライアント: [`app/radio-bridge-test-client/README.md`](app/radio-bridge-test-client/README.md)

### Raspberry Pi 5 (radio-bridge)

```bash
sudo apt install libasound2-dev pkg-config
cd app/radio-bridge
cargo build --release
./target/release/radio-bridge config.toml
```

### macOS エミュレーター (radio-bridge-emulator)

`radio-bridge` は ALSA に依存しているため Raspberry Pi 専用となっている。
macOS 向けのクロスプラットフォーム対応として `cpal` クレートへの移行を試みたが、
`cpal 0.15` では `coreaudio-sys` の bindgen アーキテクチャ不一致バグ、
`cpal 0.17` では `dispatch2` の recursion limit バグがあり、
いずれも upstream 未修正のためビルドが通らなかった。
そのため macOS 向けエミュレーターは Go + portaudio で別途実装している。

```bash
# 依存ライブラリ (MacPorts)
sudo port install portaudio libopus pkgconfig protobuf

cd app/radio-bridge-emulator
PKG_CONFIG_PATH="/opt/local/lib/pkgconfig" go build -o radio-bridge-emulator .
./radio-bridge-emulator config.toml
```

### ゲームサーバー (wl-game-server)

```bash
cd app/wl-game-server
# config.toml の api_key と radio_bridge.host を設定
go build -o wl-game-server .
./wl-game-server config.toml
```
