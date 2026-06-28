# K4Digi

K4Digi bridges an **Elecraft K4** transceiver's TCP network interface to the
local Linux audio and CAT stack, enabling data-mode operation (WSJT-X FT8,
JS8Call, Fldigi, etc.) over the LAN without any USB or audio cables.

It is similar in purpose to [kappanhang](https://github.com/nonoo/kappanhang)
for Icom radios — same goal, same general approach, different radio protocol.

## What it provides

- **PulseAudio virtual sources `K4-RX-A` and `K4-RX-B`** — 12 kHz mono S16LE
  PCM, one source per VFO (A = main, B = sub). Point WSJT-X or any other audio
  application at `K4-RX-A` for the main VFO. Pass `-stereo` (or set
  `audio.stereo: true`) to use a single stereo source `K4-RX` instead, with
  left = VFO A and right = VFO B.
- **PulseAudio virtual sink `K4-TX`** — 12 kHz stereo S16LE PCM. Audio
  written here is transmitted when PTT is active. TX is **gated on PTT state**:
  audio arriving while in RX is silently discarded so the K4's VOX cannot
  trigger accidental transmissions.
- **CAT TCP proxy on port 9299** — compatible with hamlib/rigctld and WSJT-X.
  Multiple clients can connect simultaneously; CAT responses from the K4 are
  fanned out to all of them. `TQ;` (PTT query) is answered locally so hamlib
  sees PTT transitions immediately.
- **rigctld supervision** on port 4532 — as an alternative to the CAT TCP proxy
  for apps that prefer rigctld mode, or older apps that don't know how to do
  non-rigctld CAT over TCP.

K4Digi reconnects to the K4 automatically if the TCP connection drops, without
tearing down the PulseAudio devices or the CAT proxy listener.

## Building

```
go build ./...
```

Requires Go 1.25+. No CGo. PulseAudio or PipeWire (with PulseAudio
compatibility) must be running for audio support. A recent version of hamlib
rigctld must be available in `$PATH` for rigctld support.

## Running

```
k4digi -host <K4-hostname-or-ip>
```

The passphrase can be supplied on the command line or in the config file (see
below). A config file is the recommended approach so the passphrase does not
appear in the process list.

## Configuration

K4Digi uses a YAML config file at `~/.config/k4digi/config.yaml` (XDG) and
command-line flags. Flags override the config file. All settings are optional
except `k4.host` and `k4.passphrase`.

### Minimal config file

```yaml
k4:
  host: K4-SN01234.lan
  passphrase: YourPassphrase
```

### Full config file with defaults shown

```yaml
log_level: info          # debug / info / warn / error

k4:
  host: ""               # REQUIRED — K4 hostname or IP; port defaults to 9205
  passphrase: ""         # REQUIRED — K4 network passphrase
  sl: 0                  # Streaming latency tier: 0=20 ms … 7=1200 ms

audio:
  enabled: true          # Set false to disable PulseAudio devices entirely
  stereo: false          # true: single stereo source K4-RX (L=VFO A, R=VFO B)
                         # false (default): two mono sources K4-RX-A and K4-RX-B
  rx_a_name: K4-RX-A    # VFO A source name (split/mono mode)
  rx_b_name: K4-RX-B    # VFO B source name (split/mono mode)
  rx_name: K4-RX         # Source name in stereo mode
  tx_name: K4-TX         # Name of the PulseAudio sink created for transmit audio
  consume: auto          # Self-consume RX source to prevent PipeWire latency
                         # bursts on first connect: true / false / auto
                         # auto enables this only when PipeWire is detected

cat:
  port: 9299             # TCP port for the CAT proxy

rigctld:
  enabled: false         # Supervise a rigctld child process (see below)
  port: 4532             # rigctld listen port (-t)
  model: 2047            # hamlib rig model number (-m)

tgxl:
  enabled: false         # Enable TunerGenius XL integration (see below)
  host: tunergenius.lan:9010
```

### Command-line flags

Every config file key has a corresponding flag. The most commonly needed ones:

| Flag | Config key | Description |
|---|---|---|
| `-host` | `k4.host` | K4 hostname or IP |
| `-passphrase` | `k4.passphrase` | K4 network passphrase |
| `-sl` | `k4.sl` | Streaming latency tier (0–7) |
| `-cat-port` | `cat.port` | CAT proxy TCP port |
| `-rigctld` | `rigctld.enabled` | Enable rigctld supervisor |
| `-rigctld-port` | `rigctld.port` | rigctld listen port |
| `-rigctld-model` | `rigctld.model` | hamlib rig model number |
| `-stereo` | `audio.stereo` | Use one stereo source instead of two mono sources |
| `-rx-a-name` | `audio.rx_a_name` | VFO A source name (split mode) |
| `-rx-b-name` | `audio.rx_b_name` | VFO B source name (split mode) |
| `-rx-name` | `audio.rx_name` | Source name (stereo mode) |
| `-tx-name` | `audio.tx_name` | PulseAudio sink name |
| `-pulseaudio` | `audio.enabled` | Enable/disable audio devices |
| `-log-level` | `log_level` | Logging verbosity |
| `-config-file` | — | Path to YAML config file |

## Optional features

### rigctld supervisor

With `-rigctld` (or `rigctld.enabled: true`), K4Digi supervises a `rigctld`
child process pointed at the local CAT proxy, restarting it automatically if it
exits. The process is started after the first successful K4 authentication and
is shut down gracefully (SIGINT, then SIGKILL after 10 s) when K4Digi exits.

```
rigctld -t <rigctld.port> -m <rigctld.model> -r localhost:<cat.port>
```

This lets hamlib clients connect to `localhost:4532` without needing a separate
service file.

### TunerGenius XL integration

With `-tgxl` (or `tgxl.enabled: true`), K4Digi connects to a
[TunerGenius XL](http://www.tunergenius.com/) automatic antenna tuner in TCP
mode. This allows you to use the TUNE button on the TGXL (or in the TunerGenius
app) for one-press tuning, by engaging the TUNE LP feature of the K4 when the
TGXL is in tuning mode.  It will also enable the K4's internal ATU (if present)
when the TGXL is in BYPASS, and bypass the KAT4 when the TGXL is active.

It is still strongly recommended to have an RS-232 cable connected between the
TGXL and the K4 to synchronize the current frequency over CAT.

## Credits

The K4 TCP protocol implementation is derived from
[QK4](https://github.com/mikeg-dal/QK4) by Mike Garcia. The binary framing,
authentication (SHA-384 hex), init sequence, and audio payload format are all
reproduced from that work.
