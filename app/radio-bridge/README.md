# radio-bridge

無線トランシーバーと gRPC クライアント間で音声を中継するデーモン。
受信した音声をキューに積んで順次 PTT 送信し、マイク入力をリアルタイムで gRPC クライアントにストリーミングする。

## 処理の概要

```
[gRPC クライアント]
  │  Ogg Opus 音声 (送信方向)
  ▼
[AudioQueue]  ←── キューに積む（最大 10 件、1 件 30 秒以内）
  │
  ▼
[Controller]  ←── 状態機械がキューを消費して PTT 送信
  │
  ├── GPIO PTT HIGH → 無線送信開始
  ├── ALSA 再生 (plughw:2,0)
  └── GPIO PTT LOW  → 無線送信終了

[AudioRecorder]  ←── マイク常時監視 (hw:2,0)
  │  RMS ≥ threshold で録音開始、無音 1 秒で終了
  │  Ogg Opus にエンコードして broadcast
  ▼
[gRPC クライアント]  ←── 受信した音声をストリーミング配信
```

## コンポーネント

### gRPC サーバー (`grpc/handler.rs`)

双方向ストリーミング RPC `Connect` を提供する。

- **クライアント → サーバー**: 受信した `AudioChunk` (Ogg Opus) を `AudioQueue` に追加
- **サーバー → クライアント**: `AudioRecorder` の broadcast を購読し、録音完了ごとに `AudioChunk` を配信

### AudioQueue (`queue.rs`)

受信音声クリップを FIFO で管理する。

- `max_queue_size` を超えると新規エントリを拒否
- `max_audio_duration_secs` を超える長さのクリップを拒否
- Ogg Opus の granule_position から再生時間を自動算出

### AudioRecorder (`audio/recorder.rs`)

ALSA キャプチャデバイスを常時監視し、音声検知時に録音する。

| フラグ | 意味 |
|---|---|
| `is_recording` | 現在録音中かどうか（Controller が参照） |
| `is_transmitting` | PTT 送信中（true の間は新規録音を開始しない） |
| `stop_recording` | 強制終了フラグ（次フレームで録音状態をリセット） |

録音の開始・終了条件:
- 開始: RMS ≥ `input_threshold_rms` かつ `is_transmitting = false`
- 終了: 無音（RMS < threshold）が `input_silence_ms` 継続、または `input_max_recording_secs` 超過、または `force_stop_recording()` 呼び出し

### Controller (`controller.rs`)

PTT GPIO と音声再生を制御する状態機械。

## 状態機械

```
          ┌─────────────────────────────────────────┐
          │                                         │
          ▼                                         │
        IDLE ──────────────────────────────────────►┤
          │ キューに音声あり                           │
          │ かつ is_recording=false                  │ キューが空
          │ (3 秒待っても終わらなければ force_stop)     │
          ▼                                         │
        PTT_ON                                      │
          │ is_transmitting=true をセット             │
          │ GPIO HIGH                               │
          │ ptt_on_delay_ms 待機                     │
          ▼                                         │
        PLAYING                                     │
          │ Ogg Opus デコード → ALSA 再生             │
          ▼                                         │
        PTT_OFF                                     │
          │ ptt_off_delay_ms 待機                    │
          │ GPIO LOW                                │
          │ is_transmitting=false をセット            │
          ▼                                         │
        COOLDOWN                                    │
          │ cooldown_ms 待機                         │
          ├── is_recording=true ──► LISTENING ───────┤
          ├── キューに音声あり ──────► PTT_ON          │
          └── それ以外 ────────────────────────────►┘

        LISTENING
          │ is_recording=false になるまで待機
          │ (3 秒待っても終わらなければ force_stop)
          ├── キューに音声あり ──► PTT_ON
          └── キューが空 ────────► IDLE
```

### 各状態の詳細

| 状態 | 処理内容 |
|---|---|
| **IDLE** | キューが空なら 100ms ポーリング。キューに音声があり is_recording=false になったら PTT_ON へ。3 秒経過しても録音が続く場合は force_stop して強制遷移。 |
| **PTT_ON** | is_transmitting=true をセット、GPIO を HIGH にして `ptt_on_delay_ms` 待機（無線機が送信状態になるまでの遅延）。 |
| **PLAYING** | キューから 1 件取り出し、Ogg Opus をデコードして ALSA に書き込む。再生完了またはエラーで PTT_OFF へ。 |
| **PTT_OFF** | `ptt_off_delay_ms` 待機後に GPIO を LOW にし、is_transmitting=false をセット。 |
| **COOLDOWN** | `cooldown_ms` 待機。無線機の余韻を吸収しつつ、次の状態を判定。 |
| **LISTENING** | マイクが録音中（相手が話している）の間待機。終了後にキューの有無で次状態を決定。3 秒タイムアウトで force_stop。 |

## 設定ファイル (`config.toml`)

```toml
[server]
listen_addr = "0.0.0.0:50051"   # gRPC 待受アドレス

[gpio]
ptt_pin = 26                     # PTT 制御に使う GPIO ピン番号 (BCM)

[audio]
output_device           = "plughw:2,0"  # 再生デバイス
input_device            = "hw:2,0"      # 録音デバイス
input_threshold_rms     = 1000          # 録音開始 RMS 閾値 (0–65535)
input_silence_ms        = 1000          # この無音時間で録音終了
input_max_recording_secs = 30           # 1 録音の最大秒数

[timing]
ptt_on_delay_ms  = 500   # GPIO HIGH 後の待機時間（無線機が送信準備するまで）
ptt_off_delay_ms = 200   # 再生後から GPIO LOW までの待機時間
cooldown_ms      = 1000  # PTT_OFF 後のクールダウン

[queue]
max_audio_duration_secs = 30   # キューに入れられる 1 クリップの最大長
max_queue_size          = 10   # キューの最大件数
```

## ビルド手順

### 必要なシステムライブラリ

標準の `apt` リポジトリに含まれていないパッケージがあるため、Debian の公式パッケージサーバーから直接インストールする。

```bash
# libopus-dev (Opus コーデックの開発ファイル)
wget http://ftp.debian.org/debian/pool/main/o/opus/libopus-dev_1.5.2-2_arm64.deb -O /tmp/libopus-dev.deb
sudo dpkg -i /tmp/libopus-dev.deb

# libprotoc (protoc の共有ライブラリ)
wget http://ftp.debian.org/debian/pool/main/p/protobuf/libprotoc32t64_3.21.12-11_arm64.deb -O /tmp/libprotoc32t64.deb
sudo dpkg -i /tmp/libprotoc32t64.deb

# libprotobuf-dev (Protocol Buffers の開発ファイル)
wget http://ftp.debian.org/debian/pool/main/p/protobuf/libprotobuf-dev_3.21.12-11_arm64.deb -O /tmp/libprotobuf-dev.deb
sudo dpkg -i /tmp/libprotobuf-dev.deb

# protobuf-compiler (protoc コマンド本体)
wget http://ftp.debian.org/debian/pool/main/p/protobuf/protobuf-compiler_3.21.12-11_arm64.deb -O /tmp/protobuf-compiler.deb
sudo dpkg -i /tmp/protobuf-compiler.deb
```

### ビルド

```bash
# 通常ビルド (GPIO 使用、Raspberry Pi 向け)
cargo build --release

# GPIO なしビルド (テスト用、GPIO ハードウェア不要)
cargo build --release --features stub-gpio
```

`stub-gpio` フィーチャーを有効にすると `rppal` への依存がなくなり、GPIO 操作はログ出力のみの no-op になる。
また `config.toml` の `[gpio]` セクションが省略可能になる。

### macOS でのビルド検証 (`cargo check`)

macOS では `coreaudio-sys` のビルドに Xcode SDK パスと clang ターゲットの明示が必要:

```bash
SDK=$(xcrun --sdk macosx --show-sdk-path)
COREAUDIO_SDK_PATH="$SDK" \
BINDGEN_EXTRA_CLANG_ARGS="--target=aarch64-apple-darwin -isysroot $SDK" \
cargo check --features stub-gpio
```

### 実行

```bash
# デフォルト (./config.toml を使用)
cargo run

# 設定ファイルを指定
cargo run -- /path/to/config.toml

# GPIO なしで実行
cargo run --features stub-gpio -- config.toml
```

## ALSA キャプチャゲインの設定

USB オーディオデバイスのマイクキャプチャゲインが高すぎると、ラジオの背景ノイズ (スケルチ開放時のホワイトノイズ等) が `input_threshold_rms` を超えて偽トリガーし、実際の発話が録音されなくなる。

ゲインはホスト上のセットアップスクリプトで設定する。`alsactl store` で永続化されるため、コンテナ再起動後も有効。

### 初期セットアップ

```bash
sudo sh app/radio-bridge/setup_alsa.sh
```

スクリプトの内容は `CARD`・`NUMID`・`VALUE` 変数を編集して調整できる。

### 現在の設定を確認する

```bash
# カード番号を確認
aplay -l | grep -i usb

# ミキサーコントロール一覧
amixer -c <カード番号> contents
```

### ゲイン値の目安

| 設定値 | ゲイン | 備考 |
|--------|--------|------|
| 60 (デフォルト) | 30 dB | 背景ノイズが閾値を超えて偽トリガー発生 |
| 50 | 25 dB | 5 dB 低減 |
| **30 (推奨)** | **15 dB** | **背景ノイズが閾値以下、発話は正常検知** |
| 0 | 0 dB | 信号が弱くなりすぎる場合あり |

適切な値はハードウェア構成や無線機の出力レベルによって異なる。発話時に `recording started (rms=XXXX)` ログが出ることを確認しながら調整する。

### 動作確認環境

- Raspberry Pi (aarch64)
- Debian trixie
- Sound Blaster Play! 3 (USB オーディオ、card 2)
