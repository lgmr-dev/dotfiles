package platform

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const exactAttributes = true

func copyNative(ctx context.Context, from, _ string, output *os.File, original unix.Stat_t, limit int64) (result error) {
	input, err := openSame(from, original)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, input.Close()) }()
	sourceInfo, err := input.Stat()
	if err != nil {
		return err
	}
	var target unix.Stat_t
	if err := unix.Fstat(int(output.Fd()), &target); err != nil {
		return err
	}
	if original.Uid != target.Uid || original.Gid != target.Gid {
		if err := output.Chown(int(original.Uid), int(original.Gid)); err != nil {
			return err
		}
	}
	if _, err := io.Copy(output, io.LimitReader(input, limit+1)); err != nil {
		return err
	}
	info, err := output.Stat()
	if err != nil {
		return err
	}
	if info.Size() > limit {
		return errors.New("profile grew beyond its size bound")
	}
	if err := copyAttributes(int(input.Fd()), int(output.Fd())); err != nil {
		return err
	}
	if err := output.Chmod(sourceInfo.Mode()); err != nil {
		return err
	}
	return ctx.Err()
}

func userFlags(unix.Stat_t) uint32 { return 0 }

func setFlags(int, unix.Stat_t) error { return nil }
