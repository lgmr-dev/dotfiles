package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/luisgui1757/dotfiles/installer/platform"
)

type settingsLockOwner struct{ Directory, Target string }

const settingsLockMarker = ".dotfiles-owner.json"

func settingsLockRead(path string) (settingsLockOwner, os.FileInfo, error) {
	var owner settingsLockOwner
	info, err := os.Lstat(path)
	if err != nil {
		return owner, nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return owner, nil, errors.New("settings lock is not an ordinary directory")
	}
	data, err := platform.ReadDocument(filepath.Join(path, settingsLockMarker))
	if err == nil {
		err = platform.Decode(data, &owner)
	}
	return owner, info, err
}

func settingsLock(directory, target string) (func() error, error) {
	lock, stage := target+".lock", target+".dotfiles-lock"
	bound, err := platform.BindDestination(lock)
	if err != nil || bound != lock {
		return nil, errors.Join(errors.New("settings lock target was redirected"), err)
	}
	owner := settingsLockOwner{directory, target}
	verify := func(path string) (os.FileInfo, error) {
		observed, info, err := settingsLockRead(path)
		if err != nil || observed != owner {
			return nil, errors.Join(errors.New("settings lock belongs to another operation"), err)
		}
		return info, nil
	}
	info, err := os.Lstat(lock)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Remove(stage); err != nil && !errors.Is(err, os.ErrNotExist) {
			if _, err := verify(stage); err != nil {
				return nil, fmt.Errorf("preserve unknown settings lock at %s: %w", stage, err)
			}
		} else {
			marker, err := json.Marshal(owner)
			if err != nil {
				return nil, err
			}
			if err := os.Mkdir(stage, 0700); err != nil {
				return nil, err
			}
			if err := platform.SaveDocument(filepath.Join(stage, settingsLockMarker), marker); err != nil {
				return nil, fmt.Errorf("preserve incomplete settings lock at %s: %w", stage, err)
			}
		}
		if err := platform.MoveNoReplace(stage, lock); err != nil {
			return nil, fmt.Errorf("settings are busy at %s; preserve %s and retry after the application finishes: %w", lock, stage, err)
		}
	} else if err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, fmt.Errorf("settings are busy or redirected at %s", lock)
	}
	info, err = verify(lock)
	if err != nil {
		return nil, fmt.Errorf("settings are busy at %s; let the application finish before retrying: %w", lock, err)
	}
	return func() error {
		current, err := verify(lock)
		if err != nil || !os.SameFile(current, info) {
			return errors.Join(fmt.Errorf("settings lock changed; preserve %s", lock), err)
		}
		if err := platform.MoveNoReplace(lock, stage); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(stage, settingsLockMarker)); err != nil {
			return err
		}
		return os.Remove(stage)
	}, nil
}

func settingsLocks(directory string, paths []string) (func() error, error) {
	releases := []func() error{}
	releaseAll := func() error {
		var result error
		for i := len(releases) - 1; i >= 0; i-- {
			result = errors.Join(result, releases[i]())
		}
		return result
	}
	for _, path := range paths {
		if _, err := os.Lstat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
			continue
		}
		release, err := settingsLock(directory, path)
		if err != nil {
			return nil, errors.Join(err, releaseAll())
		}
		releases = append(releases, release)
	}
	return releaseAll, nil
}
