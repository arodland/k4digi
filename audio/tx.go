package audio

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/arodland/k4digi/k4"
	log "github.com/rs/zerolog/log"
	"github.com/smallnest/ringbuffer"
)

// TXLoop reads raw PCM from the PulseAudio sink pipe, assembles K4 audio frames,
// and sends them to writeCh. isPTT is polled each frame; audio is gated off
// (ring buffer drained without sending) when PTT is inactive. This matches QK4's
// behaviour: the K4 keys based on received audio level, so silence must not reach
// the K4 while in RX to avoid audio-triggered VOX keying.
func TXLoop(ctx context.Context, pipe *os.File, writeCh chan<- []byte, slTier int, isPTT func() bool) {
	frameSize := int(k4.SLFrameSizes[slTier])
	bytesPerFrame := frameSize * k4.TXChannels * k4.BytesPerSample

	rb := ringbuffer.New(bytesPerFrame * 20)
	readBuf := make([]byte, bytesPerFrame*2)
	frameBuf := make([]byte, bytesPerFrame)
	var seq byte
	var lastPTT bool

	// Pipe reader: drain the sink pipe into the ring buffer.
	// The stop channel is closed when TXLoop returns, signalling this goroutine
	// to exit after its current Read() completes. PulseAudio writes continuously
	// (use_system_clock_for_timing=yes), so Read() returns within ~1ms.
	stop := make(chan struct{})
	defer close(stop)

	go func() {
		for {
			n, err := pipe.Read(readBuf)
			if err != nil {
				if !errors.Is(err, os.ErrClosed) {
					log.Error().Err(err).Msg("TX pipe read failed")
				}
				return
			}
			if n > 0 {
				written, werr := rb.Write(readBuf[:n])
				if werr != nil || written < n {
					log.Warn().Int("written", written).Int("wanted", n).Msg("TX ring buffer full, audio dropped")
				}
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()

	for {
		ptt := isPTT()
		if ptt && !lastPTT {
			// PTT rising edge: reset sequence counter so the K4 sees a fresh stream.
			seq = 0
		}
		lastPTT = ptt

		if rb.Length() < bytesPerFrame {
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Millisecond):
			}
			continue
		}

		n, _ := rb.Read(frameBuf)
		if n < bytesPerFrame {
			continue
		}

		if !ptt {
			// PTT off: drain ring buffer without sending — keeps the read path
			// flowing so audio is ready the instant PTT goes active, but does not
			// feed the K4, preventing audio-level TX triggering on the radio.
			continue
		}

		payload := k4.BuildAudioPayload(seq, slTier, frameBuf[:n])
		frame := k4.BuildFrame(payload)
		seq++

		select {
		case writeCh <- frame:
		case <-ctx.Done():
			return
		}
	}
}
