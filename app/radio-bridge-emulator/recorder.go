package main

import (
	"log"
	"sync"
)

// recorder はバックグラウンドでマイク録音を行い、
// 録音完了ごとに購読者チャネルへ Ogg Opus データを配信する。
type recorder struct {
	cfg  AudioConfig
	mu   sync.Mutex
	subs []chan []byte
}

func newRecorder(cfg AudioConfig) *recorder {
	return &recorder{cfg: cfg}
}

func (r *recorder) subscribe() <-chan []byte {
	ch := make(chan []byte, 8)
	r.mu.Lock()
	r.subs = append(r.subs, ch)
	r.mu.Unlock()
	return ch
}

func (r *recorder) broadcast(data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range r.subs {
		select {
		case ch <- data:
		default:
			// バックプレッシャー: 受信が追いつかなければドロップ
		}
	}
}

// run はマイク録音ループを開始する。goroutine で呼ぶこと。
func (r *recorder) run() {
	for {
		data, err := recordUntilSilence(r.cfg)
		if err != nil {
			log.Printf("[recorder] error: %v", err)
			continue
		}
		if data == nil {
			continue
		}
		log.Printf("[recorder] recorded %d bytes, broadcasting", len(data))
		r.broadcast(data)
	}
}
