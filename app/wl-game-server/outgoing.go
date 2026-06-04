package main

import pb "wl-game-server/gen"

// outgoingAudio は radio-bridge へ送出する 1 つの音声パケットを表す。
// sendCh はこの型を流し、bridge_client が AudioChunk へ変換して送信する。
//
//   - 単発再生 (S4CE/S4CA/WS chat): Status=ONESHOT, StreamID="" で 1 パケット送出。
//   - 連続再生 (S4CQ 分割送信): 同一 StreamID に START → CONTINUE... → END を付けて
//     チャンクごとに送出。radio-bridge は同一 StreamID を 1 区間で連続再生する。
type outgoingAudio struct {
	Data     []byte
	Status   pb.StreamStatus
	StreamID string
}

// oneshot は単発再生用の outgoingAudio を組み立てるヘルパー。
func oneshot(data []byte) outgoingAudio {
	return outgoingAudio{Data: data, Status: pb.StreamStatus_ONESHOT}
}
