package builds

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

func (p *Provider) publish(id, build string, rec engine.Records) error {
	current := CurrentDir(p.StateDir, id)
	record, recorded := rec.State().Targets[current]
	link, err := linkTarget(current)
	if err != nil {
		return err
	}
	if !recorded || record.Applied != build || record.Next != "" || record.Release || link != build {
		if err := rec.Update(func(s *state.State) error {
			return s.BeginTarget(current, state.Record{Resource: id, Kind: "link", Next: build, Before: "absent"})
		}); err != nil {
			return err
		}
		if err := switchLink(ResourceDir(p.StateDir, id), build); err != nil {
			return err
		}
		link, err = linkTarget(current)
		if err != nil || link != build {
			return errors.Join(fmt.Errorf("the link %s does not point at the new build", current), err)
		}
		if err := rec.Update(func(s *state.State) error { return s.Promote(current) }); err != nil {
			return err
		}
	}
	return removeStagedLinks(p.StateDir, id)
}

func stagedLink(dir, kind, build string) string {
	return filepath.Join(dir, "."+kind+"-"+filepath.Base(build))
}

func switchLink(dir, build string) error {
	current := filepath.Join(dir, "current")
	previous, next := stagedLink(dir, "previous", build), stagedLink(dir, "next", build)
	actual, err := linkTarget(current)
	if err != nil {
		return err
	}
	if actual == build {
		return nil
	}
	if actual != "" {
		if _, err := os.Lstat(previous); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(fmt.Errorf("an earlier link stays at %s; examine it and move it away", previous), err)
		}
		if err := platform.MoveNoReplace(current, previous); err != nil {
			return err
		}
	}
	staged, err := linkTarget(next)
	if err != nil {
		return err
	}
	if staged == "" {
		if err := platform.CreateDirectoryLink(build, next); err != nil {
			return err
		}
		if err := platform.SyncDirectory(dir); err != nil {
			return err
		}
	} else if staged != build {
		return fmt.Errorf("the staged link %s changed; examine it and move it away", next)
	}
	if err := platform.MoveNoReplace(next, current); err != nil {
		return err
	}
	return platform.SyncDirectory(dir)
}

func removeStagedLinks(stateDir, id string) error {
	dir := ResourceDir(stateDir, id)
	entries, err := platform.ReadPlainDirectory(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".previous-") && !strings.HasPrefix(entry.Name(), ".next-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		target, err := linkTarget(path)
		if err != nil || !ownBuild(stateDir, id, target) {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}
