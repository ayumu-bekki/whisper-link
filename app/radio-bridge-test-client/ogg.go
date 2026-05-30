package main

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
)

const framesPerPage = 10 // Oggページあたりのフレーム数 (200ms)

// Oggページライター (シンプルな実装)
type oggWriter struct {
	buf        bytes.Buffer
	serial     uint32
	pageSeqNo  uint32
	packetBuf  [][]byte
	granuleAcc uint64
}

func newOggWriter() *oggWriter {
	return &oggWriter{serial: 0x57485052} // "WHPR"
}

// writePage はヘッダパケット(OpusHead/OpusTags)を1ページとして書き出す。
func (w *oggWriter) writePage(packet []byte, granule uint64, bos bool, eos bool) {
	w.writeOggPage([][]byte{packet}, granule, bos, eos)
	w.pageSeqNo++
}

// writeAudioPacket は音声パケットをバッファに積み、framesPerPage溜まったら書き出す。
func (w *oggWriter) writeAudioPacket(packet []byte, granule uint64) {
	w.packetBuf = append(w.packetBuf, packet)
	w.granuleAcc = granule
	if len(w.packetBuf) >= framesPerPage {
		w.writeOggPage(w.packetBuf, w.granuleAcc, false, false)
		w.pageSeqNo++
		w.packetBuf = nil
	}
}

// flush は残っているパケットを最終ページとして書き出す。
func (w *oggWriter) flush() {
	if len(w.packetBuf) > 0 {
		w.writeOggPage(w.packetBuf, w.granuleAcc, false, true)
		w.pageSeqNo++
		w.packetBuf = nil
	}
}

func (w *oggWriter) bytes() []byte {
	return w.buf.Bytes()
}

// writeOggPage はOgg仕様に従ったページバイナリを書き出す。
func (w *oggWriter) writeOggPage(packets [][]byte, granulePos uint64, bos bool, eos bool) {
	// セグメントテーブルを構築
	var segments []byte
	for _, p := range packets {
		remaining := len(p)
		for remaining >= 255 {
			segments = append(segments, 255)
			remaining -= 255
		}
		segments = append(segments, byte(remaining))
	}

	headerType := byte(0)
	if bos {
		headerType |= 0x02
	}
	if eos {
		headerType |= 0x04
	}

	var page bytes.Buffer
	page.WriteString("OggS")          // capture pattern
	page.WriteByte(0)                  // version
	page.WriteByte(headerType)         // header type
	binary.Write(&page, binary.LittleEndian, granulePos) // granule position
	binary.Write(&page, binary.LittleEndian, w.serial)   // serial number
	binary.Write(&page, binary.LittleEndian, w.pageSeqNo) // page sequence number
	binary.Write(&page, binary.LittleEndian, uint32(0))  // checksum placeholder
	page.WriteByte(byte(len(segments))) // page segments
	page.Write(segments)               // segment table

	for _, p := range packets {
		page.Write(p)
	}

	pageBytes := page.Bytes()

	// CRC32計算 (Ogg固有のテーブル)
	crc := crc32Ogg(pageBytes)
	binary.LittleEndian.PutUint32(pageBytes[22:26], crc)

	w.buf.Write(pageBytes)
}

// buildOpusHead はOpusHeadパケットを返す。
func buildOpusHead() []byte {
	var b bytes.Buffer
	b.WriteString("OpusHead")
	b.WriteByte(1)   // version
	b.WriteByte(1)   // channels (mono)
	binary.Write(&b, binary.LittleEndian, uint16(0))     // pre-skip
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate)) // input sample rate
	binary.Write(&b, binary.LittleEndian, int16(0))      // output gain
	b.WriteByte(0)   // channel mapping family
	return b.Bytes()
}

// buildOpusTags はOpusTagsパケットを返す。
func buildOpusTags() []byte {
	var b bytes.Buffer
	b.WriteString("OpusTags")
	vendor := []byte("whisper-link-client")
	binary.Write(&b, binary.LittleEndian, uint32(len(vendor)))
	b.Write(vendor)
	binary.Write(&b, binary.LittleEndian, uint32(0)) // user comment list length
	return b.Bytes()
}

// Ogg専用CRC32テーブル (polynomial: 0x04c11db7, no reflection)
var oggCRCTable = func() *crc32.Table {
	// Oggは独自のCRCを使うため標準ライブラリのテーブルは使えない
	// 手動でテーブルを構築する
	var t [256]uint32
	for i := 0; i < 256; i++ {
		crc := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
		t[i] = crc
	}
	// crc32.MakeTableは使わず直接計算する
	return nil
}()

var oggCRCTableData [256]uint32

func init() {
	for i := 0; i < 256; i++ {
		crc := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
		oggCRCTableData[i] = crc
	}
}

func crc32Ogg(data []byte) uint32 {
	crc := uint32(0)
	for _, b := range data {
		crc = (crc << 8) ^ oggCRCTableData[byte(crc>>24)^b]
	}
	return crc
}
