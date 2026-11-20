// Package targets owns the managed units that setup writes on the machine of
// the operator: files, folders, links, marked blocks and named fields. Each
// unit has one record in the state, and each change follows the write order:
// save the first baseline, publish the intended value, check the destination
// again, write, verify and promote.
package targets

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

// Absent is the fingerprint of a unit that does not exist, and the baseline
// of a unit that did not exist before its first write.
const Absent = "absent"

// The values of Live.State. StateAbsent: no unit at the destination.
// StateApplied: the unit matches the applied value of its record.
// StateNext: the unit matches the intended value of an unfinished write.
// StateBefore: the unit matches the saved baseline, and setup does not own
// it. StateChanged: other content, which stays without a decision; this
// includes content that setup does not own and that differs from the desired
// content.
// StateUnreadable: setup cannot read the unit, which never proves absence.
const (
	StateAbsent     = "absent"
	StateApplied    = "applied"
	StateNext       = "next"
	StateBefore     = "before"
	StateChanged    = "changed"
	StateUnreadable = "unreadable"
)

// ErrChanged reports a destination that holds content which is not the
// recorded value. Setup keeps the content until the operator approves its
// replacement.
var ErrChanged = errors.New("the destination holds changed content; setup keeps it until you approve its replacement")

// Desired describes one managed unit. Path is the physical absolute path: a
// caller passes the result of platform.BindDestination, and a redirected
// parent stops the write. Slot names a part of the file at Path, and it is
// empty for a whole file, folder or link. Key is Path, or Path, "#" and Slot;
// an empty Key gets that value. Kind is the record kind: file, tree, link,
// block or field, or a kind of Writer.Kinds.
//
// A file takes Content with Mode (0600 when Mode is zero), or a copy of the
// regular file Tree. A tree is a copy of the folder Tree. A link points at
// the absolute path Link. A block takes its script in Content and its
// identity in Slot. A field takes its JSON value in Content and its name in
// Slot. BOM gives a new block file a UTF-8 byte order mark.
type Desired struct {
	Key, Resource, Kind, Path, Slot string
	Content                         []byte
	Link                            string
	Tree                            string
	Mode                            os.FileMode
	BOM                             bool
}

// Live is the inspection of one unit. Fingerprint is the live fingerprint
// and Want is the fingerprint after a write of the desired unit. Satisfied
// means that the unit needs no action: it equals the desired content, and
// either setup wrote it and the record is complete (StateApplied), or setup
// did not write it and the record holds it as a baseline only (StateBefore).
// Equal content without a record is not satisfied: Converge records it as a
// baseline, without ownership. Detail gives the cause of StateUnreadable.
type Live struct {
	State, Fingerprint, Want, Detail string
	Satisfied                        bool
}

// Choice is the decision of the operator for changed content.
type Choice int

// Preserve keeps changed content and stops the write with ErrChanged. Replace
// saves the changed content under the state folder first, then replaces it.
const (
	Preserve Choice = iota
	Replace
)

// Kind is the behavior of one kind of unit. The write order of the Writer
// calls it. A fingerprint is Absent or a text that covers the kind, the
// content and the relevant metadata of the unit. A baseline reference is
// Absent, "value:<base64>", "backup:<path>" or "fp:<fingerprint>".
//
// Slotted reports that the unit is a part of a file, so its key has a slot.
// Observe returns the live fingerprint and only reads. Fingerprint returns
// the fingerprint after a write of d. Baseline returns the fingerprint of a
// baseline reference. Recover completes or reverts a move that an earlier run
// left unfinished. Save stores the live unit and returns its baseline
// reference; dir is the baselines folder. Write replaces the live unit, whose
// fingerprint is from, with d, and fails with ErrChanged when the unit is not
// from. Restore replaces the live unit, whose fingerprint is from, with the
// baseline record.Before. Discard deletes the storage of a baseline that is
// restored or not necessary.
type Kind interface {
	Slotted() bool
	Observe(d Desired) (string, error)
	Fingerprint(d Desired) (string, error)
	Baseline(before string) (string, error)
	Recover(ctx context.Context, d Desired, record state.Record) error
	Save(ctx context.Context, d Desired, dir string) (string, error)
	Write(ctx context.Context, d Desired, from string) error
	Restore(ctx context.Context, d Desired, record state.Record, from string) error
	Discard(before, dir string) error
}

// Uncopyable is an optional interface of a Kind. A kind whose CannotCopy
// returns true declares that it cannot copy its unit, so Capture gives the
// live fingerprint as "fp:<fingerprint>" in place of a copy. For each other
// kind a failed Save is an error.
type Uncopyable interface {
	CannotCopy() bool
}

// Writer applies the write order to units. StateDir is the installer state
// folder: baselines and backups go to its baselines folder. Kinds adds or
// replaces kinds. A key of Kinds is a record kind, such as "font", or a
// record kind, "#" and a slot, such as "field#windows-terminal", for a kind
// that owns one slot name.
type Writer struct {
	StateDir string
	Kinds    map[string]Kind
}

var builtin = map[string]Kind{
	"file":  FileKind("file"),
	"tree":  FileKind("tree"),
	"link":  FileKind("link"),
	"block": SlotKind("block", Block{}),
	"field": SlotKind("field", Field{}),
}

var fault func(point string) error

func hit(point string) error {
	if fault != nil {
		return fault(point)
	}
	return nil
}

// Key returns the record key of a unit.
func Key(path, slot string) string {
	if slot == "" {
		return path
	}
	return path + "#" + slot
}

func (w *Writer) baselines() string { return filepath.Join(w.StateDir, "baselines") }

func (w *Writer) kind(name, slot string) (Kind, error) {
	if slot != "" {
		if kind, ok := w.Kinds[name+"#"+slot]; ok {
			return kind, nil
		}
	}
	if kind, ok := w.Kinds[name]; ok {
		return kind, nil
	}
	if kind, ok := builtin[name]; ok {
		return kind, nil
	}
	return nil, fmt.Errorf("unknown target kind %q", name)
}

func (w *Writer) desired(d Desired) (Desired, Kind, error) {
	if !filepath.IsAbs(w.StateDir) || !filepath.IsAbs(d.Path) || filepath.Clean(d.Path) != d.Path {
		return d, nil, errors.New("a target needs an absolute state folder and a cleaned absolute path")
	}
	if d.Key == "" {
		d.Key = Key(d.Path, d.Slot)
	}
	if d.Key != Key(d.Path, d.Slot) {
		return d, nil, fmt.Errorf("target key %q differs from its path and slot", d.Key)
	}
	kind, err := w.kind(d.Kind, d.Slot)
	if err != nil {
		return d, nil, err
	}
	if kind.Slotted() != (d.Slot != "") {
		return d, nil, fmt.Errorf("target %s has the wrong slot for the kind %s", d.Key, d.Kind)
	}
	return d, kind, nil
}

func (w *Writer) recorded(key string, record state.Record) (Desired, Kind, error) {
	d := Desired{Key: key, Resource: record.Resource, Kind: record.Kind, Path: key}
	if i := strings.LastIndexByte(key, '#'); i > 0 {
		if kind, err := w.kind(record.Kind, key[i+1:]); err == nil && kind.Slotted() {
			d.Path, d.Slot = key[:i], key[i+1:]
		}
	}
	return w.desired(d)
}

func bound(path string) error {
	physical, err := platform.BindDestination(path)
	if err != nil || physical != path {
		return errors.Join(fmt.Errorf("destination %s moved or was redirected; examine its parent folders, then run setup again", path), err)
	}
	return nil
}

func (w *Writer) live(kind Kind, d Desired, record state.Record, has bool) Live {
	unreadable := func(err error) Live { return Live{State: StateUnreadable, Detail: err.Error()} }
	if err := bound(d.Path); err != nil {
		return unreadable(err)
	}
	fingerprint, err := kind.Observe(d)
	if err != nil {
		return unreadable(fmt.Errorf("%s: %w", d.Path, err))
	}
	live := Live{State: StateChanged, Fingerprint: fingerprint}
	switch {
	case fingerprint == Absent:
		live.State = StateAbsent
	case has && !record.Release && record.Next != "" && fingerprint == record.Next:
		live.State = StateNext
	case has && record.Applied != "" && fingerprint == record.Applied:
		live.State = StateApplied
	case has && record.Before != "" && (record.Applied == "" || record.Release):
		baseline, err := kind.Baseline(record.Before)
		if err != nil {
			return unreadable(fmt.Errorf("baseline of %s: %w", d.Key, err))
		}
		if fingerprint == baseline {
			live.State = StateBefore
		}
	}
	return live
}

func lookup(rec engine.Records, d Desired) (state.Record, bool, error) {
	record, has := rec.State().Targets[d.Key]
	if has && record.Resource != d.Resource {
		return record, has, fmt.Errorf("target %s belongs to %s, not to %s", d.Key, record.Resource, d.Resource)
	}
	return record, has, nil
}

// Inspect reads one unit and compares it with its record. It writes nothing.
// The error reports an invalid description; an unreadable unit is a Live
// with StateUnreadable.
func (w *Writer) Inspect(d Desired, rec engine.Records) (Live, error) {
	d, kind, err := w.desired(d)
	if err != nil {
		return Live{}, err
	}
	record, has, err := lookup(rec, d)
	if err != nil {
		return Live{}, err
	}
	want, err := kind.Fingerprint(d)
	if err != nil {
		return Live{}, fmt.Errorf("%s: %w", d.Key, err)
	}
	live := w.live(kind, d, record, has)
	live.Want = want
	unowned := has && record.Applied == "" && record.Next == "" && !record.Release
	switch {
	case live.State == StateApplied:
		live.Satisfied = record.Applied == want && record.Next == "" && !record.Release
	case live.State == StateBefore && record.Applied == "" && live.Fingerprint != want:
		live.State = StateChanged
	case live.State == StateBefore:
		live.Satisfied = unowned
	}
	return live, nil
}

// InspectRecord reads the unit of a record without its desired content, for
// a release. Want is the fingerprint that the release leaves, when the
// baseline is readable. A key without a record gives StateAbsent.
func (w *Writer) InspectRecord(key string, rec engine.Records) (Live, error) {
	record, has := rec.State().Targets[key]
	if !has {
		return Live{State: StateAbsent, Fingerprint: Absent, Want: Absent}, nil
	}
	d, kind, err := w.recorded(key, record)
	if err != nil {
		return Live{}, err
	}
	live := w.live(kind, d, record, true)
	if baseline, err := kind.Baseline(baselineOf(record)); err == nil {
		live.Want = baseline
	}
	return live, nil
}

func baselineOf(record state.Record) string {
	if record.Before == "" {
		return Absent
	}
	return record.Before
}

func (w *Writer) promote(rec engine.Records, key string) (state.Record, error) {
	if err := rec.Update(func(s *state.State) error { return s.Promote(key) }); err != nil {
		return state.Record{}, err
	}
	return rec.State().Targets[key], hit("promote")
}

func (w *Writer) save(ctx context.Context, kind Kind, d Desired) (string, error) {
	if err := platform.PrepareDirectory(w.baselines()); err != nil {
		return "", err
	}
	saved, err := kind.Save(ctx, d, w.baselines())
	if err != nil {
		return "", fmt.Errorf("cannot save the earlier content of %s; move it away yourself, then run setup again: %w", d.Key, err)
	}
	return saved, nil
}

func (w *Writer) keep(ctx context.Context, d Desired, saved string) error {
	path, isBackup := strings.CutPrefix(saved, "backup:")
	if !isBackup {
		encoded, _ := strings.CutPrefix(saved, "value:")
		value, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return err
		}
		path = filepath.Join(w.baselines(), "replaced-"+rand.Text())
		if err := platform.SaveDocument(path, value); err != nil {
			return err
		}
	}
	platform.Detail(ctx, "Saved the earlier content of %s at %s", d.Key, path)
	return nil
}

func (w *Writer) disown(ctx context.Context, kind Kind, d Desired, rec engine.Records, record state.Record, has bool, fingerprint string) error {
	before := ""
	if has && record.Before != "" {
		if baseline, err := kind.Baseline(record.Before); err == nil && baseline == fingerprint {
			before = record.Before
		}
	}
	unowned := state.Record{Resource: d.Resource, Kind: d.Kind, Before: before}
	if before != "" && record.Kind == d.Kind && record.Applied == "" && record.Next == "" && !record.Release && len(record.Made) == 0 {
		return nil
	}
	fresh := before == ""
	if fresh {
		var err error
		if unowned.Before, err = w.save(ctx, kind, d); err != nil {
			return err
		}
	}
	if err := rec.Update(func(s *state.State) error { s.Targets[d.Key] = unowned; return nil }); err != nil {
		if fresh {
			err = errors.Join(err, kind.Discard(unowned.Before, w.baselines()))
		}
		return err
	}
	if path, isBackup := strings.CutPrefix(record.Before, "backup:"); has && isBackup && record.Before != unowned.Before {
		platform.Detail(ctx, "Kept the earlier content of %s at %s", d.Key, path)
	}
	return nil
}

// Converge makes one unit equal to d through the write order. A unit that
// matches the applied value of its record and an absent unit need no
// decision. Other content stops with ErrChanged, or with Replace it is saved
// first: as the first baseline when the unit has none, and otherwise as a
// file in the baselines folder that the progress display names.
//
// Equal content never gives ownership. When the unit equals d and this write
// order did not produce it, which means that it matches neither the applied
// value nor the intended value of an unfinished write, the record becomes a
// baseline only: the live unit is the baseline, there is no applied value,
// and a release leaves the unit. A later different d against such a record
// is changed content. A run after an interruption completes or repeats the
// write.
func (w *Writer) Converge(ctx context.Context, d Desired, rec engine.Records, c Choice) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d, kind, err := w.desired(d)
	if err != nil {
		return err
	}
	record, has, err := lookup(rec, d)
	if err != nil {
		return err
	}
	want, err := kind.Fingerprint(d)
	if err != nil {
		return fmt.Errorf("%s: %w", d.Key, err)
	}
	if err := bound(d.Path); err != nil {
		return err
	}
	if has {
		if err := kind.Recover(ctx, d, record); err != nil {
			return err
		}
	}
	live := w.live(kind, d, record, has)
	if live.State == StateUnreadable {
		return errors.New(live.Detail)
	}
	if live.State == StateNext {
		if record, err = w.promote(rec, d.Key); err != nil {
			return err
		}
		live.State = StateApplied
	}
	if has && live.State == StateApplied && record.Applied == want && record.Next == "" && !record.Release {
		return nil
	}
	if live.Fingerprint == want && live.State != StateApplied {
		return w.disown(ctx, kind, d, rec, record, has, live.Fingerprint)
	}
	saved := ""
	held := has && record.Applied == "" && live.State == StateBefore
	if live.State == StateChanged || held {
		if c != Replace {
			return fmt.Errorf("%s: %w", d.Key, ErrChanged)
		}
		if !held {
			if saved, err = w.save(ctx, kind, d); err != nil {
				return err
			}
			if has {
				if err := w.keep(ctx, d, saved); err != nil {
					return err
				}
			}
		}
	}
	if !has {
		first := state.Record{Resource: d.Resource, Kind: d.Kind, Before: Absent}
		if live.Fingerprint == Absent {
			first.Made, err = created(d)
		} else {
			first.Before = saved
		}
		if err != nil {
			return err
		}
		if err := rec.Update(func(s *state.State) error {
			if _, exists := s.Targets[d.Key]; exists {
				return fmt.Errorf("target %s got a record during the write", d.Key)
			}
			s.Targets[d.Key] = first
			return nil
		}); err != nil {
			return errors.Join(err, kind.Discard(first.Before, w.baselines()))
		}
		if err := hit("baseline"); err != nil {
			return err
		}
	}
	if err := rec.Update(func(s *state.State) error {
		return s.BeginTarget(d.Key, state.Record{Resource: d.Resource, Kind: d.Kind, Next: want})
	}); err != nil {
		return err
	}
	if err := hit("next"); err != nil {
		return err
	}
	if err := bound(d.Path); err != nil {
		return err
	}
	if again, err := kind.Observe(d); err != nil || again != live.Fingerprint {
		return errors.Join(fmt.Errorf("%s changed after approval: %w", d.Key, ErrChanged), err)
	}
	if err := hit("recheck"); err != nil {
		return err
	}
	if live.Fingerprint != want {
		if err := kind.Write(ctx, d, live.Fingerprint); err != nil {
			return fmt.Errorf("%s: %w", d.Key, err)
		}
	}
	if err := hit("write"); err != nil {
		return err
	}
	if after, err := kind.Observe(d); err != nil || after != want {
		return errors.Join(fmt.Errorf("%s differs from the selected content after its write; run setup again", d.Key), err)
	}
	_, err = w.promote(rec, d.Key)
	return err
}

func created(d Desired) ([]string, error) {
	made, err := platform.MissingDirectories(d.Path)
	if err != nil || d.Slot == "" {
		return made, err
	}
	if _, err := os.Lstat(d.Path); errors.Is(err, os.ErrNotExist) {
		made = append([]string{d.Path}, made...)
	}
	return made, nil
}

// Release removes one unit through the write order: it restores the first
// baseline, or absence, removes the empty folders that setup created and
// drops the record. The restored baseline stays unowned. A record without an
// applied value gives no ownership: the destination stays as it is, and the
// record and a baseline that equals the destination go away. Changed content
// stops with ErrChanged, or with Replace it is saved in the baselines folder
// first. A key without a record does nothing.
func (w *Writer) Release(ctx context.Context, key string, rec engine.Records, c Choice) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	record, has := rec.State().Targets[key]
	if !has {
		return nil
	}
	d, kind, err := w.recorded(key, record)
	if err != nil {
		return err
	}
	if err := bound(d.Path); err != nil {
		return err
	}
	if err := kind.Recover(ctx, d, record); err != nil {
		return err
	}
	live := w.live(kind, d, record, true)
	if live.State == StateUnreadable {
		return errors.New(live.Detail)
	}
	if live.State == StateNext {
		if record, err = w.promote(rec, key); err != nil {
			return err
		}
	}
	before := baselineOf(record)
	baseline, err := kind.Baseline(before)
	if err != nil {
		return fmt.Errorf("baseline of %s: %w", key, err)
	}
	drop := func() error { return rec.Update(func(s *state.State) error { s.Drop(key); return nil }) }
	if record.Applied == "" {
		if err := drop(); err != nil {
			return err
		}
		if live.Fingerprint == baseline {
			return kind.Discard(before, w.baselines())
		}
		if path, isBackup := strings.CutPrefix(before, "backup:"); isBackup {
			platform.Detail(ctx, "Kept the earlier content of %s at %s", key, path)
		}
		return nil
	}
	if !record.Release {
		if err := rec.Update(func(s *state.State) error { return s.BeginRelease(key) }); err != nil {
			return err
		}
		if err := hit("release"); err != nil {
			return err
		}
	}
	if live.Fingerprint != baseline {
		if live.Fingerprint != Absent && live.Fingerprint != record.Applied {
			if c != Replace {
				return fmt.Errorf("%s: %w", key, ErrChanged)
			}
			saved, err := w.save(ctx, kind, d)
			if err != nil {
				return err
			}
			if err := w.keep(ctx, d, saved); err != nil {
				return err
			}
		}
		if strings.HasPrefix(before, "fp:") {
			return fmt.Errorf("setup has no copy of the earlier content of %s; restore it yourself, then run setup again", key)
		}
		if err := kind.Restore(ctx, d, record, live.Fingerprint); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if err := hit("restore"); err != nil {
			return err
		}
		if after, err := kind.Observe(d); err != nil || after != baseline {
			return errors.Join(fmt.Errorf("%s differs from its baseline after the restore; run setup again", key), err)
		}
	}
	if baseline == Absent {
		made := record.Made
		if len(made) != 0 && made[0] == d.Path {
			made = made[1:]
		}
		if kept := platform.RemoveCreatedDirectories(made); kept != "" {
			platform.Detail(ctx, "Kept the folder %s, which setup created and could not remove", kept)
		}
	}
	if err := hit("made"); err != nil {
		return err
	}
	if err := drop(); err != nil {
		return err
	}
	if err := hit("drop"); err != nil {
		return err
	}
	return kind.Discard(before, w.baselines())
}
