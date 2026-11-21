//go:build !windows

package platform

import (
	"errors"
	"os"
	"syscall"
)

// RemoveEmptyDirectory removes dir only when it is an empty real folder. It
// reports false, without an error, for a folder with content.
func RemoveEmptyDirectory(dir string) (bool, error) {
	err := syscall.Rmdir(dir)
	switch {
	case err == nil, errors.Is(err, syscall.ENOENT):
		return true, nil
	case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST), errors.Is(err, syscall.ENOTDIR):
		return false, nil
	}
	return false, err
}

// RemoveOwned is os.Remove for an entry of the verified scratch space of the
// installer. On Windows it also removes a read-only folder.
func RemoveOwned(path string) error {
	return os.Remove(path)
}
