package main

import (
	"strings"
	"sync"
)

// ConversationEntry は1つの発話を表す。無線交信の1交信に相当する。
type ConversationEntry struct {
	Sender   string // 発言者コールサイン
	Receiver string // 対象コールサイン
	Message  string
}

// ConversationLog は全接続で共有する単一の会話ログ。
// 無線は単一周波数のブロードキャストなので、全プレイヤー・全NPCの発話を時系列で1本に記録する。
// 各NPCはこのログ全体を文脈として参照するため、自分が直接関与しない交信も「聞こえている」前提になる。
type ConversationLog struct {
	mu      sync.RWMutex
	entries []ConversationEntry
	max     int // 0=無制限。将来のウィンドウ化(直近N件のみ保持)用
}

// NewConversationLog は共有会話ログを生成する。max=0 で無制限。
func NewConversationLog(max int) *ConversationLog {
	return &ConversationLog{max: max}
}

// Append は発話を1件追記する。max>0 の場合は古い発話を切り捨てて直近 max 件のみ保持する。
func (l *ConversationLog) Append(e ConversationEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
	if l.max > 0 && len(l.entries) > l.max {
		l.entries = l.entries[len(l.entries)-l.max:]
	}
}

// Render は全履歴を "[Sender] -> [Receiver] Message" の改行区切り文字列に整形する。
// NPC応答生成時にプロンプトの文脈として渡す。
func (l *ConversationLog) Render() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var b strings.Builder
	for i, e := range l.entries {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("[")
		b.WriteString(e.Sender)
		b.WriteString("] -> [")
		b.WriteString(e.Receiver)
		b.WriteString("] ")
		b.WriteString(e.Message)
	}
	return b.String()
}
