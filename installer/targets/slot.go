package targets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

// Slot reads and edits one managed part of a text file of at most
// platform.MaxProfileBytes. Each byte outside the part stays. The value of a
// part is its canonical form, which is never empty: the fingerprint and the
// inline baseline use it.
//
// Extract returns the value of the part d.Slot in data, and false when data
// has no such part. Render returns the value of the desired content of d.
// Replace returns data with the part set to value, or without the part when
// present is false; data is empty for a file that does not exist. Empty
// reports that data holds nothing after the last part went away, so setup can
// delete a file that it created.
type Slot interface {
	Extract(data []byte, d Desired) (value []byte, present bool, err error)
	Render(d Desired) ([]byte, error)
	Replace(data []byte, d Desired, value []byte, present bool) ([]byte, error)
	Empty(data []byte, d Desired) (bool, error)
}

// Canonical is an optional interface of a Slot whose value has more than one
// spelling for the same settings. Fingerprint returns the fingerprint of a
// value, or Absent when the value holds no managed setting. The kind of the
// slot then uses it for each fingerprint: the live part, the desired content,
// the baseline and each check of an unfinished move.
type Canonical interface {
	Fingerprint(value []byte) (string, error)
}

type slotKind struct {
	name string
	slot Slot
}

// SlotKind returns a kind for a part of a file under the record kind name.
// Its fingerprint is the name and the SHA-256 of the value, or the
// fingerprint of a Canonical slot. Its baseline is
// inline: "value:<base64 of the value>". The write keeps the owner, the mode
// and the other metadata of the file. When setup created the file, the
// record lists the file as the first entry of its created folders, and the
// release deletes the file when it is empty again.
func SlotKind(name string, slot Slot) Kind { return slotKind{name, slot} }

func (k slotKind) fingerprint(value []byte) (string, error) {
	if canonical, ok := k.slot.(Canonical); ok {
		return canonical.Fingerprint(value)
	}
	sum := sha256.Sum256(value)
	return k.name + ":" + hex.EncodeToString(sum[:]), nil
}

func readSlotFile(path string) ([]byte, string, error) {
	if err := bound(path); err != nil {
		return nil, "", err
	}
	before, err := platform.SnapshotTreeBounded(path, platform.MaxProfileBytes, 1)
	if err != nil || before.Kind == "absent" {
		return nil, Absent, err
	}
	if before.Kind != "file" {
		return nil, "", errors.New("profile is not a regular file; preserve its redirect and use explicit migration")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	data, err := io.ReadAll(io.LimitReader(file, platform.MaxProfileBytes+1))
	if err = errors.Join(err, file.Close()); err != nil || len(data) > platform.MaxProfileBytes {
		return nil, "", errors.Join(errors.New("profile exceeds its read bound"), err)
	}
	after, err := platform.SnapshotTreeBounded(path, platform.MaxProfileBytes, 1)
	if err != nil || after != before {
		return nil, "", errors.Join(fmt.Errorf("%s changed during the inspection", path), err)
	}
	return data, fingerprintOf(before), nil
}

func (k slotKind) part(data []byte, d Desired) (string, []byte, error) {
	raw, present, err := k.slot.Extract(data, d)
	if err != nil {
		return "", nil, err
	}
	if !present {
		return Absent, nil, nil
	}
	if len(raw) == 0 {
		return "", nil, errors.New("a managed part has an empty value")
	}
	value, err := k.fingerprint(raw)
	if err != nil {
		return "", nil, err
	}
	return value, raw, nil
}

func (k slotKind) read(d Desired) (data []byte, file, value string, raw []byte, err error) {
	data, file, err = readSlotFile(d.Path)
	if err != nil || file == Absent {
		return nil, Absent, Absent, nil, err
	}
	if value, raw, err = k.part(data, d); err != nil {
		return nil, "", "", nil, err
	}
	return data, file, value, raw, nil
}

func (k slotKind) Slotted() bool { return true }

func (k slotKind) Observe(d Desired) (string, error) {
	_, _, value, _, err := k.read(d)
	return value, err
}

func (k slotKind) ObserveContent(d Desired, data []byte) (string, error) {
	value, _, err := k.part(data, d)
	return value, err
}

func (k slotKind) Fingerprint(d Desired) (string, error) {
	value, err := k.slot.Render(d)
	if err != nil {
		return "", err
	}
	return k.fingerprint(value)
}

func inlineValue(before string) ([]byte, error) {
	encoded, ok := strings.CutPrefix(before, "value:")
	if !ok {
		return nil, errors.New("the baseline of a part of a file must be an inline value")
	}
	return base64.StdEncoding.DecodeString(encoded)
}

func (k slotKind) Baseline(before string) (string, error) {
	if before == Absent {
		return Absent, nil
	}
	if fingerprint, ok := strings.CutPrefix(before, "fp:"); ok {
		return fingerprint, nil
	}
	value, err := inlineValue(before)
	if err != nil {
		return "", err
	}
	return k.fingerprint(value)
}

func (k slotKind) Save(_ context.Context, d Desired, _ string) (string, error) {
	_, _, value, raw, err := k.read(d)
	if err != nil {
		return "", err
	}
	if value == Absent {
		return "", errors.New("the part does not exist")
	}
	return "value:" + base64.StdEncoding.EncodeToString(raw), nil
}

func (k slotKind) Discard(string, string) error { return nil }

func (k slotKind) valueOf(data []byte, d Desired) (string, []byte, error) {
	raw, present, err := k.slot.Extract(data, d)
	if err != nil || !present {
		return Absent, nil, err
	}
	value, err := k.fingerprint(raw)
	return value, raw, err
}

func (k slotKind) Recover(ctx context.Context, d Desired, record state.Record) error {
	staged, aside := siblings(d)
	earlier, moved, err := readSlotFile(aside)
	if err != nil {
		return errors.Join(leftover(aside), err)
	}
	if moved != Absent {
		current, file, _, raw, err := k.read(d)
		if err != nil {
			return err
		}
		expected := record.Applied
		if expected == "" {
			if expected, err = k.Baseline(baselineOf(record)); err != nil {
				return err
			}
		}
		value, part, err := k.valueOf(earlier, d)
		replaced := err == nil && part != nil && savedIn(ctx, func(saved string) bool {
			info, err := os.Lstat(saved)
			if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(part)) {
				return false
			}
			copied, err := os.ReadFile(saved)
			return err == nil && bytes.Equal(copied, part)
		})
		if err != nil || !replaced && (value != expected || file == Absent && value == Absent && len(record.Made) != 0 && record.Made[0] == d.Path) {
			return errors.Join(leftover(aside), err)
		}
		if file == Absent {
			err = platform.MoveNoReplace(aside, d.Path)
		} else {
			expected, replaceErr := k.slot.Replace(earlier, d, raw, raw != nil)
			if replaceErr != nil || !bytes.Equal(expected, current) {
				return errors.Join(leftover(aside), replaceErr)
			}
			err = os.Remove(aside)
		}
		if err != nil {
			return err
		}
	}
	left, file, err := readSlotFile(staged)
	if err != nil {
		return errors.Join(leftover(staged), err)
	}
	if file == Absent {
		return nil
	}
	value, raw, err := k.valueOf(left, d)
	expected := expectedStage(k, record)
	if record.Release && expected == "" {
		expected = Absent
	}
	if err != nil || value != expected {
		return errors.Join(leftover(staged), err)
	}
	current, file, _, _, err := k.read(d)
	if err != nil {
		return err
	}
	if file == Absent && d.BOM {
		current = []byte{0xef, 0xbb, 0xbf}
	}
	present := raw != nil
	if baseline, err := inlineValue(baselineOf(record)); record.Release && err == nil {
		raw, present = baseline, true
	}
	rendered, err := k.slot.Replace(current, d, raw, present)
	if err != nil || !bytes.Equal(rendered, left) {
		return errors.Join(leftover(staged), err)
	}
	return os.Remove(staged)
}

func writeStaged(path string, content []byte) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("staged profile is not a regular file")
	}
	mode := info.Mode().Perm()
	readOnly := mode&0200 == 0
	if readOnly {
		if err := os.Chmod(path, mode|0200); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return errors.Join(err, os.Chmod(path, mode))
	}
	_, writeErr := file.Write(content)
	var modeErr error
	if readOnly {
		modeErr = file.Chmod(mode)
	}
	return errors.Join(writeErr, modeErr, file.Sync(), file.Close())
}

func (k slotKind) publish(ctx context.Context, d Desired, from string, value []byte, present, mayDelete bool) error {
	data, file, live, _, err := k.read(d)
	if err != nil {
		return err
	}
	if live != from {
		return fmt.Errorf("%s changed during the write: %w", d.Path, ErrChanged)
	}
	exists := file != Absent
	if !exists && !present {
		return nil
	}
	if !exists && d.BOM {
		data = []byte{0xef, 0xbb, 0xbf}
	}
	content, err := k.slot.Replace(data, d, value, present)
	if err != nil {
		return err
	}
	if len(content) > platform.MaxProfileBytes {
		return errors.New("merged profile exceeds its size bound")
	}
	remove := false
	if !present && mayDelete {
		if remove, err = k.slot.Empty(content, d); err != nil {
			return err
		}
	}
	staged, aside := siblings(d)
	clean := func() error { return nil }
	if !remove {
		if err := os.MkdirAll(filepath.Dir(d.Path), 0700); err != nil {
			return err
		}
		if err := reserve(staged); err != nil {
			return err
		}
		if exists {
			if err := platform.CopyWithMetadata(ctx, d.Path, staged); err != nil {
				return fmt.Errorf("cannot stage profile %s at %s: %w; original profile remains active; resolve the reported file access or ownership/permissions problem, then run setup again", d.Path, staged, err)
			}
		} else {
			created, err := os.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if errors.Is(err, os.ErrExist) {
				return errors.Join(leftover(staged), err)
			}
			if err != nil {
				return err
			}
			if err := errors.Join(created.Chmod(fileMode(d)), created.Close()); err != nil {
				return errors.Join(err, removeEntry(staged))
			}
		}
		clean = func() error { return removeEntry(staged) }
		if err := writeStaged(staged, content); err != nil {
			return errors.Join(err, clean())
		}
	}
	if exists {
		if err := platform.MoveNoReplace(d.Path, aside); err != nil {
			return errors.Join(err, clean())
		}
		if moved, err := snapshot(aside); err != nil || moved != file {
			return errors.Join(fmt.Errorf("%s changed during the write: %w", d.Path, ErrChanged), err, platform.MoveNoReplace(aside, d.Path), clean())
		}
		if err := hit("move"); err != nil {
			return err
		}
	}
	if !remove {
		if err := moveIn(staged, d.Path); err != nil {
			return err
		}
		if err := hit("publish"); err != nil {
			return err
		}
	}
	if exists {
		return os.Remove(aside)
	}
	return nil
}

func (k slotKind) Write(ctx context.Context, d Desired, from string) error {
	value, err := k.slot.Render(d)
	if err != nil {
		return err
	}
	return k.publish(ctx, d, from, value, true, false)
}

func (k slotKind) Restore(ctx context.Context, d Desired, record state.Record, from string) error {
	if baselineOf(record) == Absent {
		return k.publish(ctx, d, from, nil, false, len(record.Made) != 0 && record.Made[0] == d.Path)
	}
	value, err := inlineValue(record.Before)
	if err != nil {
		return err
	}
	return k.publish(ctx, d, from, value, true, false)
}
