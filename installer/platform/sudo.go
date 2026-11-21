package platform

import (
	"context"
	"os/exec"
	"time"
)

// KeepSudoAlive runs the foreground authentication one time. Then it
// refreshes the sudo timestamp with `sudo -n -v` each minute until release, so
// a long approved run does not stop on an expired credential. The refresh
// never asks for a password.
func KeepSudoAlive(ctx context.Context, program string, authenticate func(context.Context) error) (func() error, error) {
	return keepSudoAlive(ctx, program, authenticate, time.Minute)
}

func keepSudoAlive(ctx context.Context, program string, authenticate func(context.Context) error, interval time.Duration) (func() error, error) {
	if err := authenticate(ctx); err != nil {
		return nil, err
	}
	refresh, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-refresh.Done():
				return
			case <-ticker.C:
			}
			output, err := exec.CommandContext(refresh, program, "-n", "-v").CombinedOutput()
			if err != nil && refresh.Err() == nil {
				Detail(ctx, "Administrator credential refresh stopped (%v%s); a later privileged step asks again if needed.", err, Diagnostic(output))
				return
			}
		}
	}()
	return func() error {
		cancel()
		<-stopped
		return nil
	}, nil
}
