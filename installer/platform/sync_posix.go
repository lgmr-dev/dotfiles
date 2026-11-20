//go:build darwin || linux

package platform

import (
	"errors"
	"os"
)

// SyncDirectory flushes a folder to disk. It does nothing on Windows, where a
// directory handle cannot be flushed.
func SyncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(syncHandle(file), file.Close())
}

func syncHandle(file *os.File) error { return file.Sync() }
