package main

import (
	"reflect"
	"testing"
)

func TestSplitAnswerForTTS(t *testing.T) {
	tests := []struct {
		name   string
		answer string
		want   []string
	}{
		{
			name:   "冒頭呼び出し + 本文(複数文)で分割される",
			answer: "エス ヨン エー ケー。こちらエス ヨン シー キュー。 詳しい天気予報をお伝えします。 気温は二十度くらいです。 どうぞ。",
			// 冒頭2句点をまとめて独立。本文を句点で分割し、短い "どうぞ。" は直前に結合。
			want: []string{
				"エス ヨン エー ケー。こちらエス ヨン シー キュー。",
				"詳しい天気予報をお伝えします。",
				"気温は二十度くらいです。どうぞ。",
			},
		},
		{
			name:   "冒頭チャンクは短くても独立させ、本文の短い断片だけ結合する",
			answer: "アール。こちらキュー。 はい。 詳しく説明すると長い文章になります。 どうぞ。",
			want: []string{
				// 冒頭呼び出しは短くてもそのまま(最速送出のため)
				"アール。こちらキュー。",
				// 本文先頭 "はい。" は短いので後続へ前方結合、"どうぞ。" も短いので直前へ結合
				"はい。詳しく説明すると長い文章になります。どうぞ。",
			},
		},
		{
			name:   "句点が冒頭分に満たない場合は全文1チャンク(フォールバック)",
			answer: "了解しました",
			want:   []string{"了解しました"},
		},
		{
			name:   "句点がちょうど冒頭分(本文なし)も全文1チャンク",
			answer: "アール。こちらキュー。",
			want:   []string{"アール。こちらキュー。"},
		},
		{
			name:   "空文字列は nil",
			answer: "",
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitAnswerForTTS(tt.answer)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitAnswerForTTS(%q)\n got = %#v\nwant = %#v", tt.answer, got, tt.want)
			}
		})
	}
}

func TestSplitAnswerForTTS_ChunkCountWithinQueueLimit(t *testing.T) {
	// AudioQueue は最大 10 件。通常の回答(数文)で 10 を超えないことを確認する。
	answer := "アール。こちらキュー。 一つ目の文です。 二つ目の文です。 三つ目の文です。 四つ目の文です。 どうぞ。"
	got := splitAnswerForTTS(answer)
	if len(got) > 10 {
		t.Errorf("chunk count %d exceeds AudioQueue limit (10)", len(got))
	}
}
