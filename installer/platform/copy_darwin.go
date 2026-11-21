package platform

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

const exactAttributes = false

const userFlagMask = unix.UF_NODUMP | unix.UF_IMMUTABLE | unix.UF_APPEND | unix.UF_OPAQUE | unix.UF_HIDDEN

func copyNative(ctx context.Context, from, to string, _ *os.File, _ unix.Stat_t, _ int64) error {
	output, err := exec.CommandContext(ctx, "/bin/cp", "-p", from, to).CombinedOutput()
	if err != nil {
		return fmt.Errorf("preserve profile metadata: %w: %s", err, output)
	}
	return nil
}

func userFlags(info unix.Stat_t) uint32 { return info.Flags & userFlagMask }

func setFlags(fd int, original unix.Stat_t) error {
	if flags := userFlags(original); flags != 0 {
		return unix.Fchflags(fd, int(flags))
	}
	return nil
}
