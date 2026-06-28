package k4

import (
	"encoding/binary"
	"fmt"
	"io"
)

var startMarker = [4]byte{0xFE, 0xFD, 0xFC, 0xFB}
var endMarker = [4]byte{0xFB, 0xFC, 0xFD, 0xFE}

const MaxFrameSize = 1 << 20 // 1 MB

// Payload types
const (
	PayloadCAT   byte = 0x00
	PayloadAudio byte = 0x01
	PayloadPAN   byte = 0x02
	PayloadMini  byte = 0x03
)

// Audio encode modes
const (
	EncodeRAW32 byte = 0
	EncodeRAW16 byte = 1 // S16LE — our default
	EncodeOpusI byte = 2
	EncodeOpusF byte = 3
)

// Audio packet header offsets
const (
	AudioTypeOffset       = 0
	AudioVersionOffset    = 1
	AudioSeqOffset        = 2
	AudioModeOffset       = 3
	AudioFrameSizeOffset  = 4 // uint16 LE, samples per channel
	AudioSampleRateOffset = 6
	AudioDataOffset       = 7
	AudioHeaderSize       = 7
)

// SL tier → samples per channel at 12 kHz
var SLFrameSizes = [8]uint16{240, 480, 720, 1440, 2400, 4800, 7200, 14400}

const (
	SampleRate     = 12000
	RXChannels     = 2 // stereo: left=main VFO A, right=sub VFO B
	TXChannels     = 2 // stereo TX — K4 expects the same frame geometry as RX
	BytesPerSample = 2 // s16le
)

// BuildFrame wraps a payload in K4 binary framing.
func BuildFrame(payload []byte) []byte {
	frame := make([]byte, 4+4+len(payload)+4)
	copy(frame[0:4], startMarker[:])
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	copy(frame[8+len(payload):], endMarker[:])
	return frame
}

// ReadFrame reads one frame from r and returns its payload.
// It scans byte-by-byte for the start marker, handling TCP stream splits.
func ReadFrame(r io.Reader) ([]byte, error) {
	var window [4]byte
	b := [1]byte{}
	for window != startMarker {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, fmt.Errorf("scanning for start marker: %w", err)
		}
		window[0] = window[1]
		window[1] = window[2]
		window[2] = window[3]
		window[3] = b[0]
	}

	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("reading frame length: %w", err)
	}
	length := binary.BigEndian.Uint32(lenBuf[:])
	if length > MaxFrameSize {
		return nil, fmt.Errorf("frame too large: %d bytes", length)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("reading payload: %w", err)
	}

	var end [4]byte
	if _, err := io.ReadFull(r, end[:]); err != nil {
		return nil, fmt.Errorf("reading end marker: %w", err)
	}
	if end != endMarker {
		return nil, fmt.Errorf("bad end marker: %x", end)
	}

	return payload, nil
}

// BuildCATPayload builds a K4 CAT payload: type(1) + padding(2) + ASCII command.
func BuildCATPayload(cmd string) []byte {
	p := make([]byte, 3+len(cmd))
	p[0] = PayloadCAT
	p[1] = 0x00
	p[2] = 0x00
	copy(p[3:], cmd)
	return p
}

// ParseCATPayload extracts the ASCII command string from a CAT payload.
func ParseCATPayload(p []byte) (string, error) {
	if len(p) < 3 {
		return "", fmt.Errorf("CAT payload too short: %d bytes", len(p))
	}
	if p[0] != PayloadCAT {
		return "", fmt.Errorf("not a CAT payload (type=%02x)", p[0])
	}
	return string(p[3:]), nil
}

// BuildAudioPayload builds a K4 EM1 (RAW16/S16LE) audio payload.
func BuildAudioPayload(seq byte, slTier int, pcm []byte) []byte {
	p := make([]byte, AudioHeaderSize+len(pcm))
	p[AudioTypeOffset] = PayloadAudio
	p[AudioVersionOffset] = 0x01
	p[AudioSeqOffset] = seq
	p[AudioModeOffset] = EncodeRAW16
	binary.LittleEndian.PutUint16(p[AudioFrameSizeOffset:], SLFrameSizes[slTier])
	p[AudioSampleRateOffset] = 0x00 // 12 kHz
	copy(p[AudioDataOffset:], pcm)
	return p
}
