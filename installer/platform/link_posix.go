//go:build darwin || linux

package platform

import (
	"os"
	"path/filepath"
)

// CreateDirectoryLink makes link point at the folder target: a symbolic link
// on POSIX and a junction on Windows.
func CreateDirectoryLink(target, link string) error { return os.Symlink(target, link) }

// ResolvePath returns the physical path of an existing path. On Windows it
// also follows junctions.
func ResolvePath(path string) (string, error) { return filepath.EvalSymlinks(path) }
