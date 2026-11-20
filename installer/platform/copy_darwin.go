package platform

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

func copyNative(ctx context.Context, from, to string, _ *os.File, _ unix.Stat_t) error {
	output, err := exec.CommandContext(ctx, "/bin/cp", "-p", from, to).CombinedOutput()
	if err != nil {
		return fmt.Errorf("preserve profile metadata: %w: %s", err, output)
	}
	return nil
}
