//go:build darwin || linux

package platform

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// DirectoryIdentity returns the volume and file identity of an open folder.
func DirectoryIdentity(file *os.File) (string, error) {
	var info unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", info.Dev, info.Ino), nil
}

// FileOwner returns the user, the group and the mode bits of path without
// following a link. It fails on Windows, which has no POSIX ownership.
func FileOwner(path string) (uint32, uint32, uint32, error) {
	var info unix.Stat_t
	if err := unix.Lstat(path, &info); err != nil {
		return 0, 0, 0, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	return info.Uid, info.Gid, uint32(info.Mode) & 07777, nil
}
