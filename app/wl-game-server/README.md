# wl-game-server

ゲームサーバー。WebSocket でクライアントのセッション管理を行い、radio-bridge との音声通信を仲介する。

## 依存ライブラリ (MacPorts)

```bash
sudo port install soxr pkgconfig protobuf3-cpp
```

## Proto ファイルの生成

`wl-game-server/proto/` 以下の `.pb.go` は Docker ビルド時に自動生成されます。
ローカルビルドでは手動で生成が必要です。

**プラグインのインストール（初回のみ）**

```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
```

**生成実行（`wl-game-server/` ディレクトリで実行）**

```bash
go generate ./...
```

## ビルド・起動

```bash
PKG_CONFIG_PATH="/opt/local/lib/pkgconfig" go build -o wl-game-server .
./wl-game-server config.toml
```

## 設定ファイル (`config.toml`)

```toml
[radio_bridge]
host = "127.0.0.1"
port = 50051
reconnect_interval_secs = 5
max_reconnect_attempts = 0  # 0 = 無制限

[gemini]
api_key = "..."
transcribe_model = "gemini-3.1-flash-lite"
reasoning_model  = "gemini-3.5-flash"
tts_model        = "gemini-3.1-flash-tts-preview"
transcribe_prompt_file = "transcribe_prompt.txt"
transcribe_schema_file = "transcribe_schema.json"

[websocket]
listen_addr = ":8080"  # WebSocket サーバーのリッスンアドレス

[log]
level = "info"
```

---

## WebSocket API

エンドポイント: `ws://<host>:8080/ws`

メッセージはすべて UTF-8 JSON のテキストフレーム。

### コマンド一覧

#### `login` — ログイン / コールサイン発行

接続後、このコマンドを送るとコールサインが発行される。
すでにログイン済みの場合は同じコールサインを返す。
WebSocket 接続が閉じられると、コールサインは自動的に返却される。

**リクエスト**

```json
{"type": "login"}
```

**レスポンス (成功)**

```json
{"type": "login", "callsign": "S4AB"}
```

**レスポンス (エラー: コールサインが枯渇している場合)**

```json
{"type": "error", "error": "no callsign available"}
```

### エラーレスポンス

不正なコマンドや JSON パースエラーの場合:

```json
{"type": "error", "error": "<エラーメッセージ>"}
```

---

## コールサイン仕様

- フォーマット: `S4` + 2文字
- 使用可能文字: `A B C D E G J K P R V Z`
- 後続2文字の **1文字目** は `C` を使用しない
- 発行順はランダム、重複なし
- 最大発行数: 11 × 12 = 132
