package main

import (
	"context"
	"log"

	"github.com/google/uuid"
	pb "wl-game-server/gen"
)

// ttsChunkResult は 1 チャンクの並列 TTS 生成・エンコード結果を表す。
type ttsChunkResult struct {
	ogg []byte
	err error
}

// generateChunksParallel は分割済みプロンプト(組み立て済み文字列)を並列で TTS 生成・エンコードし、
// 各チャンクの結果チャネルをインデックス順に返す。呼び出し側は results[i] を昇順に待ち合わせることで、
// 全チャンクを同時生成しつつ順序を保って取り出せる。
// プロンプトの組み立ては呼び出し元(streamTTSChunks)に委ねる。
func generateChunksParallel(ctx context.Context, ttsClient *TTSClient, prompts []string) []chan ttsChunkResult {
	results := make([]chan ttsChunkResult, len(prompts))
	for i := range results {
		results[i] = make(chan ttsChunkResult, 1)
	}
	for i, prompt := range prompts {
		go func(idx int, ttsPrompt string) {
			pcm, err := ttsClient.GeneratePCM24kFromPrompt(ctx, ttsPrompt)
			if err != nil {
				results[idx] <- ttsChunkResult{err: err}
				return
			}
			ogg, err := encodePCMToOggOpus(pcm)
			results[idx] <- ttsChunkResult{ogg: ogg, err: err}
		}(i, prompt)
	}
	return results
}

// streamTTSChunks は分割済みチャンクを並列で TTS 生成し、できた順 (インデックス昇順) に
// 1 チャンクずつ独立した Ogg Opus にエンコードして sendCh へ送出する (分割送信)。
//
// 各チャンクには同一の stream_id と status (先頭=START / 中間=CONTINUE / 末尾=END) を
// 付与する。radio-bridge は同一 stream_id のチャンク群を 1 区間で連続再生する。先頭チャンク
// が生成でき次第すぐ送れるため、最初の音が鳴るまでの体感レイテンシが小さい。
//
// 1 チャンクの TTS が失敗した場合はログを残してそのチャンクのみスキップし、全体は止めない。
// 失敗で送出チャンクが 0 件になった場合は何も送らない。ctx がキャンセルされた場合は
// ctx.Err() を返す。logPrefix はログ出力の接頭辞。
// buildPrompt はテキストチャンクを TTS 用の完成プロンプトに変換する関数。呼び出し元が
// ペルソナ・シーン設定をここで差し込むことで、generateChunksParallel は純粋な生成処理に徹する。
func streamTTSChunks(ctx context.Context, ttsClient *TTSClient, sendCh chan<- outgoingAudio, chunks []string, buildPrompt func(string) string, logPrefix string) error {
	if len(chunks) == 0 {
		return nil
	}

	prompts := make([]string, len(chunks))
	for i, chunk := range chunks {
		prompts[i] = buildPrompt(chunk)
	}
	results := generateChunksParallel(ctx, ttsClient, prompts)
	streamID := uuid.NewString()

	// results[i] を順番に受け取り、完了次第即送信する。
	// 末尾判定のため、後続チャンクの results をノンブロッキングで先読みして
	// 成功チャンクが残っているかを確認する。先読み済み結果は cached に保存して二重待ちを防ぐ。
	n := len(prompts)
	cached := make([]*ttsChunkResult, n)

	getResult := func(i int) (ttsChunkResult, error) {
		if cached[i] != nil {
			r := *cached[i]
			cached[i] = nil
			return r, nil
		}
		select {
		case r := <-results[i]:
			return r, nil
		case <-ctx.Done():
			return ttsChunkResult{}, ctx.Err()
		}
	}

	sentCount := 0
	for i := 0; i < n; i++ {
		res, err := getResult(i)
		if err != nil {
			return err
		}
		if res.err != nil {
			log.Printf("%s TTS/encode error (chunk %d/%d): %v", logPrefix, i+1, n, res.err)
			continue
		}
		log.Printf("%s TTS generated chunk %d/%d (%d bytes)", logPrefix, i+1, n, len(res.ogg))

		// 後続に成功チャンクがあるか、ノンブロッキングで先読みして確認する
		hasNext := false
		for j := i + 1; j < n; j++ {
			if cached[j] != nil {
				if cached[j].err == nil {
					hasNext = true
					break
				}
				continue
			}
			select {
			case r := <-results[j]:
				r2 := r
				cached[j] = &r2
				if r.err == nil {
					hasNext = true
				}
			default:
				// まだ生成中 → 後続が存在するとみなす
				hasNext = true
			}
			break
		}

		var status pb.StreamStatus
		if sentCount == 0 {
			status = pb.StreamStatus_START
		} else if !hasNext {
			status = pb.StreamStatus_END
		} else {
			status = pb.StreamStatus_CONTINUE
		}

		out := outgoingAudio{Data: res.ogg, Status: status, StreamID: streamID}
		select {
		case sendCh <- out:
			log.Printf("%s sent chunk %d (status=%s stream_id=%s)", logPrefix, sentCount+1, status, streamID)
			sentCount++
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if sentCount == 0 {
		log.Printf("%s no TTS audio generated, nothing to send", logPrefix)
	}
	return nil
}
