//go:build windows

package platform

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// RemoveEmptyDirectory removes dir only when it is an empty real folder. It
// reports false, without an error, for a folder with content. The caller must
// refuse a reparse point first, because RemoveDirectory unlinks one.
func RemoveEmptyDirectory(dir string) (bool, error) {
	remove := func() error {
		path, err := windows.UTF16PtrFromString(dir)
		if err != nil {
			return err
		}
		return windows.RemoveDirectory(path)
	}
	err := remove()
	if err != nil {
		err = retryReadOnlyDirectory(dir, err, remove)
	}
	switch {
	case err == nil, errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return true, nil
	case errors.Is(err, windows.ERROR_DIR_NOT_EMPTY):
		return false, nil
	}
	return false, err
}

// RemoveOwned is os.Remove for an entry of the verified scratch space of the
// installer. On Windows it also removes a read-only folder, which OneDrive
// makes of each folder that it syncs.
func RemoveOwned(path string) error {
	err := os.Remove(path)
	if err == nil {
		return nil
	}
	return retryReadOnlyDirectory(path, err, func() error { return os.Remove(path) })
}

func retryReadOnlyDirectory(path string, err error, remove func() error) error {
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return err
	}
	info, statErr := os.Lstat(path)
	if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0200 != 0 {
		return err
	}
	if chmodErr := os.Chmod(path, info.Mode().Perm()|0200); chmodErr != nil {
		return errors.Join(err, chmodErr)
	}
	retryErr := remove()
	if retryErr == nil {
		return nil
	}
	if restoreErr := os.Chmod(path, info.Mode().Perm()); restoreErr != nil {
		return fmt.Errorf("cannot restore the read-only attribute of %s after a failed removal (%v): %w", path, retryErr, restoreErr)
	}
	return retryErr
}
