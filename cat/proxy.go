package cat

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/arodland/k4digi/k4"
	log "github.com/rs/zerolog/log"
)

// Proxy is a multi-client TCP CAT passthrough server. It fans out responses
// from the K4 to all connected clients, and forwards client commands to the K4.
type Proxy struct {
	addr string

	mu        sync.RWMutex
	conn      *k4.Conn // current K4 connection; may be nil between reconnects
	clients   map[net.Conn]chan []byte
	pttActive atomic.Bool // true when any client has sent TX; and not yet RX;
}

// IsPTTActive reports whether any CAT client has asserted PTT (TX;) and not
// yet released it (RX;). Used by TXLoop to gate audio frames to the K4.
func (p *Proxy) IsPTTActive() bool { return p.pttActive.Load() }

// ClearPTT resets PTT to the off state. Called when the K4 connection drops
// so a fresh reconnect doesn't inherit stale TX state.
func (p *Proxy) ClearPTT() { p.pttActive.Store(false) }

func NewProxy(addr string) *Proxy {
	return &Proxy{
		addr:    addr,
		clients: make(map[net.Conn]chan []byte),
	}
}

// SendCAT sends a CAT command to the K4 if a connection is active.
func (p *Proxy) SendCAT(ctx context.Context, cmd string) {
	p.mu.RLock()
	conn := p.conn
	p.mu.RUnlock()
	if conn != nil {
		conn.SendCAT(ctx, cmd)
	}
}

// SetConn swaps in a new K4 connection (or nil when disconnected).
func (p *Proxy) SetConn(conn *k4.Conn) {
	p.mu.Lock()
	p.conn = conn
	p.mu.Unlock()
}

// BroadcastFromK4 fans a K4 CAT payload out to all connected clients.
func (p *Proxy) BroadcastFromK4(payload []byte) {
	ascii, err := k4.ParseCATPayload(payload)
	if err != nil || len(ascii) == 0 {
		return
	}
	// PING/PONG are internal keepalives; external clients don't understand them
	// and will misbehave (e.g. WSJT-X rapid PTT toggling) if they receive them.
	if strings.HasPrefix(ascii, "PING") || strings.HasPrefix(ascii, "PONG") {
		return
	}
	data := []byte(ascii)

	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, ch := range p.clients {
		select {
		case ch <- data:
		default:
			// client's send buffer full; drop rather than block
		}
	}
}

// Serve accepts connections until ctx is cancelled. It binds the listener once
// and keeps it alive across K4 reconnects.
func (p *Proxy) Serve(ctx context.Context) error {
	l, err := net.Listen("tcp", p.addr)
	if err != nil {
		return err
	}
	log.Info().Str("addr", p.addr).Msg("CAT proxy listening")

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Error().Err(err).Msg("CAT proxy accept error")
			continue
		}
		go p.handleClient(ctx, conn)
	}
}

func (p *Proxy) handleClient(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	log.Info().Str("client", remote).Msg("CAT client connected")

	sendCh := make(chan []byte, 64)
	p.mu.Lock()
	p.clients[conn] = sendCh
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		delete(p.clients, conn)
		p.mu.Unlock()
		log.Info().Str("client", remote).Msg("CAT client disconnected")
	}()

	// Writer goroutine: forward K4 responses to this client.
	go func() {
		for {
			select {
			case data, ok := <-sendCh:
				if !ok {
					return
				}
				if _, err := conn.Write(data); err != nil {
					conn.Close()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Reader: parse semicolon-delimited commands, handle intercepted ones locally,
	// and batch the rest into a single K4 frame per read cycle.
	var cmdBuf []byte
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			cmdBuf = append(cmdBuf, buf[:n]...)

			if len(cmdBuf) > 65536 {
				log.Warn().Str("client", remote).Msg("CAT command buffer overflow, disconnecting")
				return
			}

			var toK4 []byte
			for {
				idx := bytes.IndexByte(cmdBuf, ';')
				if idx < 0 {
					break
				}
				cmd := cmdBuf[:idx+1]
				cmdBuf = cmdBuf[idx+1:]

				switch string(cmd) {
				case "TX;":
					p.pttActive.Store(true)
					log.Debug().Msg("PTT ON from CAT client")
					toK4 = append(toK4, cmd...)
				case "TX/;":
					// Toggle PTT
					p.pttActive.Store(!p.pttActive.Load())
					toK4 = append(toK4, cmd...)
				case "RX;":
					p.pttActive.Store(false)
					log.Debug().Msg("PTT OFF from CAT client")
					toK4 = append(toK4, cmd...)
				case "TQ;":
					// Answer locally from our PTT gate state.
					// The K4 reports TQ0; until the audio stream keys it (which takes
					// one full frame after TX;), causing hamlib to think PTT failed.
					resp := "TQ0;"
					if p.pttActive.Load() {
						resp = "TQ1;"
					}
					select {
					case sendCh <- []byte(resp):
					default:
						log.Debug().Str("client", remote).Msg("TQ response dropped: send buffer full")
					}
				default:
					toK4 = append(toK4, cmd...)
				}
			}

			if len(toK4) > 0 {
				p.mu.RLock()
				k4conn := p.conn
				p.mu.RUnlock()

				if k4conn != nil {
					frame := k4.BuildFrame(k4.BuildCATPayload(string(toK4)))
					select {
					case k4conn.WriteCh <- frame:
					case <-ctx.Done():
						return
					default:
						log.Warn().Msg("K4 write buffer full, dropping CAT command")
					}
				} else {
					log.Debug().Msg("CAT commands received but K4 not connected, dropping")
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				log.Debug().Err(err).Str("client", remote).Msg("CAT client read error")
			}
			return
		}
	}
}
