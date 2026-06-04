package main

import "testing"

func TestConversationLogRender(t *testing.T) {
	l := NewConversationLog(0)
	l.Append(ConversationEntry{Sender: "S4PZ", Receiver: "S4DE", Message: "各局おはようございます"})
	l.Append(ConversationEntry{Sender: "S4DE", Receiver: "S4PZ", Message: "おはようございます、感度良好です"})
	l.Append(ConversationEntry{Sender: "S4PZ", Receiver: "S4DE", Message: "さっき入感してましたね"})

	want := "[S4PZ] -> [S4DE] 各局おはようございます\n" +
		"[S4DE] -> [S4PZ] おはようございます、感度良好です\n" +
		"[S4PZ] -> [S4DE] さっき入感してましたね"

	if got := l.Render(); got != want {
		t.Errorf("Render() mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestConversationLogRenderEmpty(t *testing.T) {
	l := NewConversationLog(0)
	if got := l.Render(); got != "" {
		t.Errorf("Render() on empty log = %q, want empty", got)
	}
}

func TestConversationLogMaxWindow(t *testing.T) {
	l := NewConversationLog(2)
	l.Append(ConversationEntry{Sender: "A", Receiver: "B", Message: "1"})
	l.Append(ConversationEntry{Sender: "A", Receiver: "B", Message: "2"})
	l.Append(ConversationEntry{Sender: "A", Receiver: "B", Message: "3"})

	// 直近2件のみ保持される
	want := "[A] -> [B] 2\n[A] -> [B] 3"
	if got := l.Render(); got != want {
		t.Errorf("Render() with max=2 = %q, want %q", got, want)
	}
}
