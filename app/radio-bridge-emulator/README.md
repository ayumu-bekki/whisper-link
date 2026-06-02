# radio-bridge-emulator

macOS上で動作する `radio-bridge` のエミュレーター。
`radio-bridge` と同じ gRPC インターフェース (`Connect` 双方向ストリーミング) を提供し、
`wl-game-server` や `radio-bridge-test-client` の接続先として使用できる。

## radio-bridge との違い

| 機能 | radio-bridge (Raspberry Pi) | radio-bridge-emulator (macOS) |
|---|---|---|
| 実装言語 | Rust | Go |
| 音声I/O | ALSA | portaudio (CoreAudio経由) |
| PTT制御 | GPIO (rppal) | なし (ログ出力のみ) |
| マイク入力 | 有線接続のUSBオーディオ | macOSのデフォルト入力デバイス |
| スピーカー出力 | ALSA再生デバイス | macOSのデフォルト出力デバイス |

> **背景**: `radio-bridge` の Rust コードをマルチプラットフォーム対応にするため `cpal` への移行を試みたが、
> `cpal 0.15` では `coreaudio-sys` の bindgen アーキテクチャ不一致バグ、
> `cpal 0.17` では `dispatch2` の recursion limit バグがあり upstream 未修正のためビルドが通らなかった。
> そのため macOS 向けエミュレーターは Go + portaudio で別途実装している。

## 必要なもの

MacPorts で以下をインストールする。

```bash
sudo port install protobuf pkgconfig portaudio libopus
```

| パッケージ | 用途 |
|---|---|
| `protobuf` | `protoc` コンパイラ (proto ファイルから Go コード生成) |
| `pkgconfig` | C ライブラリのビルドフラグを Go に渡す |
| `portaudio` | macOS の音声入出力 |
| `libopus` | Opus コーデック |

## ビルド

```bash
cd app/radio-bridge-emulator
PKG_CONFIG_PATH="/opt/local/lib/pkgconfig" go build -o radio-bridge-emulator .
```

## 設定ファイル

`config.toml` を編集して起動する。

```toml
[server]
listen_addr = "0.0.0.0:50051"

[audio]
input_threshold_rms      = 1000  # 録音開始 RMS 閾値 (0-32767)
input_silence_ms         = 1000  # この無音時間で録音終了
input_max_recording_secs = 30    # 1 録音の最大秒数
```

## 実行

```bash
# デフォルト (./config.toml を使用)
./radio-bridge-emulator

# 設定ファイルを指定
./radio-bridge-emulator /path/to/config.toml
```

起動するとマイク入力の監視を開始し、RMS 閾値を超えた音声を自動録音して接続中のクライアントへ配信する。
クライアントから受信した音声はスピーカーで再生する。

## proto ファイルを変更した場合

`app/proto/transceiver.proto` を変更したあとは以下で再生成する。

```bash
cd app/radio-bridge-emulator
go generate
```
