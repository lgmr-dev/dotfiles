//go:build darwin || linux

package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// CopyWithMetadata copies a regular file of at most MaxProfileBytes to a new
// path with its owner, group, mode, ACL and extended attributes. It reserves
// the destination exclusively and never replaces a file. On a failure it
// removes only the file that it made.
func CopyWithMetadata(ctx context.Context, from, to string) error {
	return copyFileWithMetadata(ctx, from, to, MaxProfileBytes)
}

func copyFileWithMetadata(ctx context.Context, from, to string, limit int64) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	var original unix.Stat_t
	if err := unix.Lstat(from, &original); err != nil {
		return err
	}
	if original.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("profile source is not a regular file")
	}
	output, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	created, statErr := output.Stat()
	if statErr != nil {
		return errors.Join(statErr, output.Close())
	}
	defer func() {
		result = errors.Join(result, output.Close())
		if result != nil {
			result = errors.Join(result, removeFailedCopy(to, created))
		}
	}()
	if err := copyNative(ctx, from, to, output, original, limit); err != nil {
		return err
	}
	current, err := os.Lstat(to)
	if err != nil || !os.SameFile(created, current) {
		return errors.Join(errors.New("profile staging identity changed"), err)
	}
	input, err := openSame(from, original)
	if err != nil {
		return err
	}
	if err := errors.Join(verifyMetadata(from, input, output), input.Close()); err != nil {
		return err
	}
	return errors.Join(ctx.Err(), output.Sync())
}

func copyFolderMetadata(ctx context.Context, from, to string, info os.FileInfo, children func() error) (result error) {
	if err := children(); err != nil {
		return err
	}
	var original unix.Stat_t
	if err := unix.Lstat(from, &original); err != nil {
		return err
	}
	input, err := openSame(from, original)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, input.Close()) }()
	opened, err := input.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.Join(errors.New("folder changed during the copy"), err)
	}
	output, err := os.Open(to)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, output.Close()) }()
	var target unix.Stat_t
	if err := unix.Fstat(int(output.Fd()), &target); err != nil {
		return err
	}
	if original.Uid != target.Uid || original.Gid != target.Gid {
		if err := output.Chown(int(original.Uid), int(original.Gid)); err != nil {
			return err
		}
	}
	if err := copyAttributes(int(input.Fd()), int(output.Fd())); err != nil {
		return err
	}
	if err := output.Chmod(info.Mode()); err != nil {
		return err
	}
	if err := setFlags(int(output.Fd()), original); err != nil {
		return err
	}
	if err := verifyMetadata(from, input, output); err != nil {
		return err
	}
	return ctx.Err()
}

func openSame(path string, original unix.Stat_t) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var opened unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &opened); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if opened.Dev != original.Dev || opened.Ino != original.Ino {
		return nil, errors.Join(fmt.Errorf("%s changed during the copy", path), file.Close())
	}
	return file, nil
}

func copyAttributes(from, to int) error {
	original, err := fileAttributes(from)
	if err != nil {
		return err
	}
	if exactAttributes {
		inherited, err := fileAttributes(to)
		if err != nil {
			return err
		}
		for name := range inherited {
			if _, exists := original[name]; !exists {
				if err := unix.Fremovexattr(to, name); err != nil {
					return err
				}
			}
		}
	}
	for name, value := range original {
		if err := unix.Fsetxattr(to, name, value, 0); err != nil {
			return err
		}
	}
	return nil
}

func verifyMetadata(from string, input, output *os.File) error {
	var original, copied unix.Stat_t
	if err := errors.Join(unix.Fstat(int(input.Fd()), &original), unix.Fstat(int(output.Fd()), &copied)); err != nil {
		return err
	}
	if original.Uid != copied.Uid || original.Gid != copied.Gid {
		return fmt.Errorf("cannot preserve profile owner/group for %s; original profile is unchanged", from)
	}
	if userFlags(original) != userFlags(copied) {
		return fmt.Errorf("cannot preserve the file flags of %s; original profile is unchanged", from)
	}
	want, err := fileAttributes(int(input.Fd()))
	if err != nil {
		return err
	}
	got, err := fileAttributes(int(output.Fd()))
	if err != nil {
		return err
	}
	if exactAttributes && len(got) != len(want) {
		return errors.New("staged profile attributes differ from the original")
	}
	for name, value := range want {
		if actual, exists := got[name]; !exists || !bytes.Equal(actual, value) {
			return errors.New("staged profile attribute differs: " + name)
		}
	}
	return nil
}

func fileAttributes(fd int) (map[string][]byte, error) {
	result := map[string][]byte{}
	size, err := unix.Flistxattr(fd, nil)
	if errors.Is(err, unix.ENOTSUP) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	if size > 1<<20 {
		return nil, errors.New("profile attribute list exceeds its bound")
	}
	names := make([]byte, size)
	if size > 0 {
		count, err := unix.Flistxattr(fd, names)
		if err != nil {
			return nil, err
		}
		if count > len(names) {
			return nil, errors.New("profile attribute list changed during inspection")
		}
		names = names[:count]
	}
	for _, name := range strings.Split(string(names), "\x00") {
		if name == "" {
			continue
		}
		size, err := unix.Fgetxattr(fd, name, nil)
		if err != nil {
			return nil, err
		}
		if size > 1<<20 {
			return nil, errors.New("profile attribute exceeds its bound")
		}
		value := make([]byte, size)
		count, err := unix.Fgetxattr(fd, name, value)
		if err != nil {
			return nil, err
		}
		if count > len(value) {
			return nil, errors.New("profile attribute changed during inspection")
		}
		result[name] = value[:count]
	}
	return result, nil
}
