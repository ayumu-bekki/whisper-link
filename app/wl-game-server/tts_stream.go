package main

import (
	"context"
	"fmt"
	"log"
)

// ttsChunkResult は 1 チャンクの並列 TTS 生成結果(48kHz PCM)を表す。
type ttsChunkResult struct {
	pcm []int16
	err error
}

// streamTTSChunks は分割済みチャンクを並列で TTS 生成し、1 本の Ogg Opus に結合して
// 1 回だけ sendCh へ送出する。
//
// 全チャンクの TTS リクエストを同時に発行することで、初回応答までのレイテンシを短縮する。
// 一方で radio-bridge には他プロセスからもデータが送られる可能性があり、チャンクを別々の
// AudioChunk として送ると間に無関係な音声が割り込んで PTT 制御サイクルが乱れる。そのため
// 並列生成した各チャンクの PCM をチャンク順に連結し、1 本の Ogg Opus ストリームへエンコード
// してから 1 回だけ送出する(granule 連続・ヘッダ 1 つの正しい単一ストリームになる)。
//
// 1 チャンクの TTS が失敗した場合はログを残してそのチャンクのみスキップし、全体は止めない
// (従来動作を踏襲)。ctx がキャンセルされた場合は ctx.Err() を返す。
//
// logPrefix はログ出力の接頭辞(例: "[S4CQ]" / "[WS chat]")。
func streamTTSChunks(ctx context.Context, ttsClient *TTSClient, sendCh chan<- []byte, chunks []string, logPrefix string) error {
	if len(chunks) == 0 {
		return nil
	}

	// 各チャンク用の結果チャネル。容量 1 にして生成 goroutine がブロックせず終了できるようにする。
	results := make([]chan ttsChunkResult, len(chunks))
	for i := range results {
		results[i] = make(chan ttsChunkResult, 1)
	}

	// 全チャンクの TTS を並列に開始する。
	for i, chunk := range chunks {
		go func(idx int, text string) {
			ttsPrompt := fmt.Sprintf(ttsPromptTemplateS4CQ, text)
			pcm, err := ttsClient.GeneratePCM48kFromPrompt(ctx, ttsPrompt)
			results[idx] <- ttsChunkResult{pcm: pcm, err: err}
		}(i, chunk)
	}

	// インデックス昇順に待ち合わせ、成功したチャンクの PCM を順に連結する。
	var merged []int16
	for i := range chunks {
		var res ttsChunkResult
		select {
		case res = <-results[i]:
		case <-ctx.Done():
			return ctx.Err()
		}

		if res.err != nil {
			log.Printf("%s TTS error (chunk %d/%d): %v", logPrefix, i+1, len(chunks), res.err)
			continue // 1 チャンクの失敗で全体を止めない
		}
		log.Printf("%s TTS generated chunk %d/%d (%d samples)", logPrefix, i+1, len(chunks), len(res.pcm))
		merged = append(merged, res.pcm...)
	}

	if len(merged) == 0 {
		log.Printf("%s no TTS audio generated, nothing to send", logPrefix)
		return nil
	}

	oggData, err := encodePCMToOggOpus(merged)
	if err != nil {
		return fmt.Errorf("encodePCMToOggOpus: %w", err)
	}
	log.Printf("%s TTS merged %d chunks into %d bytes, sending", logPrefix, len(chunks), len(oggData))

	select {
	case sendCh <- oggData:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}
