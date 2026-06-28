package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"strings"
	"syscall"
	"time"

	"github.com/arodland/k4digi/audio"
	"github.com/arodland/k4digi/cat"
	"github.com/arodland/k4digi/k4"
	pulsedev "github.com/arodland/k4digi/pulse"
	"github.com/arodland/k4digi/rigctld"
	"github.com/arodland/k4digi/tgxl"
	"github.com/adrg/xdg"
	"github.com/jfreymuth/pulse"
	"github.com/rs/zerolog"
	log "github.com/rs/zerolog/log"
	"github.com/vimeo/dials/ez"
	dialsflag "github.com/vimeo/dials/sources/flag"
)

type K4Config struct {
	Host       string `dialsflag:"host" yaml:"host" dialsdesc:"K4 host:port (default port 9205)"`
	Passphrase string `dialsflag:"passphrase" yaml:"passphrase" dialsdesc:"K4 auth passphrase"`
	SLTier     int    `dialsflag:"sl" yaml:"sl" dialsdesc:"Streaming latency tier 0-7 (0=20ms, 1=40ms, …)"`
}

type AudioConfig struct {
	Enabled bool   `dialsflag:"pulseaudio" yaml:"enabled" dialsdesc:"Enable PulseAudio audio devices"`
	Stereo  bool   `dialsflag:"stereo" yaml:"stereo" dialsdesc:"Use a single stereo source (left=VFO A, right=VFO B) instead of two mono sources"`
	RXName  string `dialsflag:"rx-name" yaml:"rx_name" dialsdesc:"PulseAudio source name in stereo mode"`
	RXAName string `dialsflag:"rx-a-name" yaml:"rx_a_name" dialsdesc:"PulseAudio source name for VFO A (split mode)"`
	RXBName string `dialsflag:"rx-b-name" yaml:"rx_b_name" dialsdesc:"PulseAudio source name for VFO B (split mode)"`
	TXName  string `dialsflag:"tx-name" yaml:"tx_name" dialsdesc:"PulseAudio sink name for K4 transmit audio"`
	Consume string `dialsflag:"consume" yaml:"consume" dialsdesc:"Self-consume RX source to prevent PipeWire latency bursts (true/false/auto)"`
}

type CATConfig struct {
	Port int `dialsflag:"cat-port" yaml:"port" dialsdesc:"Local CAT TCP server port"`
}

type TGXLConfig struct {
	Enabled bool   `dialsflag:"tgxl" yaml:"enabled" dialsdesc:"Enable TunerGenius XL integration"`
	Host    string `dialsflag:"tgxl-host" yaml:"host" dialsdesc:"TunerGenius XL host:port"`
}

type RigctldConfig struct {
	Enabled bool   `dialsflag:"rigctld" yaml:"enabled" dialsdesc:"Supervise a rigctld child process"`
	Port    int    `dialsflag:"rigctld-port" yaml:"port" dialsdesc:"rigctld TCP port"`
	Model   int    `dialsflag:"rigctld-model" yaml:"model" dialsdesc:"rigctld rig model number"`
}

type Config struct {
	ConfigFile string        `dialsdesc:"Config file path"`
	LogLevel   string        `dialsflag:"log-level" yaml:"log_level" dialsdesc:"Log level: debug/info/warn/error"`
	K4         K4Config      `yaml:"k4"`
	Audio      AudioConfig   `yaml:"audio"`
	CAT        CATConfig     `yaml:"cat"`
	TGXL       TGXLConfig    `yaml:"tgxl"`
	Rigctld    RigctldConfig `yaml:"rigctld"`
}

func (c *Config) ConfigPath() (string, bool) {
	return c.ConfigFile, c.ConfigFile != ""
}

func defaultConfig() *Config {
	xdgConfig, _ := xdg.ConfigFile("k4digi/config.yaml")
	return &Config{
		ConfigFile: xdgConfig,
		LogLevel:   "info",
		K4: K4Config{
			SLTier: 0,
		},
		Audio: AudioConfig{
			Enabled: true,
			Stereo:  false,
			RXName:  "K4-RX",
			RXAName: "K4-RX-A",
			RXBName: "K4-RX-B",
			TXName:  "K4-TX",
			Consume: "auto",
		},
		CAT: CATConfig{
			Port: 9299,
		},
		TGXL: TGXLConfig{
			Host: "tunergenius.lan:9010",
		},
		Rigctld: RigctldConfig{
			Port:  4532,
			Model: 2047,
		},
	}
}

func main() {
	log.Logger = zerolog.New(
		zerolog.ConsoleWriter{Out: os.Stderr},
	).With().Timestamp().Logger()

	d, err := ez.YAMLConfigEnvFlag(context.Background(), defaultConfig(), ez.Params[Config]{
		FlagConfig: dialsflag.DefaultFlagNameConfig(),
	})
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load config")
	}
	cfg := d.View()

	logLevel, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		log.Fatal().Str("level", cfg.LogLevel).Msg("Unknown log level")
	}
	zerolog.SetGlobalLevel(logLevel)

	if cfg.K4.Host == "" {
		log.Fatal().Msg("-host is required (K4 hostname or IP)")
	}
	host := cfg.K4.Host
	if !strings.Contains(host, ":") {
		host += ":9205"
	}
	if cfg.K4.SLTier < 0 || cfg.K4.SLTier > 7 {
		log.Fatal().Msg("-sl must be between 0 and 7")
	}
	switch cfg.Audio.Consume {
	case "true", "false", "auto":
	default:
		log.Fatal().Msg("-consume must be 'true', 'false', or 'auto'")
	}

	// PulseAudio devices are created once and persist across K4 reconnects.
	var rxSrc *pulsedev.PipeSource    // stereo mode
	var rxSrcA *pulsedev.PipeSource   // split mode, VFO A
	var rxSrcB *pulsedev.PipeSource   // split mode, VFO B
	var txSink *pulsedev.PipeSink
	if cfg.Audio.Enabled {
		var pc *pulse.Client
		pc, err = pulse.NewClient(pulse.ClientApplicationName("k4digi"))
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to connect to PulseAudio")
		}

		var sourceNames []string
		if cfg.Audio.Stereo {
			sourceNames = []string{cfg.Audio.RXName}
		} else {
			sourceNames = []string{cfg.Audio.RXAName, cfg.Audio.RXBName}
		}
		if err = pulsedev.CheckConflicts(pc, sourceNames, cfg.Audio.TXName); err != nil {
			log.Fatal().Err(err).Send()
		}

		shouldConsume := cfg.Audio.Consume == "true"
		if cfg.Audio.Consume == "auto" {
			isPW, pwErr := pulsedev.IsPipeWire(pc)
			if pwErr != nil {
				log.Warn().Err(pwErr).Msg("Could not detect PA server type; skipping self-consumer")
			} else {
				shouldConsume = isPW
			}
		}

		if cfg.Audio.Stereo {
			rxSrc, err = pulsedev.CreatePipeSource(pc, cfg.Audio.RXName, "K4 RX", k4.SampleRate, k4.RXChannels)
			if err != nil {
				log.Fatal().Err(err).Msg("Failed to create RX PulseAudio source")
			}
			defer rxSrc.Close(pc)
			if shouldConsume {
				consumer, err := rxSrc.Consume(pc, k4.SampleRate, k4.RXChannels)
				if err != nil {
					log.Fatal().Err(err).Msg("Failed to create RX self-consumer")
				}
				defer consumer.Close()
			}
			log.Info().Str("rx_source", cfg.Audio.RXName).Msg("PulseAudio RX source created (stereo)")
		} else {
			rxSrcA, err = pulsedev.CreatePipeSource(pc, cfg.Audio.RXAName, "K4 RX A", k4.SampleRate, 1)
			if err != nil {
				log.Fatal().Err(err).Msg("Failed to create RX-A PulseAudio source")
			}
			defer rxSrcA.Close(pc)
			rxSrcB, err = pulsedev.CreatePipeSource(pc, cfg.Audio.RXBName, "K4 RX B", k4.SampleRate, 1)
			if err != nil {
				log.Fatal().Err(err).Msg("Failed to create RX-B PulseAudio source")
			}
			defer rxSrcB.Close(pc)
			if shouldConsume {
				consumerA, err := rxSrcA.Consume(pc, k4.SampleRate, 1)
				if err != nil {
					log.Fatal().Err(err).Msg("Failed to create RX-A self-consumer")
				}
				defer consumerA.Close()
				consumerB, err := rxSrcB.Consume(pc, k4.SampleRate, 1)
				if err != nil {
					log.Fatal().Err(err).Msg("Failed to create RX-B self-consumer")
				}
				defer consumerB.Close()
			}
			log.Info().
				Str("rx_source_a", cfg.Audio.RXAName).
				Str("rx_source_b", cfg.Audio.RXBName).
				Msg("PulseAudio RX sources created (split)")
		}
		if shouldConsume {
			log.Info().Msg("RX source self-consumer started (PipeWire latency workaround)")
		}

		txSink, err = pulsedev.CreatePipeSink(pc, cfg.Audio.TXName, "K4 TX", k4.SampleRate, k4.TXChannels)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to create TX PulseAudio sink")
		}
		defer txSink.Close(pc)

		log.Info().Str("tx_sink", cfg.Audio.TXName).Int("sample_rate", k4.SampleRate).Msg("PulseAudio TX sink created")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Info().Msg("Shutting down")
		cancel()
	}()

	// CAT proxy: listener binds once, stays up across K4 reconnects.
	proxy := cat.NewProxy(fmt.Sprintf(":%d", cfg.CAT.Port))
	go func() {
		if err := proxy.Serve(ctx); err != nil {
			log.Fatal().Err(err).Msg("CAT proxy failed to start")
		}
	}()

	if cfg.TGXL.Enabled {
		tgxlHost := cfg.TGXL.Host
		if !strings.Contains(tgxlHost, ":") {
			tgxlHost += ":9010"
		}
		go tgxl.Run(ctx, tgxlHost, proxy.SendCAT)
	}

	var startRigctld sync.Once

	// K4 connection retry loop.
	for ctx.Err() == nil {
		conn, err := k4.Dial(ctx, host, cfg.K4.Passphrase, cfg.K4.SLTier)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error().Err(err).Msg("Failed to connect to K4, retrying in 5s")
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}

		if cfg.Rigctld.Enabled {
			startRigctld.Do(func() {
				go rigctld.Run(ctx, cfg.Rigctld.Port, cfg.Rigctld.Model, cfg.CAT.Port)
			})
		}

		proxy.SetConn(conn)

		// Per-connection context so we can stop loops when this conn dies.
		connCtx, connCancel := context.WithCancel(ctx)

		var wg sync.WaitGroup

		if rxSrc != nil {
			wg.Go(func() {
				audio.RXLoop(connCtx, conn.RXCh, rxSrc.Handle)
			})
		} else if rxSrcA != nil {
			wg.Go(func() {
				audio.RXLoopSplit(connCtx, conn.RXCh, rxSrcA.Handle, rxSrcB.Handle)
			})
		}

		if txSink != nil {
			wg.Go(func() {
				audio.TXLoop(connCtx, txSink.Handle, conn.WriteCh, cfg.K4.SLTier, proxy.IsPTTActive)
			})
		}

		// Relay K4 CAT responses to all proxy clients.
		wg.Go(func() {
			for {
				select {
				case payload, ok := <-conn.CATCh:
					if !ok {
						return
					}
					proxy.BroadcastFromK4(payload)
				case <-connCtx.Done():
					return
				}
			}
		})

		// Run blocks until the TCP connection dies or ctx is cancelled.
		if err := conn.Run(connCtx); err != nil && ctx.Err() == nil {
			log.Error().Err(err).Msg("K4 connection lost")
		}

		connCancel()
		proxy.SetConn(nil)
		proxy.ClearPTT() // reset PTT on disconnect so reconnect starts in RX
		wg.Wait()

		if ctx.Err() != nil {
			return
		}

		log.Info().Msg("Reconnecting in 5s")
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}
