# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project overview

K4Digi bridges an Elecraft K4 transceiver's TCP network interface to the local Linux audio and CAT stack, enabling data-mode operation (WSJT-X FT8, JS8Call, etc.) over the LAN. It exposes:
- Two PulseAudio virtual sources `K4-RX-A` and `K4-RX-B` (12 kHz mono S16LE; A = VFO A, B = VFO B) — default
- Optionally a single stereo source `K4-RX` (12 kHz stereo S16LE; left = VFO A, right = VFO B) via `-stereo`
- A PulseAudio virtual sink `K4-TX` (12 kHz stereo S16LE)
- A CAT TCP proxy on port 9299 (compatible with WSJT-X / hamlib / rigctld clients)

## Commands

```
go build ./...          # build
go run . -host <ip>     # run directly
go test ./...           # test
go vet ./...            # vet
```

Typical invocation:
```
./k4digi -host 192.168.1.x
```
Config file: `~/.config/k4digi/config.yaml` (XDG). All options can also be set as flags. Mandatory: `k4.host`, `k4.passphrase`. See README for the full flag list.

## Architecture

### Configuration (`main.go`)
Uses `github.com/vimeo/dials` with `ez.YAMLConfigEnvFlag`. Config is a nested struct hierarchy:
- `K4Config` — `host`, `passphrase`, `sl` (latency tier)
- `AudioConfig` — `enabled`, `stereo`, `rx_name`, `rx_a_name`, `rx_b_name`, `tx_name`, `consume`
- `CATConfig` — `port`
- `TGXLConfig` — `enabled`, `host`
- `RigctldConfig` — `enabled`, `port`, `model`

All struct fields carry `dialsflag:"<name>"` tags with explicit compound names to avoid dials' automatic per-letter kebab splitting (e.g. `CATPort` → `c-a-t-port`). The `ConfigPath()` method on `*Config` tells dials where to load the optional YAML file; the default path comes from `xdg.ConfigFile("k4digi/config.yaml")`.

### K4 TCP protocol (`k4/`)
Binary framing: `[0xFEFDFCFB][u32 BE length][payload][0xFBFCFDFE]`. Auth is SHA-384(passphrase) as lowercase hex sent raw (no framing) before the first frame. Init sequence after auth: `RDY; K4; K2; EM1; SL<n>;` — `EM1` selects S16LE PCM (avoiding Opus/CGo), `SL` sets the latency tier (0=20 ms → 7=1200 ms).

Payload types: `0x00` CAT, `0x01` audio (RX and TX), `0x02`/`0x03` PAN/MiniPAN (ignored). Audio payloads carry a 7-byte header (`k4.AudioHeaderSize`) before PCM data; the sample rate field is always 0x00 (meaning 12 kHz).

`k4.Dial(ctx, addr, passphrase, slTier)` connects and authenticates, returning a `*Conn`. `k4.Conn` exposes three channels: `WriteCh` (frames to send), `RXCh` (audio payloads received), `CATCh` (CAT payloads received). `Run()` blocks until the connection dies.

### PulseAudio integration (`pulse/`)
Uses `module-pipe-source` and `module-pipe-sink`. The pipes live at `/tmp/k4digi-<name>.pipe`. A `k4digi.pid` property is stamped on each module so `CheckConflicts(pc, sourceNames []string, sinkName string)` can evict stale modules left by dead processes without touching live ones.

PipeWire self-consumer workaround: when no real consumer is attached to a `module-pipe-source`, PipeWire lets the pipe buffer fill, causing a latency burst on first connect. The `consume: auto` setting (default) detects PipeWire via `GetServerInfo` and starts a silent `io.Discard` record stream to keep the pipe drained. In split mode, a self-consumer is started for each of the two mono sources.

### Audio loops (`audio/`)
- `RXLoop(ctx, rxCh, pipe)`: stereo mode — reads from `RXCh`, strips the 7-byte header, boosts gain by 16× (≈ +24 dB, compensating for EM1's -35 dBFS output), and writes stereo PCM to a single PulseAudio pipe.
- `RXLoopSplit(ctx, rxCh, pipeA, pipeB)`: split mode (default) — same receive/gain path, then deinterleaves the S16LE stereo stream into two mono buffers written to separate pipes (pipeA = VFO A / left channel, pipeB = VFO B / right channel).
- Both RX functions use `startPipeWriter` to run pipe writes in a separate goroutine so a stalled PulseAudio consumer never blocks context cancellation.
- `TXLoop`: reads from the PulseAudio sink pipe into a ring buffer, assembles frames of exactly `SLFrameSizes[slTier]` samples, and enqueues them to `WriteCh`. Audio is **gated on PTT state** — when PTT is inactive, the ring buffer is drained without sending (prevents the K4's audio-level VOX from triggering TX).

### CAT proxy (`cat/`)
`Proxy` is a multi-client TCP server that fans K4 CAT responses to all connected clients and forwards client commands to the K4. The listener and client connections survive K4 reconnects (`SetConn(nil)` / `SetConn(conn)`).

PTT is tracked with an `atomic.Bool`: `TX;` sets it, `RX;` clears it. `TQ;` (PTT query) is intercepted and answered locally from the atomic because the K4 only acknowledges TX after receiving a full audio frame, which would make hamlib see PTT as failed. `PING`/`PONG` keepalives are filtered out before broadcast — external clients (WSJT-X) misbehave if they receive them.

`proxy.SendCAT(ctx, cmd)` is used by internal components (TGXL, future subsystems) to send CAT commands without a TCP client connection.

### TunerGenius XL integration (`tgxl/`)
`tgxl.Run(ctx, addr, sendCAT)` runs a persistent reconnect loop (5 s backoff) connecting to a TunerGenius XL in TCP mode using the flexclient protocol. Watches the `state` object: `tuning=1/0` sends `TU2;`/`TU0;` to the K4; `bypass=1/0` sends `AT2;`/`AT1;`. Uses `client.Close()` for cleanup — `Unsubscribe` must not be called after `Close` (panics).

### rigctld supervisor (`rigctld/`)
`rigctld.Run(ctx, port, model, catPort)` supervises a `rigctld` child process pointed at the local CAT proxy, restarting it on unexpected exit (2 s backoff). Started after the first successful K4 authentication (via `sync.Once` in the connection loop). Shutdown sends SIGINT, then SIGKILL after 10 s. Uses `exec.Command` rather than `exec.CommandContext` to control the shutdown sequence manually.

### Main loop (`main.go`)
PulseAudio devices are created once at startup and persist across K4 reconnects. The CAT proxy listener also binds once. The K4 connection retry loop runs forever (5 s backoff) until `SIGINT`/`SIGTERM`. Per-connection goroutines (RX audio, TX audio, CAT relay) are started with `wg.Go()` and waited on after each disconnect.

### Dependencies
- `github.com/jfreymuth/pulse` — PulseAudio native protocol client (no cgo)
- `github.com/rs/zerolog` — structured logging
- `github.com/smallnest/ringbuffer` — lock-free ring buffer for TX audio
- `github.com/vimeo/dials` — config from flags + YAML
- `github.com/adrg/xdg` — XDG base directory for default config path
- `github.com/kc2g-flex-tools/flexclient` — FlexRadio protocol client (used for TGXL)
