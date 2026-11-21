package platform

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

// MaxProfileBytes bounds a file that CopyWithMetadata copies.
const MaxProfileBytes = 256 << 10

func removeFailedCopy(path string, created os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(created, current) {
		return fmt.Errorf("staged profile changed; preserve %s for inspection", path)
	}
	if runtime.GOOS == "windows" && current.Mode().Perm()&0200 == 0 {
		if err := os.Chmod(path, 0600); err != nil {
			return fmt.Errorf("cannot clean failed profile copy at %s: %w", path, err)
		}
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("cannot clean failed profile copy at %s: %w", path, err)
	}
	return nil
}
