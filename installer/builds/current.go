package builds

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

// Publish makes the current link of a resource point at build and records
// the link as a link target of the resource: the intended build before the
// switch, the applied build after it. It removes only the staged links that
// the record of the publication names: the link to the intended build and
// the moved link to the applied build. Each other entry stays.
func Publish(stateDir, id, build string, rec engine.Records) error {
	current := CurrentDir(stateDir, id)
	dir := ResourceDir(stateDir, id)
	record, recorded := rec.State().Targets[current]
	link, err := linkTarget(current)
	if err != nil {
		return err
	}
	if recorded && record.Next != build {
		if err := removeRecordedStagedLinks(dir, record); err != nil {
			return err
		}
	}
	if !recorded || record.Applied != build || record.Next != "" || record.Release || link != build {
		if err := rec.Update(func(s *state.State) error {
			return s.BeginTarget(current, state.Record{Resource: id, Kind: "link", Next: build, Before: "absent"})
		}); err != nil {
			return err
		}
		if err := switchLink(dir, build); err != nil {
			return err
		}
		link, err = linkTarget(current)
		if err != nil || link != build {
			return errors.Join(fmt.Errorf("the link %s does not point at the new build", current), err)
		}
		if err := removeRecordedStagedLinks(dir, rec.State().Targets[current]); err != nil {
			return err
		}
		if err := rec.Update(func(s *state.State) error { return s.Promote(current) }); err != nil {
			return err
		}
	}
	return nil
}

// Unpublish removes the current link of a resource and its record. The link
// goes only while it is the one that setup published. A resource without a
// record stays as it is. The builds stay for Sweep.
func Unpublish(stateDir, id string, rec engine.Records) error {
	current := CurrentDir(stateDir, id)
	record, recorded := rec.State().Targets[current]
	if !recorded {
		return nil
	}
	link, err := linkTarget(current)
	if err != nil {
		return err
	}
	if link != "" && link != record.Applied && link != record.Next {
		return fmt.Errorf("the link %s is not the one that setup published; it stays", current)
	}
	if record.Applied != "" && !record.Release {
		if err := rec.Update(func(s *state.State) error { return s.BeginRelease(current) }); err != nil {
			return err
		}
	}
	if link != "" {
		if err := os.Remove(current); err != nil {
			return err
		}
	}
	if err := removeRecordedStagedLinks(ResourceDir(stateDir, id), record); err != nil {
		return err
	}
	if err := platform.SyncDirectory(ResourceDir(stateDir, id)); err != nil {
		return err
	}
	return rec.Update(func(s *state.State) error { s.Drop(current); return nil })
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

func recordedStagedLink(dir, path string, record state.Record) bool {
	if !filepath.IsAbs(record.Next) {
		return false
	}
	target, err := linkTarget(path)
	if err != nil || target == "" {
		return false
	}
	return path == stagedLink(dir, "next", record.Next) && target == record.Next ||
		path == stagedLink(dir, "previous", record.Next) && target == record.Applied
}

func removeRecordedStagedLinks(dir string, record state.Record) error {
	if !filepath.IsAbs(record.Next) {
		return nil
	}
	for _, kind := range []string{"next", "previous"} {
		path := stagedLink(dir, kind, record.Next)
		if !recordedStagedLink(dir, path, record) {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}
