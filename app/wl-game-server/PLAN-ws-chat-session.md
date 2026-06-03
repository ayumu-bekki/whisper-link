# WSセッションごとの genai チャットセッション + 相手コールサイン対応

## Context（背景・目的）

`wl-game-server` は、radio-bridge から来た音声を構造化データ
（`TranscriptionItem{Sender, Receiver, Message}`）に変換し、`Receiver` に応じて
`Dispatcher` が固定ハンドラ（S4CE/S4CA/S4CQ）へ振り分けて TTS 応答を返す構成。

将来シナリオを切り替えて生成AIの人格・プロンプトを差し替える予定があり、その土台として
**「WebSocketセッションごとに genai チャットセッションを持ち、継続的な雑談ができるベース実装」**
を先に用意したい。

確定した要件（ユーザー確認済み）:
1. ログイン時、自分のコールサインに加えて **相手コールサインを 0〜n** 持てる構造にする。
   いまは **1つ固定** で発行するが、将来複数になる前提の設計。発行元は自分と同じ
   **S4 プール（同一 `CallsignManager`）** から追加 Issue する。
2. genai チャットセッションを **WSセッション単位** で管理。ログイン時に生成。
   **SystemInstruction はセッション生成時に引数で渡せる**ようにする（将来シナリオ差し替え前提）。
3. 本番フロー結合: radio-bridge → 音声 → `TranscriptionItem.Receiver` が
   **WSセッションで払い出した自分CSに一致したら、そのセッションのチャットで応答生成 → TTS → 返送**。
4. 検証は **radio-bridge-emulator 経由の本番フロー** で行う（専用注入経路は作らない）。
   雑談の **履歴が継続** できることを確認できる構成にする。

## 設計方針

新規 `SessionRegistry`（CS→`wsSession`）を導入。`wsSession` に相手CSスライスと
`*genai.Chat` を持たせる。`Dispatcher` は静的ハンドラを引く前に Registry を引き、
動的払い出しCS宛てなら該当セッションのチャットへ `item.Message` を流して応答 → TTS → `sendCh`。

```
bridge handleAudio → Transcribe → Dispatch(item)
  ├ item.Receiver が SessionRegistry にヒット?
  │   Yes → session.HandleMessage(ctx, item) → chat.SendMessage → 応答テキスト
  │          → splitAnswerForTTS → streamTTSChunks（並列 TTS 生成→PCM 連結→
  │            単一 Ogg Opus を 1 回送出）→ sendCh
  │   No  → 既存 handlers[receiver]（S4CE/S4CA/S4CQ）or fallback（変更なし）
```

- 静的CS（S4CE等）と動的払い出しCS（ランダム）は名前空間が衝突しない前提のため、
  Registry を優先しても既存挙動を壊さない。Registry は **nil 許容**（未注入時は従来動作）。
- `genai.Chat` は履歴状態を持つため、同一セッションの `SendMessage` は **`chatMu` で直列化**。

## 変更・新規ファイル（すべて `app/wl-game-server/`）

### 新規: `session_registry.go`
```go
type SessionRegistry struct {
    mu       sync.RWMutex
    sessions map[string]*wsSession // 自分CS/相手CS すべてをキーに同一 *wsSession を登録
}
func NewSessionRegistry() *SessionRegistry
func (r *SessionRegistry) Register(callsign string, s *wsSession)    // Lock
func (r *SessionRegistry) Unregister(callsign string)               // Lock
func (r *SessionRegistry) Lookup(callsign string) (*wsSession, bool) // RLock
```

### 変更: `ws_session.go`
- 構造体拡張:
  ```go
  type wsSession struct {
      conn          *websocket.Conn
      callsigns     *CallsignManager
      registry      *SessionRegistry
      processor     *GeminiProcessor
      ttsClient     *TTSClient
      sendCh        chan<- []byte
      callsign      string   // 自分CS
      peerCallsigns []string // 相手CS 0..n（将来 n 個前提）
      chat          *genai.Chat
      chatMu        sync.Mutex
  }
  ```
- `newWSSession(conn, callsigns, registry, processor, ttsClient, sendCh)` にシグネチャ拡張。
- `run(ctx)` → `handleLogin(ctx)` へ ctx を伝播（チャット生成に必要）。
- `handleLogin(ctx)`:
  1. 自分CS Issue（従来）
  2. 相手CSを Issue（ベース実装は **1個**。同一プールから追加。失敗時は自分CSを Release してエラー）
  3. `chat, err = processor.NewChat(ctx, defaultChatSystemInstruction)`（下記ヘルパー）
  4. `registry.Register` で 自分CS + 全相手CS を登録
  5. `wsResponse{Type:"login", Callsign, PeerCallsigns}` を返す
- `run()` の `defer`: 自分CS + 全相手CS を `callsigns.Release` し、`registry.Unregister` する。
- 新規メソッド:
  ```go
  func (s *wsSession) HandleMessage(ctx context.Context, item TranscriptionItem) error
  // chatMu でロック → chat.SendMessage(ctx, genai.Part{Text: item.Message})
  // 応答テキスト抽出 → splitAnswerForTTS → ttsPromptTemplateS4CQ で
  //   streamTTSChunks（並列 TTS 生成→PCM 連結→単一 Ogg Opus を 1 回送出）→ sendCh
  //   （ctx.Done で中断）
  ```
- `wsResponse` に `PeerCallsigns []string `json:"peer_callsigns,omitempty"`` を追加。
- `defaultChatSystemInstruction` 定数（雑談継続用。`askSystemPromptTemplate` のコールサイン
  読み変換ルールを流用しつつ「一問一答」でなく「雑談」向けに調整）。

### 変更: `gemini.go`
- ヘルパー追加（フィールド直アクセスを局所化）:
  ```go
  func (p *GeminiProcessor) NewChat(ctx context.Context, systemInstruction string) (*genai.Chat, error) {
      cfg := &genai.GenerateContentConfig{
          SystemInstruction: genai.NewContentFromText(systemInstruction, genai.RoleUser),
      }
      return p.client.Chats.Create(ctx, p.cfg.ReasoningModel, cfg, nil)
  }
  ```

### 変更: `dispatcher.go`
- `Dispatcher` に `registry *SessionRegistry`（nil 許容）追加。`NewDispatcher(registry)`。
- `Dispatch` ループ冒頭で動的解決を優先:
  ```go
  if d.registry != nil {
      if sess, ok := d.registry.Lookup(item.Receiver); ok {
          if err := sess.HandleMessage(ctx, item); err != nil { return err }
          continue
      }
  }
  ```

### 変更: `ws_server.go`
- `WSServer` に `registry/processor/ttsClient/sendCh` を追加。
  `NewWSServer(callsigns, registry, processor, ttsClient, sendCh)`。
- `ServeHTTP` で `newWSSession(...)` に新引数を渡す。

### 変更: `main.go`
- 組み立て順序:
  1. `processor` → 2. `ttsClient` → 3. `sendCh`
  4. `registry := NewSessionRegistry()`
  5. `dispatcher := NewDispatcher(registry)` ＋ 静的ハンドラ Register（従来どおり）
  6. `callsigns := NewCallsignManager("S4")`
  7. `wsServer := NewWSServer(callsigns, registry, processor, ttsClient, sendCh)`
  8. `bridgeClient`（変更なし）

### 変更: `../wl-game-server-test-client/main.go`（検証用）
- `wsResponse` に `PeerCallsigns []string` を追加し、login 表示に相手CSを出す
  （`<< [login] コールサイン: S4PZ / 相手: [S4AB]` のように）。

## 並行性・注意点
- `SessionRegistry`: `sync.RWMutex`（Lookup=RLock、Register/Unregister=Lock）。
- 同一セッションのチャットは `chatMu` で直列化（`genai.Chat` は履歴状態を持つため）。
  bridge は `audioCh`(buffer=1) で受信を直列処理するので同時呼び出しは実運用では稀だが防御する。
- Lookup 後に run() 終了して conn が閉じるレース: `HandleMessage` は conn へ書かず
  共有 `sendCh` 経由なので送信は安全。Unregister 済みなら Lookup で外れ新規は届かない。
- `chat.SendMessage` の引数は `...genai.Part`（値型）。`genai.Part{Text: item.Message}` を渡す。

## 検証手順（emulator 経由の本番フロー）

1. emulator 起動（別ターミナル）:
   ```
   cd app/radio-bridge-emulator
   PKG_CONFIG_PATH="/opt/local/lib/pkgconfig" go build -o radio-bridge-emulator .
   ./radio-bridge-emulator config.toml
   ```
   `listen_addr` と wl-game-server の `[radio_bridge] host/port` が一致することを確認。
2. wl-game-server 起動:
   ```
   cd app/wl-game-server
   PKG_CONFIG_PATH="/opt/local/lib/pkgconfig" go build -o wl-game-server .
   ./wl-game-server config.toml
   ```
3. WSログインで動的CS払い出し（test-client で `ws://127.0.0.1:8080/ws` に接続し `login`）。
   返ってきた `callsign`（例: `S4PZ`）と `peer_callsigns` を確認。
4. emulator のマイクに「<払い出しCSの読み>。こちら<任意CS>。 今日はいい天気だね。 どうぞ。」と発話。
   wl-game-server ログで `transcribed: receiver=S4PZ` → registry ヒット → `chat.SendMessage`
   → TTS → `sent audio`、emulator スピーカーで応答音声再生を確認。
5. **履歴継続の確認（本要件の肝）**:
   - 1ターン目「私の名前はタロウです。覚えておいて。どうぞ。」
   - 2ターン目「私の名前は何でしたか。どうぞ。」
   - 応答で「タロウ」が返れば `*genai.Chat` が履歴を保持し継続会話できていると確認。
6. WSクライアント切断 → 自分CS + 相手CS全ての `callsign released` / Unregister がログに出ること、
   再ログインで再発行されることを確認。

注意: emulator はマイク必須（portaudio）。マイク発話が難しい場合は
`radio-bridge-test-client`（Ogg を流せる）で同等の本番経路を流すのが代替。
Transcribe が receiver を払い出しCSと正しく聞き取れるかが成否を分けるため明瞭に発話する。
