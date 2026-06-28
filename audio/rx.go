package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"os"

	"github.com/arodland/k4digi/k4"
	log "github.com/rs/zerolog/log"
)

// em1GainFactor compensates for the K4's EM1 audio level.
// The K4 ships EM1 at approximately -35 dBFS; 16× ≈ 24 dB boost brings it
// to nominal listening level, matching QK4's normalization.
const em1GainFactor = 16

// applyGain multiplies every S16LE sample in pcm by factor, clipping at ±32767.
func applyGain(pcm []byte, factor int32) {
	for i := 0; i+1 < len(pcm); i += 2 {
		s := int32(int16(binary.LittleEndian.Uint16(pcm[i:])))
		s *= factor
		if s > 32767 {
			s = 32767
		} else if s < -32768 {
			s = -32768
		}
		binary.LittleEndian.PutUint16(pcm[i:], uint16(int16(s)))
	}
}

// startPipeWriter launches a goroutine that drains ch into pipe.
// The caller must close(ch) to stop it.
func startPipeWriter(pipe *os.File) chan<- []byte {
	ch := make(chan []byte, 8)
	go func() {
		for data := range ch {
			if _, err := pipe.Write(data); err != nil {
				if !errors.Is(err, os.ErrClosed) {
					log.Error().Err(err).Msg("RX pipe write failed")
				}
				return
			}
		}
	}()
	return ch
}

// validateAudioPayload checks that payload is a valid EM1 audio frame and
// returns the raw PCM slice (after the header). Returns nil on any mismatch.
func validateAudioPayload(payload []byte) []byte {
	if len(payload) <= k4.AudioHeaderSize {
		return nil
	}
	if payload[k4.AudioTypeOffset] != k4.PayloadAudio {
		return nil
	}
	if payload[k4.AudioModeOffset] != k4.EncodeRAW16 {
		log.Warn().Uint8("mode", payload[k4.AudioModeOffset]).
			Msg("Unexpected audio encode mode from K4 (expected EM1/RAW16)")
		return nil
	}
	return payload[k4.AudioDataOffset:]
}

// RXLoop receives K4 EM1 audio payloads and writes stereo PCM to a single
// PulseAudio pipe (left = VFO A, right = VFO B).
//
// Pipe writes run in a separate goroutine so that a full pipe (PulseAudio not
// consuming) never blocks the select loop.
func RXLoop(ctx context.Context, rxCh <-chan []byte, pipe *os.File) {
	pipeWriteCh := startPipeWriter(pipe)
	defer close(pipeWriteCh)

	for {
		select {
		case <-ctx.Done():
			return
		case payload, ok := <-rxCh:
			if !ok {
				return
			}
			pcm := validateAudioPayload(payload)
			if pcm == nil {
				continue
			}
			buf := make([]byte, len(pcm))
			copy(buf, pcm)
			applyGain(buf, em1GainFactor)
			select {
			case pipeWriteCh <- buf:
			case <-ctx.Done():
				return
			default:
				log.Debug().Msg("RX pipe write buffer full, dropping audio frame")
			}
		}
	}
}

// RXLoopSplit receives K4 EM1 audio payloads and demultiplexes the interleaved
// stereo stream into two mono PulseAudio pipes: pipeA receives VFO A (left
// channel) and pipeB receives VFO B (right channel).
func RXLoopSplit(ctx context.Context, rxCh <-chan []byte, pipeA, pipeB *os.File) {
	writeA := startPipeWriter(pipeA)
	writeB := startPipeWriter(pipeB)
	defer close(writeA)
	defer close(writeB)

	for {
		select {
		case <-ctx.Done():
			return
		case payload, ok := <-rxCh:
			if !ok {
				return
			}
			pcm := validateAudioPayload(payload)
			if pcm == nil {
				continue
			}
			buf := make([]byte, len(pcm))
			copy(buf, pcm)
			applyGain(buf, em1GainFactor)

			// Deinterleave S16LE stereo: frames are [L0 L1 R0 R1] repeating.
			nFrames := len(buf) / 4
			monoA := make([]byte, nFrames*2)
			monoB := make([]byte, nFrames*2)
			for i := range nFrames {
				monoA[i*2] = buf[i*4]
				monoA[i*2+1] = buf[i*4+1]
				monoB[i*2] = buf[i*4+2]
				monoB[i*2+1] = buf[i*4+3]
			}

			select {
			case writeA <- monoA:
			case <-ctx.Done():
				return
			default:
				log.Debug().Msg("RX-A pipe write buffer full, dropping audio frame")
			}
			select {
			case writeB <- monoB:
			case <-ctx.Done():
				return
			default:
				log.Debug().Msg("RX-B pipe write buffer full, dropping audio frame")
			}
		}
	}
}
