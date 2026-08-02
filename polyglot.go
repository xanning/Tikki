package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
)

const pngCanvasSize = 640

var pngSignature = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
var canvasHeader []byte

func pngChunk(kind string, data []byte) []byte {
	buf := new(bytes.Buffer)
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(data)))
	buf.Write(length)
	buf.WriteString(kind)
	buf.Write(data)
	crc := crc32.NewIEEE()
	crc.Write([]byte(kind))
	crc.Write(data)
	crcBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(crcBytes, crc.Sum32())
	buf.Write(crcBytes)
	return buf.Bytes()
}

func buildWhiteCanvas(size int) ([]byte, error) {
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(size))
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(size))
	ihdr[8] = 8
	ihdr[9] = 6
	rowLen := 1 + size*4
	raw := make([]byte, rowLen*size)
	for y := 0; y < size; y++ {
		row := raw[y*rowLen : (y+1)*rowLen]
		for x := 0; x < size; x++ {
			px := row[1+x*4 : 5+x*4]
			px[0], px[1], px[2], px[3] = 0xff, 0xff, 0xff, 0xff
		}
	}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	out := new(bytes.Buffer)
	out.Write(pngSignature)
	out.Write(pngChunk("IHDR", ihdr))
	out.Write(pngChunk("IDAT", compressed.Bytes()))
	out.Write(pngChunk("IEND", nil))
	return out.Bytes(), nil
}

func initPolyglot() error {
	header, err := buildWhiteCanvas(pngCanvasSize)
	if err != nil {
		return err
	}
	canvasHeader = header
	return nil
}

func wrapPolyglot(payload []byte) ([]byte, int) {
	out := make([]byte, 0, len(canvasHeader)+len(payload))
	out = append(out, canvasHeader...)
	out = append(out, payload...)
	return out, len(canvasHeader)
}

func segmentKind(payload []byte) string {
	if isMPEGTS(payload) {
		return "ts"
	}
	return "m4s"
}

func isMPEGTS(payload []byte) bool {
	const packetSize = 188
	if len(payload) < packetSize*2 {
		return false
	}
	checks := len(payload) / packetSize
	if checks > 16 {
		checks = 16
	}
	for i := 0; i < checks; i++ {
		if payload[i*packetSize] != 0x47 {
			return false
		}
	}
	return true
}
