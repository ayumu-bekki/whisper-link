package main

import "fmt"

// baseInstructionRules は全ペルソナ共通の口調・フォーマット・カナ変換ルール。
// 各ペルソナ固有のキャラ設定にこれを結合して最終的な System Instruction を構築する。
const baseInstructionRules = `以下のルールに必ず従ってください:
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

// Persona は NPC1体分のキャラ設定。
type Persona struct {
	// Character はペルソナ固有のキャラ設定（性格・口調の方向性など）。
	// %s に自分のコールサインを差し込むテンプレート文字列。
	Character string
}

// SystemInstruction は自分のコールサインを差し込んだ最終的な System Instruction を返す。
// Character（CS差し込み済み）に共通ルールを結合する。
func (p Persona) SystemInstruction(selfCallsign string) string {
	return fmt.Sprintf(p.Character, selfCallsign) + "\n" + baseInstructionRules
}

// Scenario は NPC に払い出し順で割り当てるペルソナの順序付きリスト。
// 将来はユーザーが選択するシナリオが NPC 割り当て数(=len(Personas))を決める。
type Scenario struct {
	Personas []Persona
}

// PersonaFor は払い出し順インデックス idx に対応するペルソナを返す。
// idx がペルソナ数を超える場合は循環参照する（将来のシナリオ機能で本格対応する暫定挙動）。
func (s Scenario) PersonaFor(idx int) Persona {
	n := len(s.Personas)
	if n == 0 {
		return Persona{}
	}
	return s.Personas[idx%n]
}

// defaultScenario は今回の固定シナリオ。ペルソナ1体のみを持つ。
// 既存の汎用オペレーター設定をそのまま1ペルソナとして定義する。
func defaultScenario() Scenario {
	return Scenario{
		Personas: []Persona{
			{Character: `あなたはアマチュア無線のオペレーターです。あなたのコールサインは%sです。`},
		},
	}
}
