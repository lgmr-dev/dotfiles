package targets

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

type pathKind struct{ name string }

// FileKind returns a kind for a whole file, folder or link under the record
// kind name. The name "link" gives a link: a symbolic link on POSIX and a
// junction on Windows. Each other name, for example "font", gives the file
// and tree behavior: Content with Mode, or a copy of Tree. The fingerprint
// is the snapshot of the path: it covers the entry kind, the mode and the
// content of each file, and the target of a link, never what a link points
// at. A baseline is a copy in the baselines folder.
func FileKind(name string) Kind { return pathKind{name} }

func fingerprintOf(snapshot platform.Snapshot) string {
	if snapshot.Kind == "absent" {
		return Absent
	}
	return snapshot.Kind + ":" + snapshot.Hash
}

func snapshot(path string) (string, error) {
	result, err := platform.SnapshotTree(path)
	if err != nil {
		return "", err
	}
	return fingerprintOf(result), nil
}

func siblings(d Desired) (staged, aside string) {
	sum := sha256.Sum256([]byte(d.Key))
	name := filepath.Join(filepath.Dir(d.Path), ".dotfiles-"+hex.EncodeToString(sum[:8]))
	return name + ".next", name + ".previous"
}

func removeEntry(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return os.Remove(path)
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(root.RemoveAll(filepath.Base(path)), root.Close())
}

func removeMatching(path, fingerprint string) error {
	current, err := snapshot(path)
	if err != nil || current != fingerprint {
		return errors.Join(fmt.Errorf("%s changed; setup keeps it: examine it, move it away, then run setup again", path), err)
	}
	return removeEntry(path)
}

func fileMode(d Desired) os.FileMode {
	mode := d.Mode.Perm()
	if mode == 0 {
		mode = 0600
	}
	if runtime.GOOS == "windows" {
		if mode&0200 == 0 {
			return 0444
		}
		return 0666
	}
	return mode
}

func (k pathKind) Slotted() bool { return false }

func (k pathKind) Observe(d Desired) (string, error) { return snapshot(d.Path) }

func (k pathKind) Fingerprint(d Desired) (string, error) {
	entry := platform.Entry{Path: "."}
	switch {
	case k.name == "link":
		if !filepath.IsAbs(d.Link) || filepath.Clean(d.Link) != d.Link {
			return "", errors.New("a link target needs a cleaned absolute path")
		}
		entry.Kind, entry.Content = "link", d.Link
		if runtime.GOOS == "windows" {
			entry.Kind = "junction"
		}
	case d.Tree != "":
		source, err := platform.SnapshotTree(d.Tree)
		expected := "file"
		if k.name == "tree" {
			expected = "directory"
		}
		if err != nil || source.Kind != expected {
			return "", errors.Join(errors.New("canonical configuration source is unavailable or has the wrong type; restore it before applying"), err)
		}
		return fingerprintOf(source), nil
	case k.name == "tree":
		return "", errors.New("a tree target needs its source folder")
	default:
		sum := sha256.Sum256(d.Content)
		entry.Kind, entry.Content, entry.Mode = "file", hex.EncodeToString(sum[:]), uint32(fileMode(d))
	}
	result, err := platform.SnapshotEntries([]platform.Entry{entry})
	return fingerprintOf(result), err
}

func (k pathKind) Baseline(before string) (string, error) {
	if before == Absent {
		return Absent, nil
	}
	if fingerprint, ok := strings.CutPrefix(before, "fp:"); ok {
		return fingerprint, nil
	}
	path, ok := strings.CutPrefix(before, "backup:")
	if !ok {
		return "", errors.New("the baseline of a file, folder or link must be a backup")
	}
	fingerprint, err := snapshot(path)
	if err == nil && fingerprint == Absent {
		err = fmt.Errorf("the saved baseline %s is missing; restore it, or restore the earlier content yourself", path)
	}
	return fingerprint, err
}

func (k pathKind) stage(d Desired, staged string) error {
	switch {
	case k.name == "link":
		return platform.CreateDirectoryLink(d.Link, staged)
	case d.Tree != "":
		return platform.CopyTree(d.Tree, staged)
	}
	file, err := os.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(d.Content)
	return errors.Join(err, file.Chmod(fileMode(d)), file.Sync(), file.Close())
}

func copyUnit(ctx context.Context, from, to string) error {
	source, err := platform.SnapshotTree(from)
	if err != nil {
		return err
	}
	switch source.Kind {
	case "link", "junction":
		target, err := os.Readlink(from)
		if err != nil {
			return err
		}
		if source.Kind == "junction" {
			err = platform.CreateDirectoryLink(target, to)
		} else {
			err = os.Symlink(target, to)
		}
		if err != nil {
			return err
		}
	case "file":
		info, err := os.Lstat(from)
		if err != nil {
			return err
		}
		if info.Size() <= platform.MaxProfileBytes {
			err = platform.CopyWithMetadata(ctx, from, to)
		} else if err = platform.CopyTree(from, to); err != nil {
			err = errors.Join(err, removeEntry(to))
		}
		if err != nil {
			return err
		}
	case "directory":
		if err := platform.CopyTree(from, to); err != nil {
			return errors.Join(err, removeEntry(to))
		}
	default:
		return fmt.Errorf("%s does not exist", from)
	}
	copied, err := platform.SnapshotTree(to)
	if err != nil || copied != source {
		return errors.Join(fmt.Errorf("the copy of %s differs from its source", from), err, removeEntry(to))
	}
	return nil
}

func (k pathKind) Save(ctx context.Context, d Desired, dir string) (string, error) {
	backup := filepath.Join(dir, strings.TrimLeft(filepath.Base(d.Path), ".")+"-"+rand.Text())
	if err := copyUnit(ctx, d.Path, backup); err != nil {
		return "", err
	}
	return "backup:" + backup, nil
}

func leftover(path string) error {
	return fmt.Errorf("%s has a name that setup reserves, and the record cannot prove that setup made it; examine it, move it away, then run setup again", path)
}

func reserve(staged string) error {
	if _, err := os.Lstat(staged); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(leftover(staged), err)
	}
	return nil
}

func expectedStage(kind Kind, record state.Record) string {
	if !record.Release {
		return record.Next
	}
	baseline, err := kind.Baseline(baselineOf(record))
	if err != nil || baseline == Absent {
		return ""
	}
	return baseline
}

func (k pathKind) Recover(ctx context.Context, d Desired, record state.Record) error {
	staged, aside := siblings(d)
	moved, err := snapshot(aside)
	if err != nil {
		return errors.Join(leftover(aside), err)
	}
	if moved != Absent {
		baseline := ""
		if record.Before != "" {
			baseline, _ = k.Baseline(record.Before)
		}
		if moved != record.Applied && moved != baseline && !savedIn(ctx, func(saved string) bool {
			copied, err := snapshot(saved)
			return err == nil && copied == moved
		}) {
			return leftover(aside)
		}
		current, err := snapshot(d.Path)
		if err != nil {
			return err
		}
		if current == Absent {
			err = platform.MoveNoReplace(aside, d.Path)
		} else {
			err = removeEntry(aside)
		}
		if err != nil {
			return err
		}
	}
	left, err := snapshot(staged)
	if err != nil {
		return errors.Join(leftover(staged), err)
	}
	if left == Absent {
		return nil
	}
	if left != expectedStage(k, record) {
		return leftover(staged)
	}
	return removeEntry(staged)
}

func (k pathKind) swap(d Desired, from, want string, stage func(staged string) error) error {
	staged, aside := siblings(d)
	clean := func() error { return nil }
	if stage != nil {
		if err := os.MkdirAll(filepath.Dir(d.Path), 0700); err != nil {
			return err
		}
		if err := reserve(staged); err != nil {
			return err
		}
		if err := stage(staged); err != nil {
			if errors.Is(err, os.ErrExist) {
				return errors.Join(leftover(staged), err)
			}
			return errors.Join(err, removeEntry(staged))
		}
		clean = func() error { return removeEntry(staged) }
		if made, err := snapshot(staged); err != nil || made != want {
			return errors.Join(errors.New("the staged content differs from the selected content"), err, clean())
		}
	}
	if from != Absent {
		if err := platform.MoveNoReplace(d.Path, aside); err != nil {
			return errors.Join(err, clean())
		}
		if moved, err := snapshot(aside); err != nil || moved != from {
			return errors.Join(fmt.Errorf("%s changed during the write: %w", d.Path, ErrChanged), err, platform.MoveNoReplace(aside, d.Path), clean())
		}
		if err := hit("move"); err != nil {
			return err
		}
	}
	if stage != nil {
		if err := moveIn(staged, d.Path); err != nil {
			return err
		}
	}
	if from != Absent {
		return removeMatching(aside, from)
	}
	return nil
}

func moveIn(staged, path string) error {
	err := platform.MoveNoReplace(staged, path)
	if err == nil {
		return nil
	}
	if _, statErr := os.Lstat(path); statErr == nil {
		err = fmt.Errorf("%s appeared during the write: %w", path, ErrChanged)
	}
	return errors.Join(err, removeEntry(staged))
}

func (k pathKind) Write(_ context.Context, d Desired, from string) error {
	want, err := k.Fingerprint(d)
	if err != nil {
		return err
	}
	return k.swap(d, from, want, func(staged string) error { return k.stage(d, staged) })
}

func (k pathKind) Restore(ctx context.Context, d Desired, record state.Record, from string) error {
	backup, ok := strings.CutPrefix(record.Before, "backup:")
	if !ok {
		return k.swap(d, from, Absent, nil)
	}
	want, err := k.Baseline(record.Before)
	if err != nil {
		return err
	}
	return k.swap(d, from, want, func(staged string) error { return copyUnit(ctx, backup, staged) })
}

func (k pathKind) Discard(before, dir string) error {
	backup, ok := strings.CutPrefix(before, "backup:")
	if !ok || filepath.Dir(backup) != dir {
		return nil
	}
	return removeEntry(backup)
}
