package platform

import (
	"errors"
	"os"
	"path/filepath"
)

// BindDestination returns the physical path of a destination: it resolves the
// deepest existing parent and keeps the missing part. It creates nothing. A
// redirected known folder or a replaced parent link gives a different result,
// which shows that an approved destination moved.
func BindDestination(path string) (string, error) {
	parent, suffix := filepath.Dir(path), []string{filepath.Base(path)}
	for {
		info, err := os.Lstat(parent)
		if err == nil {
			resolved, err := ResolvePath(parent)
			if err != nil {
				return "", err
			}
			info, err = os.Stat(resolved)
			if err != nil || !info.IsDir() {
				return "", errors.Join(errors.New("configuration parent is not a directory"), err)
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(parent) == parent {
			return "", err
		}
		suffix = append(suffix, filepath.Base(parent))
		parent = filepath.Dir(parent)
	}
}
