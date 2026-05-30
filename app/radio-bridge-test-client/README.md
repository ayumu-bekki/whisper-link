# radio-bridge-test-client

`transceiver_audio_queue` サーバーの動作確認用CLIアプリ (macOS向け)。

スペースキーを押している間マイクで録音し、離すとOgg Opusにエンコードしてサーバーへ送信する。
サーバー側がトランシーバーで受信した音声は随時受信して再生する。

## 必要なもの

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

## ビルド

```bash
cd app/radio-bridge-test-client
PKG_CONFIG_PATH="/opt/local/lib/pkgconfig" go build -o radio-bridge-test-client .
```

## 使い方

```bash
./radio-bridge-test-client <host> <port>
```

**例:**
```bash
./radio-bridge-test-client 192.168.1.10 50051
```

起動するとサーバーに接続し、以下の操作が可能になる。

| 操作 | 動作 |
|---|---|
| スペースキーを押す | マイク録音開始 |
| スペースキーを離す | 録音停止 → Ogg Opusにエンコード → サーバーへ送信 |
| Ctrl+C | 終了 |

サーバー側のトランシーバー受信音声は接続中に随時再生される。

## protoファイルを変更した場合

`app/proto/transceiver.proto` を変更したあとは以下で再生成する。

```bash
cd app/radio-bridge-test-client
go generate
```
