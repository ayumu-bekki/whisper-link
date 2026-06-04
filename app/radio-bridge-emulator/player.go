package main

import (
	"log"
	"sync"
	"time"

	pb "radio-bridge-emulator/proto"
)

// player は受信した AudioChunk をシリアルに再生するキュー付きプレイヤー。
//
// radio-bridge (Rust) の Controller / AudioQueue を macOS 上で擬似再現する。
// GPIO/PTT は無いが、連続再生制御 (stream_id + status) のロジックは同等にする。
//
// 設計: stream_id をキーとする dict 構造。
//   - order: 再生する stream_id の順序 (FIFO)。同一 id は 1 回のみ登録。
//   - streams: stream_id → チャンクスライス (到着順)。
//   - 別 stream_id のチャンクは別キーに溜まるだけで、現ストリームに割り込めない。
//   - ONESHOT は server.go 側で内部一意 ID を振り 1 チャンクのストリームとして扱う。
const (
	// 連続再生中にキューが空になったとき後続を待つ時間。radio-bridge の
	// STREAM_TIMEOUT_MS と揃える。
	streamTimeout = 2 * time.Second
	// クローズ済み stream_id を覚えておく最大件数。
	maxClosedStreams = 16
)

type playerEntry struct {
	data   []byte
	status pb.StreamStatus
}

type player struct {
	mu   sync.Mutex
	cond *sync.Cond

	// 再生順序。同一 stream_id は 1 回だけ並ぶ。
	order []string
	// stream_id → チャンク列。
	streams map[string][]*playerEntry

	// クローズ済み stream_id (直近 maxClosedStreams 件)。
	closedOrder []string
	closedSet   map[string]struct{}
}

func newPlayer() *player {
	p := &player{
		streams:   make(map[string][]*playerEntry),
		closedSet: make(map[string]struct{}),
	}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// push はチャンクをキューに積む。
// UNKNOWN / 未知値は破棄。クローズ済みストリームの遅延チャンクも破棄。
// streamID は呼び出し側で必ず非空にすること (ONESHOT も内部 ID を振る)。
func (p *player) push(data []byte, status pb.StreamStatus, streamID string) {
	switch status {
	case pb.StreamStatus_ONESHOT, pb.StreamStatus_START, pb.StreamStatus_CONTINUE, pb.StreamStatus_END:
		// 有効。
	default:
		log.Printf("[play] audio discarded: invalid status (raw=%d)", int32(status))
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, closed := p.closedSet[streamID]; closed {
		log.Printf("[play] audio discarded: stream closed (stream_id=%s)", streamID)
		return
	}

	entry := &playerEntry{data: data, status: status}
	if _, exists := p.streams[streamID]; !exists {
		p.order = append(p.order, streamID)
		p.streams[streamID] = []*playerEntry{entry}
	} else {
		p.streams[streamID] = append(p.streams[streamID], entry)
	}
	p.cond.Signal()
}

// markClosed は stream_id をクローズ済みとして記録する (要ロック済み)。
func (p *player) markClosed(streamID string) {
	if streamID == "" {
		return
	}
	if _, ok := p.closedSet[streamID]; ok {
		return
	}
	p.closedSet[streamID] = struct{}{}
	p.closedOrder = append(p.closedOrder, streamID)
	for len(p.closedOrder) > maxClosedStreams {
		old := p.closedOrder[0]
		p.closedOrder = p.closedOrder[1:]
		delete(p.closedSet, old)
	}
}

// currentStream は order 先頭の stream_id を返す (要ロック済み)。"" は空。
func (p *player) currentStream() string {
	if len(p.order) == 0 {
		return ""
	}
	return p.order[0]
}

// currentHasChunk は現ストリームに未再生チャンクがあるか (要ロック済み)。
func (p *player) currentHasChunk() bool {
	id := p.currentStream()
	if id == "" {
		return false
	}
	return len(p.streams[id]) > 0
}

// popCurrent は現ストリームの先頭チャンクを取り出す (要ロック済み)。
func (p *player) popCurrent() *playerEntry {
	id := p.currentStream()
	if id == "" {
		return nil
	}
	chunks := p.streams[id]
	if len(chunks) == 0 {
		return nil
	}
	entry := chunks[0]
	p.streams[id] = chunks[1:]
	return entry
}

// finishCurrent は現ストリームを完了として閉じ order を進める (要ロック済み)。
func (p *player) finishCurrent() {
	if len(p.order) == 0 {
		return
	}
	id := p.order[0]
	p.order = p.order[1:]
	delete(p.streams, id)
	p.markClosed(id)
}

// hasStream は再生対象ストリームがあるか (要ロック済み)。
func (p *player) hasStream() bool {
	return len(p.order) > 0
}

// run は再生ループ。goroutine で 1 本だけ起動する。
func (p *player) run() {
	for {
		p.mu.Lock()
		for !p.hasStream() {
			p.cond.Wait()
		}
		streamID := p.currentStream()
		p.mu.Unlock()

		log.Printf("[play] stream playback started (stream_id=%s)", streamID)
		p.playStream(streamID)
	}
}

// playStream は 1 ストリームを連続再生する。
func (p *player) playStream(streamID string) {
	for {
		p.mu.Lock()
		entry := p.popCurrent()
		p.mu.Unlock()

		if entry != nil {
			isTerminal := entry.status == pb.StreamStatus_END || entry.status == pb.StreamStatus_ONESHOT
			p.playEntry(streamID, entry)

			if isTerminal {
				log.Printf("[play] stream complete (stream_id=%s)", streamID)
				p.mu.Lock()
				p.finishCurrent()
				p.mu.Unlock()
				return
			}
			continue
		}

		// 現ストリームのチャンクが一時的に尽きた。streamTimeout だけ後続を待つ。
		if p.waitForChunk(streamID) {
			continue
		}
		log.Printf("[play] stream timeout (stream_id=%s)", streamID)
		p.mu.Lock()
		p.finishCurrent()
		p.mu.Unlock()
		return
	}
}

// waitForChunk は streamTimeout の間、現ストリームのチャンク到着を待つ。
// チャンクが来たら true、タイムアウトなら false を返す。
func (p *player) waitForChunk(streamID string) bool {
	const poll = 100 * time.Millisecond
	deadline := time.Now().Add(streamTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(poll)
		p.mu.Lock()
		has := p.currentHasChunk()
		p.mu.Unlock()
		if has {
			return true
		}
	}
	return false
}

// playEntry は 1 エントリを再生する (完了までブロック)。
func (p *player) playEntry(streamID string, entry *playerEntry) {
	log.Printf("[play] playing %d bytes (status=%s stream_id=%s)", len(entry.data), entry.status, streamID)
	if err := playOggOpus(entry.data); err != nil {
		log.Printf("[play] error: %v", err)
	}
}
