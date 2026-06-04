package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/gorilla/websocket"
	"google.golang.org/genai"
)

// defaultChatSystemInstruction は雑談継続用のデフォルト SystemInstruction。
// 将来シナリオごとに差し替える前提で、NewChat の引数として渡す。
// 各ターンでの送受信者は HandleMessage が userMessage に含めて渡すので、
// ここでは口調・フォーマットルールのみ定義する。
const defaultChatSystemInstruction = `あなたはアマチュア無線のオペレーターです。
以下のルールに必ず従ってください:
- 回答は日本語で、要点のみを1文から2文で述べてください
- 最大60文字程度に収めてください（フォーマット部分を除く）
- マークダウン記法や箇条書きは使わず、プレーンテキストで回答してください
- 無線交信らしい口調で話してください
- 会話の文脈を踏まえて自然に雑談を続けてください
- コールサインは音声で読み上げられるため、1文字ずつ日本語のカタカナ読みに変換して出力してください
  - 数字は「ゼロ イチ ニー サン ヨン ゴー ロク ナナ ハチ キュー」
  - アルファベットは「エー ビー シー ディー イー エフ ジー エイチ アイ ジェイ ケー エル エム エヌ オー ピー キュー アール エス ティー ユー ブイ ダブリュー エックス ワイ ゼット」
  - 各読みの区切りには半角スペースを入れてください
  - 例: S4CQ → 「エス ヨン シー キュー」、S4AK → 「エス ヨン エー ケー」
- 毎回の返答は以下のフォーマットを守ってください（コールサイン部分は上記の読み変換を適用すること）
  - <相手コールサインの読み>。こちら<自分コールサインの読み>。 <回答内容>。 どうぞ。`

// chatUserMessageTemplate は各ターンで SendMessage に渡すメッセージのテンプレート。
// sender/receiver を毎回明示することで、LLM がコールサインを自作するのを防ぐ。
// %s の順: 送信者CS(item.Sender), 受信者CS=自分CS(s.callsign), メッセージ
const chatUserMessageTemplate = `送信者のコールサイン: %s
受信者のコールサイン（あなた）: %s
メッセージ: %s`

// クライアントから送られるコマンドの共通構造
type wsCommand struct {
	Type string `json:"type"`
}

// "login" コマンド
type wsLoginCommand struct {
	Type string `json:"type"`
}

// サーバーからクライアントへのレスポンス
type wsResponse struct {
	Type          string   `json:"type"`
	Callsign      string   `json:"callsign,omitempty"`
	PeerCallsigns []string `json:"peer_callsigns,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// wsSession は1つの WebSocket 接続のセッション状態を保持する。
type wsSession struct {
	conn          *websocket.Conn
	callsigns     *CallsignManager
	registry      *SessionRegistry
	processor     *GeminiProcessor
	ttsClient     *TTSClient
	sendCh        chan<- outgoingAudio

	callsign      string   // 自分CS
	peerCallsigns []string // 相手CS 0..n（将来複数前提）

	chat   *genai.Chat
	chatMu sync.Mutex // chat.SendMessage を直列化
}

func newWSSession(
	conn *websocket.Conn,
	callsigns *CallsignManager,
	registry *SessionRegistry,
	processor *GeminiProcessor,
	ttsClient *TTSClient,
	sendCh chan<- outgoingAudio,
) *wsSession {
	return &wsSession{
		conn:      conn,
		callsigns: callsigns,
		registry:  registry,
		processor: processor,
		ttsClient: ttsClient,
		sendCh:    sendCh,
	}
}

func (s *wsSession) run(ctx context.Context) {
	defer func() {
		// 自分CS + 全相手CS をプールへ返却し、レジストリから解除する
		for _, cs := range append([]string{s.callsign}, s.peerCallsigns...) {
			if cs == "" {
				continue
			}
			s.callsigns.Release(cs)
			s.registry.Unregister(cs)
			log.Printf("[WS] callsign released: %s", cs)
		}
		s.conn.Close()
	}()

	for {
		_, msg, err := s.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("[WS] read error: %v", err)
			}
			return
		}

		var cmd wsCommand
		if err := json.Unmarshal(msg, &cmd); err != nil {
			s.sendError("invalid JSON")
			continue
		}

		switch cmd.Type {
		case "login":
			s.handleLogin(ctx)
		default:
			s.sendError("unknown command type: " + cmd.Type)
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func (s *wsSession) handleLogin(ctx context.Context) {
	if s.callsign != "" {
		// すでにログイン済み: 現在のコールサインをそのまま返す
		s.sendJSON(wsResponse{Type: "login", Callsign: s.callsign, PeerCallsigns: s.peerCallsigns})
		return
	}

	// 自分CS を発行
	cs, ok := s.callsigns.Issue()
	if !ok {
		s.sendError("no callsign available")
		return
	}
	s.callsign = cs

	// 相手CS を1つ発行（同一プール。将来複数発行はここで繰り返す）
	peerCS, ok := s.callsigns.Issue()
	if !ok {
		s.callsigns.Release(cs)
		s.callsign = ""
		s.sendError("no peer callsign available")
		return
	}
	s.peerCallsigns = []string{peerCS}

	// genai チャットセッションを生成
	chat, err := s.processor.NewChat(ctx, defaultChatSystemInstruction)
	if err != nil {
		s.callsigns.Release(cs)
		s.callsigns.Release(peerCS)
		s.callsign = ""
		s.peerCallsigns = nil
		s.sendError(fmt.Sprintf("failed to create chat session: %v", err))
		return
	}
	s.chat = chat

	// レジストリに自分CS + 全相手CS を登録
	s.registry.Register(s.callsign, s)
	for _, peer := range s.peerCallsigns {
		s.registry.Register(peer, s)
	}

	log.Printf("[WS] login: callsign=%s peers=%v", s.callsign, s.peerCallsigns)
	s.sendJSON(wsResponse{Type: "login", Callsign: s.callsign, PeerCallsigns: s.peerCallsigns})
}

// HandleMessage は TranscriptionItem を受け取り、チャットで応答を生成して TTS 送信する。
// Dispatcher から呼ばれる（bridge goroutine）ため chatMu で直列化する。
// TTS は S4CQ と同様にチャンク並列生成し、PCM 連結して単一 Ogg Opus を 1 回送出する。
func (s *wsSession) HandleMessage(ctx context.Context, item TranscriptionItem) error {
	// sender/receiver を毎回明示して LLM がコールサインを自作するのを防ぐ。
	// item.Receiver が自分CS（相手CS宛メッセージもこのセッションで処理するため s.callsign とは限らない）
	userMessage := fmt.Sprintf(chatUserMessageTemplate, item.Sender, item.Receiver, item.Message)

	s.chatMu.Lock()
	resp, err := s.chat.SendMessage(ctx, genai.Part{Text: userMessage})
	s.chatMu.Unlock()
	if err != nil {
		return fmt.Errorf("chat.SendMessage: %w", err)
	}

	if resp == nil || len(resp.Candidates) == 0 ||
		resp.Candidates[0].Content == nil ||
		len(resp.Candidates[0].Content.Parts) == 0 {
		return fmt.Errorf("empty chat response")
	}
	answer := resp.Candidates[0].Content.Parts[0].Text
	if answer == "" {
		return fmt.Errorf("empty chat response text")
	}
	log.Printf("[WS chat] sender=%s answer: %s", item.Sender, answer)

	// 全チャンクを並列に TTS 生成して全体の生成時間を短縮しつつ、できた順に同一 stream_id +
	// START/CONTINUE/END を付けて 1 チャンクずつ送出する（分割送信）。radio-bridge は同一
	// stream_id を 1 区間で連続再生するため、先頭チャンクが鳴るまでの体感レイテンシが小さい。
	chunks := splitAnswerForTTS(answer)
	return streamTTSChunks(ctx, s.ttsClient, s.sendCh, chunks, "[WS chat]")
}

func (s *wsSession) sendError(msg string) {
	s.sendJSON(wsResponse{Type: "error", Error: msg})
}

func (s *wsSession) sendJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("[WS] marshal error: %v", err)
		return
	}
	if err := s.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Printf("[WS] write error: %v", err)
	}
}
