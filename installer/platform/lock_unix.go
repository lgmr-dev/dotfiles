//go:build !windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Lock takes the exclusive installer lock at path and creates the file and its
// private folder when necessary. It does not wait: a second holder fails. The
// lock ends when release runs or the process exits.
func Lock(path string) (func() error, error) {
	if err := PrepareDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_CREAT, 0600)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, errors.Join(err, unix.Close(fd))
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || int(stat.Uid) != os.Getuid() || stat.Mode&0077 != 0 {
		return nil, errors.Join(errors.New("mutation lock must be a private regular file owned by this user"), unix.Close(fd))
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("another installer holds the mutation lock: %w", err), unix.Close(fd))
	}
	return func() error { return unix.Close(fd) }, nil
}

func publishDocument(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
