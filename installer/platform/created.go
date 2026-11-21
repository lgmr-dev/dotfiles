package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// MaxCreatedDirectories bounds the missing ancestors of a target.
const MaxCreatedDirectories = 16

// MissingDirectories returns the folders above path that do not exist,
// nearest first, up to the first existing ancestor. A publication creates
// exactly these folders.
func MissingDirectories(path string) ([]string, error) {
	missing := []string{}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		_, err := os.Lstat(dir)
		if err == nil {
			return missing, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if filepath.Dir(dir) == dir || len(missing) == MaxCreatedDirectories {
			return nil, fmt.Errorf("target %s has no existing ancestor within %d folders", path, MaxCreatedDirectories)
		}
		missing = append(missing, dir)
	}
}

// ValidCreatedDirectories reports that created is the exact ancestor chain of
// path, nearest first.
func ValidCreatedDirectories(path string, created []string) bool {
	if len(created) > MaxCreatedDirectories {
		return false
	}
	dir := path
	for _, recorded := range created {
		if parent := filepath.Dir(dir); parent == dir || recorded != parent {
			return false
		}
		dir = recorded
	}
	return true
}

// RemoveCreatedDirectories removes recorded folders, nearest first, each only
// while it is an empty real folder under unredirected parents. A folder that
// stays keeps each ancestor. Links, junctions and personal additions stay. The
// removal is cosmetic: on an unexpected failure it returns the folder that
// stays and does not fail.
func RemoveCreatedDirectories(created []string) (kept string) {
	for _, dir := range created {
		if bound, err := BindDestination(dir); err != nil {
			return dir
		} else if bound != dir {
			return ""
		}
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return dir
		}
		if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return ""
		}
		removed, err := RemoveEmptyDirectory(dir)
		if err != nil {
			return dir
		}
		if !removed {
			return ""
		}
	}
	return ""
}
