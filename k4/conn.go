package k4

import (
	"bufio"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"net"
	"time"

	log "github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"
)

// pingInterval matches QK4's PING_INTERVAL_MS.
const pingInterval = time.Second

// Conn represents an authenticated K4 TCP connection.
type Conn struct {
	conn    net.Conn
	reader  *bufio.Reader
	WriteCh chan []byte // accepts complete K4 frames ready to send
	RXCh    chan []byte // audio payloads received from K4
	CATCh   chan []byte // CAT payloads received from K4
}

// Dial connects to the K4, authenticates with SHA-384, and sends the init sequence.
func Dial(ctx context.Context, addr string, passphrase string, slTier int) (*Conn, error) {
	log.Info().Str("addr", addr).Msg("Connecting to K4")

	d := net.Dialer{}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	tcp := raw.(*net.TCPConn)
	tcp.SetNoDelay(true)
	tcp.SetKeepAlive(true)
	tcp.SetKeepAlivePeriod(30 * time.Second)

	// Auth: SHA-384(passphrase) as lowercase hex string, sent raw (no K4 framing).
	sum := sha512.Sum384([]byte(passphrase))
	authHex := []byte(hex.EncodeToString(sum[:]))
	log.Debug().Int("bytes", len(authHex)).Msg("Sending auth hash")
	if _, err := tcp.Write(authHex); err != nil {
		tcp.Close()
		return nil, fmt.Errorf("sending auth: %w", err)
	}

	reader := bufio.NewReaderSize(tcp, 65536)

	// Wait up to 5s for any frame — receipt confirms auth success.
	tcp.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := ReadFrame(reader); err != nil {
		tcp.Close()
		return nil, fmt.Errorf("auth response: %w", err)
	}
	tcp.SetDeadline(time.Time{})
	log.Info().Msg("Authenticated with K4")

	c := &Conn{
		conn:    tcp,
		reader:  reader,
		WriteCh: make(chan []byte, 128),
		RXCh:    make(chan []byte, 256),
		CATCh:   make(chan []byte, 256), // large enough for the RDY; state dump
	}

	// Init sequence: RDY triggers state dump; K4/K2 enable protocol features;
	// EM1 selects S16LE PCM audio; SL sets packet latency tier.
	initCmds := []string{
		"RDY;",
		"K4;",
		"K2;",
		fmt.Sprintf("EM%d;", EncodeRAW16),
		fmt.Sprintf("SL%d;", slTier),
	}
	for _, cmd := range initCmds {
		frame := BuildFrame(BuildCATPayload(cmd))
		if _, err := tcp.Write(frame); err != nil {
			tcp.Close()
			return nil, fmt.Errorf("sending init command %q: %w", cmd, err)
		}
		log.Debug().Str("cmd", cmd).Msg("Sent init command")
	}

	return c, nil
}

// Run starts the read, write, and ping loops, blocking until the connection closes or ctx is done.
// It always closes the underlying TCP connection and the RXCh/CATCh channels before returning.
func (c *Conn) Run(ctx context.Context) error {
	g, runCtx := errgroup.WithContext(ctx)
	g.Go(func() error { return c.readLoop(runCtx) })
	g.Go(func() error { return c.writeLoop(runCtx) })
	g.Go(func() error { return c.pingLoop(runCtx) })
	g.Go(func() error {
		<-runCtx.Done()
		c.conn.Close()
		return nil
	})
	err := g.Wait()
	close(c.RXCh)
	close(c.CATCh)
	return err
}

// pingLoop sends PING<timestamp>; every second to keep the K4 from closing the connection.
func (c *Conn) pingLoop(ctx context.Context) error {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case t := <-ticker.C:
			frame := BuildFrame(BuildCATPayload(fmt.Sprintf("PING%d;", t.Unix())))
			select {
			case c.WriteCh <- frame:
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *Conn) readLoop(ctx context.Context) error {
	for {
		payload, err := ReadFrame(c.reader)
		if err != nil {
			return err
		}
		if len(payload) == 0 {
			continue
		}
		switch payload[0] {
		case PayloadAudio:
			select {
			case c.RXCh <- payload:
			case <-ctx.Done():
				return ctx.Err()
			default:
				log.Warn().Msg("RX audio buffer full, dropping frame")
			}
		case PayloadCAT:
			select {
			case c.CATCh <- payload:
			case <-ctx.Done():
				return ctx.Err()
			default:
				log.Warn().Msg("CAT buffer full, dropping frame")
			}
		}
		// PAN/MiniPAN frames ignored
	}
}

func (c *Conn) writeLoop(ctx context.Context) error {
	for {
		select {
		case frame, ok := <-c.WriteCh:
			if !ok {
				return nil
			}
			if _, err := c.conn.Write(frame); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// SendCAT enqueues a CAT command string for transmission to the K4.
func (c *Conn) SendCAT(ctx context.Context, cmd string) {
	select {
	case c.WriteCh <- BuildFrame(BuildCATPayload(cmd)):
	case <-ctx.Done():
	}
}
