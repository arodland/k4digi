package tgxl

import (
	"context"
	"fmt"
	"time"

	"github.com/kc2g-flex-tools/flexclient"
	log "github.com/rs/zerolog/log"
)

// Run connects to the TunerGenius XL at addr and watches for tuning state changes,
// forwarding TU2; (tune) and TU0; (stop) to the K4 via sendCAT. It retries silently
// on failure with a 5 s back-off, and runs until ctx is cancelled.
func Run(ctx context.Context, addr string, sendCAT func(context.Context, string)) {
	for ctx.Err() == nil {
		err := connect(ctx, addr, sendCAT)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
			}
		}
	}
}

func connect(ctx context.Context, addr string, sendCAT func(context.Context, string)) error {
	client, err := flexclient.NewFlexClient(addr)
	if err != nil {
		return err
	}

	log.Info().Str("addr", addr).Msg("Connected to TunerGenius XL")

	// connCtx lets us close the client on return regardless of whether ctx fired.
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	updates := make(chan flexclient.StateUpdate, 10)
	client.Subscribe(flexclient.Subscription{
		Prefix:  "state",
		Updates: updates,
	})

	// Close the client when our context (or parent ctx) is done.
	// connCancel() (via defer) triggers this goroutine on return.
	go func() {
		<-connCtx.Done()
		client.Close()
	}()

	go client.Run()

	tuning := false
	for update := range updates {
		if val, ok := update.Updated["tuning"]; ok {
			if val == "1" && !tuning {
				tuning = true
				sendCAT(ctx, "TU2;")
			} else if val == "0" && tuning {
				tuning = false
				sendCAT(ctx, "TU0;")
			}
		}
		if val, ok := update.Updated["bypass"]; ok {
			if val == "1" {
				sendCAT(ctx, "AT2;")
			} else if val == "0" {
				sendCAT(ctx, "AT1;")
			}
		}
	}

	// Channel closed: connection lost.
	if tuning {
		sendCAT(ctx, "TU0;")
	}

	if ctx.Err() != nil {
		return nil
	}

	log.Info().Str("addr", addr).Msg("Lost connection to TunerGenius XL")
	return fmt.Errorf("connection lost")
}
