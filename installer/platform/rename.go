package platform

import (
	"errors"
	"os"
	"path/filepath"
)

// MoveNoReplace renames from to to and fails when to exists. Both parent
// folders are pinned for the native rename, and both paths must be bound
// destinations. A move keeps the native permissions and the link identity.
func MoveNoReplace(from, to string) (result error) {
	left, err := openParent(from)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, left.Close()) }()
	right, err := openParent(to)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, right.Close()) }()
	if err := renameNoReplace(left, filepath.Base(from), right, filepath.Base(to)); err != nil {
		return err
	}
	return errors.Join(syncHandle(left), syncHandle(right))
}

func openParent(path string) (*os.File, error) {
	bound, err := BindDestination(path)
	if err != nil || bound != path {
		return nil, errors.Join(errors.New("configuration parent changed; preserve recovery files"), err)
	}
	parent := filepath.Dir(path)
	before, err := os.Lstat(parent)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, errors.Join(errors.New("configuration parent is not a directory"), err)
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	file, err := root.Open(".")
	if err = errors.Join(err, root.Close()); err != nil {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		return nil, err
	}
	opened, statErr := file.Stat()
	after, pathErr := os.Lstat(parent)
	if statErr != nil || pathErr != nil || !os.SameFile(before, opened) || !os.SameFile(before, after) {
		return nil, errors.Join(errors.New("configuration parent changed while opening"), statErr, pathErr, file.Close())
	}
	return file, nil
}
