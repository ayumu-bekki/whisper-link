package main

import (
	"fmt"
	"math/rand"
	"sync"
)

// コールサインのルール:
//   - プレフィックス: "S4"
//   - 後続2文字: [A B C D E G J K P R V Z] から選択
//   - 1文字目(3文字目全体): C は使用不可
//   - 例: S4AB, S4PZ など

// allChars: 後続2文字の2文字目に使える文字
var allChars = []rune{'A', 'B', 'C', 'D', 'E', 'G', 'J', 'K', 'P', 'R', 'V', 'Z'}

// 後続2文字の1文字目に使える文字 (C除外)
var firstChars = []rune{'A', 'B', 'D', 'E', 'G', 'J', 'K', 'P', 'R', 'V', 'Z'}

// CallsignManager はコールサインの発行と返却を管理する。
// prefix を変えることで S4 以外のコールサインにも使える。
type CallsignManager struct {
	mu       sync.Mutex
	prefix   string
	pool     []string // 未発行のコールサイン
	issued   map[string]struct{}
}

// NewCallsignManager は指定プレフィックスのマネージャーを生成する。
// prefix="S4" の場合、S4AB 〜 S4ZZ (ルール内) のプールを作成する。
func NewCallsignManager(prefix string) *CallsignManager {
	pool := buildPool(prefix)
	// シャッフルしてランダム順にする
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	return &CallsignManager{
		prefix: prefix,
		pool:   pool,
		issued: make(map[string]struct{}),
	}
}

// buildPool はルールに従い全コールサインを列挙する。
func buildPool(prefix string) []string {
	var pool []string
	for _, c1 := range firstChars {
		for _, c2 := range allChars {
			pool = append(pool, fmt.Sprintf("%s%c%c", prefix, c1, c2))
		}
	}
	return pool
}

// Issue はコールサインを1つ発行する。プールが空の場合は "" と false を返す。
func (m *CallsignManager) Issue() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.pool) == 0 {
		return "", false
	}
	cs := m.pool[len(m.pool)-1]
	m.pool = m.pool[:len(m.pool)-1]
	m.issued[cs] = struct{}{}
	return cs, true
}

// Release はコールサインをプールへ返却する。未発行のコールサインは無視する。
func (m *CallsignManager) Release(cs string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.issued[cs]; !ok {
		return
	}
	delete(m.issued, cs)
	m.pool = append(m.pool, cs)
}

// IssuedCount は現在発行中のコールサイン数を返す。
func (m *CallsignManager) IssuedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.issued)
}
