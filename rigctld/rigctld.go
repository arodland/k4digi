package rigctld

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	log "github.com/rs/zerolog/log"
)

// Run supervises a rigctld child process, restarting it if it exits unexpectedly.
// On ctx cancellation it sends SIGINT and waits up to 10 s before SIGKILL.
func Run(ctx context.Context, port int, model int, catPort int) {
	args := []string{
		"-t", fmt.Sprintf("%d", port),
		"-m", fmt.Sprintf("%d", model),
		"-r", fmt.Sprintf("localhost:%d", catPort),
	}

	for ctx.Err() == nil {
		cmd := exec.Command("rigctld", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Start(); err != nil {
			log.Warn().Err(err).Msg("Failed to start rigctld, retrying in 2s")
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}
		log.Info().Int("pid", cmd.Process.Pid).Strs("args", args).Msg("rigctld started")

		// Wait for exit in a goroutine so we can also select on ctx.
		exitCh := make(chan error, 1)
		go func() { exitCh <- cmd.Wait() }()

		select {
		case err := <-exitCh:
			if ctx.Err() != nil {
				return
			}
			log.Warn().Err(err).Msg("rigctld exited unexpectedly, restarting in 2s")
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return
			}

		case <-ctx.Done():
			cmd.Process.Signal(os.Interrupt)
			select {
			case <-exitCh:
			case <-time.After(10 * time.Second):
				log.Warn().Int("pid", cmd.Process.Pid).Msg("rigctld did not exit after SIGINT, sending SIGKILL")
				cmd.Process.Kill()
				<-exitCh
			}
			return
		}
	}
}
