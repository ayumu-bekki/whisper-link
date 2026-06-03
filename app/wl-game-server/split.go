package main

import "strings"

const (
	// ttsChunkMinRunes は分割後チャンクの最小文字数(rune)。これを下回る断片は
	// 次の断片と結合する。極端に短いチャンクは PTT on/off のオーバーヘッドが
	// 音声より長くなり逆効果になるため。
	ttsChunkMinRunes = 12

	// ttsLeadSentences は冒頭呼び出し部("<相手>。こちら<自分>。")が含む句点の数。
	// 先頭からこの数の「。」までを冒頭チャンクとして 1 つにまとめ、最速で送出する。
	ttsLeadSentences = 2
)

// splitAnswerForTTS は Ask の回答テキストを TTS 用のチャンクに分割する。
//
// Ask の回答は askSystemPromptTemplate により
//
//	<相手コールサイン>。こちら<自分コールサイン>。 <回答内容>。 どうぞ。
//
// という形式で返る。先頭 2 つの「。」までの冒頭呼び出し部を最初の 1 チャンクとして
// 切り出し、残りの本文を句点「。」で分割する。短すぎる断片(ttsChunkMinRunes 未満)は
// 直前のチャンクに結合し、過剰に短いチャンクを作らない。
//
// 句点が ttsLeadSentences に満たないなど想定外フォーマットの場合は、分割せず
// 全文を 1 チャンクとして返す(従来動作へのフォールバック)。
func splitAnswerForTTS(answer string) []string {
	// 句点単位の断片に分割する。各断片には末尾の「。」を含める。
	sentences := splitBySentence(answer)

	// 句点が冒頭呼び出し分に満たない場合はフォーマット想定外。全文 1 チャンク。
	if len(sentences) <= ttsLeadSentences {
		trimmed := strings.TrimSpace(answer)
		if trimmed == "" {
			return nil
		}
		return []string{trimmed}
	}

	// 先頭 ttsLeadSentences 個を冒頭呼び出しチャンクとして 1 つにまとめる。
	// 冒頭チャンクは短くても最速で送出したいので、本文の短い断片を結合する対象には
	// せず常に独立させる。
	lead := strings.TrimSpace(strings.Join(sentences[:ttsLeadSentences], ""))
	body := mergeShortChunks(sentences[ttsLeadSentences:])

	chunks := make([]string, 0, len(body)+1)
	chunks = append(chunks, lead)
	chunks = append(chunks, body...)
	return chunks
}

// splitBySentence は文字列を句点「。」区切りの断片に分割する。各断片は末尾の
// 「。」を含み、前後の空白は取り除く。句点を含まない末尾の余り(例: 句点で
// 終わらないテキスト)も 1 断片として返す。
func splitBySentence(s string) []string {
	var sentences []string
	var sb strings.Builder
	for _, r := range s {
		sb.WriteRune(r)
		if r == '。' {
			if seg := strings.TrimSpace(sb.String()); seg != "" {
				sentences = append(sentences, seg)
			}
			sb.Reset()
		}
	}
	if seg := strings.TrimSpace(sb.String()); seg != "" {
		sentences = append(sentences, seg)
	}
	return sentences
}

// mergeShortChunks は ttsChunkMinRunes 文字未満の短い断片を結合する。短い断片は
// まず直前のチャンクへ、先頭チャンクが短い場合は後続チャンクへ繰り込む。
func mergeShortChunks(chunks []string) []string {
	if len(chunks) == 0 {
		return chunks
	}

	var merged []string
	for _, c := range chunks {
		if len(merged) > 0 && countRunes(c) < ttsChunkMinRunes {
			merged[len(merged)-1] += c
			continue
		}
		merged = append(merged, c)
	}

	// 先頭チャンクが短いまま残った場合(後続が無く繰り込めなかったケースを除き)、
	// 次のチャンクへ前方結合する。
	for len(merged) >= 2 && countRunes(merged[0]) < ttsChunkMinRunes {
		merged[1] = merged[0] + merged[1]
		merged = merged[1:]
	}

	return merged
}

func countRunes(s string) int {
	return len([]rune(s))
}
