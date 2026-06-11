package main

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/klauspost/compress/zstd"
)

// ---------------------------------------------------------------------------
// PCM binary packet decoder
// ---------------------------------------------------------------------------
// The UberSDR server sends packets in the ubersdr hybrid binary format.
// Two packet types:
//
//	Full header v1 (magic 0x5043 "PC", 29 bytes):
//	  [0:2]   uint16  magic
//	  [2]     uint8   version
//	  [3]     uint8   format (0=PCM, 2=PCM-zstd)
//	  [4:12]  uint64  GPS timestamp in nanoseconds (LE)
//	  [12:20] uint64  wall-clock ms (LE)
//	  [20:24] uint32  sample rate (LE)
//	  [24]    uint8   channels
//	  [25:29] uint32  reserved
//	  [29:]   []byte  PCM samples (big-endian int16)
//
//	Full header v2 (37 bytes) adds signal quality fields:
//	  [4:12]  uint64  GPS timestamp in nanoseconds (LE)  ← extracted for TDOA
//	  [25:29] float32 baseband power dBFS
//	  [29:33] float32 noise density dBFS
//	  [33:37] uint32  reserved
//	  [37:]   []byte  PCM samples (big-endian int16)
//
//	Minimal header (magic 0x504D "PM", 13 bytes):
//	  [0:2]   uint16  magic
//	  [2]     uint8   version
//	  [3:11]  uint64  GPS timestamp in nanoseconds (LE)  ← present in every packet
//	  [11:13] uint16  reserved
//	  [13:]   []byte  PCM samples (big-endian int16)
//
// For iq48 mode the PCM payload contains interleaved S16BE I/Q pairs at
// 48 kHz (stereo, channels=2). The decoder converts to S16LE and sets
// channels=2 so callers can split I and Q channels as needed.

const (
	magicFull    = 0x5043 // "PC"
	magicMinimal = 0x504D // "PM"
)

// pcmPacket is the result of decoding one binary WebSocket message.
type pcmPacket struct {
	pcm          []byte  // little-endian int16 PCM samples (interleaved I/Q for iq48)
	sampleRate   int
	channels     int
	timestampNs  int64   // GPS-synchronised Unix timestamp in nanoseconds (every packet)
	hasSigInfo   bool    // true only for v2 full-header packets
	basebandDBFS float32 // baseband power dBFS (v2 only; -999 = no data)
	noiseDBFS    float32 // noise density dBFS (v2 only; -999 = no data)
}

type pcmDecoder struct {
	zd           *zstd.Decoder
	lastRate     int
	lastChannels int
}

func newPCMDecoder() (*pcmDecoder, error) {
	zd, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("zstd init: %w", err)
	}
	return &pcmDecoder{zd: zd}, nil
}

// decode decompresses (if needed) and parses a binary PCM packet.
// Returns a pcmPacket with little-endian int16 PCM bytes, GPS timestamp,
// and optional signal quality info.
func (d *pcmDecoder) decode(data []byte, isZstd bool) (pcmPacket, error) {
	if isZstd {
		var err error
		data, err = d.zd.DecodeAll(data, nil)
		if err != nil {
			return pcmPacket{}, fmt.Errorf("zstd decompress: %w", err)
		}
	}

	if len(data) < 4 {
		return pcmPacket{}, fmt.Errorf("packet too short (%d bytes)", len(data))
	}

	magic := binary.LittleEndian.Uint16(data[0:2])

	var pkt pcmPacket
	pkt.basebandDBFS = -999
	pkt.noiseDBFS = -999

	var raw []byte

	switch magic {
	case magicFull:
		version := data[2]
		var headerLen int
		switch version {
		case 2:
			headerLen = 37
		default: // version 1
			headerLen = 29
		}
		if len(data) < headerLen {
			return pcmPacket{}, fmt.Errorf("full-header packet too short (%d < %d)", len(data), headerLen)
		}

		// GPS timestamp is at offset 4–11 in both v1 and v2 full headers.
		pkt.timestampNs = int64(binary.LittleEndian.Uint64(data[4:12]))

		pkt.sampleRate = int(binary.LittleEndian.Uint32(data[20:24]))
		pkt.channels = int(data[24])
		raw = data[headerLen:]
		d.lastRate = pkt.sampleRate
		d.lastChannels = pkt.channels

		if version == 2 {
			pkt.hasSigInfo = true
			pkt.basebandDBFS = math.Float32frombits(binary.LittleEndian.Uint32(data[25:29]))
			pkt.noiseDBFS = math.Float32frombits(binary.LittleEndian.Uint32(data[29:33]))
		}

	case magicMinimal:
		if len(data) < 13 {
			return pcmPacket{}, fmt.Errorf("minimal-header packet too short (%d bytes)", len(data))
		}

		// GPS timestamp is at offset 3–10 in the minimal header.
		pkt.timestampNs = int64(binary.LittleEndian.Uint64(data[3:11]))

		raw = data[13:]
		pkt.sampleRate = d.lastRate
		pkt.channels = d.lastChannels
		if pkt.sampleRate == 0 || pkt.channels == 0 {
			return pcmPacket{}, fmt.Errorf("minimal header received before full header")
		}

	default:
		return pcmPacket{}, fmt.Errorf("unknown magic 0x%04X", magic)
	}

	// Convert big-endian int16 → little-endian int16.
	// For iq48 mode the payload is interleaved S16BE I/Q pairs (channels=2);
	// the same byte-swap applies — callers split I and Q after decoding.
	n := len(raw) / 2
	le := make([]byte, len(raw))
	for i := 0; i < n; i++ {
		s := binary.BigEndian.Uint16(raw[i*2:])
		binary.LittleEndian.PutUint16(le[i*2:], s)
	}
	pkt.pcm = le
	return pkt, nil
}

func (d *pcmDecoder) close() { d.zd.Close() }

// splitIQ splits an interleaved S16LE I/Q buffer (channels=2, iq48 mode)
// into separate I and Q float64 slices normalised to [-1, 1].
// The input must have an even number of int16 samples (i.e. len divisible by 4).
func splitIQ(pcmLE []byte) (iSamples, qSamples []float64) {
	n := len(pcmLE) / 4 // 2 bytes per sample × 2 channels
	iSamples = make([]float64, n)
	qSamples = make([]float64, n)
	const scale = 1.0 / 32768.0
	for i := 0; i < n; i++ {
		iSamples[i] = float64(int16(binary.LittleEndian.Uint16(pcmLE[i*4:]))) * scale
		qSamples[i] = float64(int16(binary.LittleEndian.Uint16(pcmLE[i*4+2:]))) * scale
	}
	return
}

// envelope computes the instantaneous amplitude √(I²+Q²) for each sample pair.
// Input is the same interleaved S16LE I/Q buffer as produced by decode().
func envelope(pcmLE []byte) []float64 {
	n := len(pcmLE) / 4
	env := make([]float64, n)
	const scale = 1.0 / 32768.0
	for i := 0; i < n; i++ {
		iv := float64(int16(binary.LittleEndian.Uint16(pcmLE[i*4:]))) * scale
		qv := float64(int16(binary.LittleEndian.Uint16(pcmLE[i*4+2:]))) * scale
		env[i] = math.Sqrt(iv*iv + qv*qv)
	}
	return env
}
