package v2import

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

// RestoreItem is the plan for one saved target. Action is "none" for a
// target that already holds its saved value, "restore" for a target that
// holds the value of release v3 and gets its saved value, "remove" for a
// target that the switch created, "changed" for a target that you edited
// after the switch, "manual" for a target that has no copy by design, such
// as a font registration, and "blocked" for a target that setup cannot
// compare with its saved value. A blocked target stops the restore before
// the publication of the schema-1 state. Replacement is "restore" or
// "remove" for a changed target: the action that an approved replacement
// does.
type RestoreItem struct {
	Key, Resource, Action, Detail string
	Replacement                   string

	live, saved string
}

// Restoration is the preview of restore-v2. Done is true when the schema-1
// state is active again and nothing is left to do. Pending lists the pending
// marks of the state. Confirmed lists the marks whose command you confirmed
// as exited: Run stops at each other mark.
type Restoration struct {
	Items     []RestoreItem
	Done      bool
	Pending   []string
	Confirmed []string

	o        Options
	reviewed bool
	digest   [sha256.Size]byte
}

var errSavedChanged = errors.New("the saved v2 state changed after the review; run restore-v2 again")

const afterRestore = "Release v2 is active again. Check out " + SourceCommit + " (" + SourceRelease + ") and run its setup."

func (r *Restoration) classify(set *restoreSet, st state.State, key string) RestoreItem {
	slot := set.manifest.Slots[key]
	item := RestoreItem{Key: key, Resource: slot.Resource, Action: "blocked"}
	inspect := func(before string) (targets.Live, error) {
		live, err := set.writer.InspectRecord(key, &memory{state.State{Targets: map[string]state.Record{key: {Resource: slot.Resource, Kind: slot.Kind, Before: before}}}})
		if err == nil && live.State == targets.StateUnreadable {
			err = errors.New(live.Detail)
		}
		return live, err
	}
	live, err := inspect(slot.Saved)
	if err != nil {
		item.Detail = "setup cannot compare it with its saved value: " + err.Error()
		return item
	}
	if live.Want != slot.Fingerprint {
		item.Detail = "its saved copy in " + set.dir + " changed after the switch, and setup does not give back a changed copy"
		return item
	}
	started, restoring := set.manifest.Restoring[key]
	record, has := st.Targets[key]
	deleted := has && record.Applied != "" && record.Next == "" && !record.Release && !restoring
	candidates := []string{live.Want}
	if !deleted {
		candidates = append(candidates, targets.Absent)
	}
	baseline := ""
	switch imported, had := set.manifest.Imported.Targets[key]; {
	case has && record.Release:
		candidates, baseline = append(candidates, record.Applied), record.Before
	case has:
		candidates = append(candidates, record.Applied, record.Next)
	case had:
		baseline = imported.Before
	}
	if baseline != "" {
		if released, err := inspect(baseline); err == nil {
			candidates = append(candidates, released.Want)
		}
	}
	if restoring && !slices.Contains(candidates, started) {
		copied, err := inspect(set.manifest.Edited[key])
		restoring = set.manifest.Edited[key] != "" && err == nil && copied.Want == started
	}
	switch {
	case live.Fingerprint == live.Want && !restoring:
		item.Action, item.Detail, item.saved = "none", "it holds its value of release v2", live.Want
		return item
	case strings.HasPrefix(slot.Saved, "fp:"):
		item.Action, item.Detail = "manual", "setup has no copy of its value of release v2; Update tools of release v2 repairs it"
		return item
	}
	if restoring {
		candidates = append(candidates, started)
	}
	item.saved = live.Want
	item.Replacement = "restore"
	if slot.Saved == targets.Absent {
		item.Replacement = "remove"
	}
	switch {
	case deleted && live.Fingerprint == targets.Absent:
		item.Action, item.Detail = "changed", "you deleted it after the switch; restore-v2 keeps the deletion unless you choose its replacement"
	case !slices.Contains(candidates, live.Fingerprint):
		item.Action, item.Detail = "changed", "it changed after the switch; restore-v2 keeps it unless you choose its replacement"
	case slot.Saved == targets.Absent:
		item.Action, item.Detail, item.Replacement = "remove", "the switch created it", ""
	default:
		item.Action, item.Detail, item.Replacement = "restore", "it gets its value of release v2", ""
	}
	item.live = live.Fingerprint
	if restoring && (live.Fingerprint == targets.Absent || live.Fingerprint == live.Want) {
		item.live = started
	}
	return item
}

func (r *Restoration) plan() (*restoreSet, error) {
	e := r.o.Engine
	st, err := state.Peek(e.StateDir, e.Target)
	if errors.Is(err, state.ErrSchema1) {
		if _, manifestErr := loadManifest(e.StateDir); manifestErr != nil {
			return nil, errors.New("this installation runs release v2 and has no restore set; nothing to restore")
		}
		r.Done = true
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if st.V2Restore != "switching" && st.V2Restore != "open" {
		return nil, errors.New("the v2 fallback is closed or was never open; restore-v2 is not available. Nothing was changed")
	}
	set, err := openRestoreSet(r.o)
	if err != nil {
		return nil, err
	}
	original, err := r.original(set.manifest.State)
	if err != nil {
		return nil, err
	}
	r.digest, r.Pending = sha256.Sum256(original), slices.Clone(st.Pending)
	r.Items = nil
	for _, key := range slotOrder(set.manifest.Slots) {
		r.Items = append(r.Items, r.classify(set, st, key))
	}
	return set, nil
}

func (r *Restoration) original(saved string) ([]byte, error) {
	_, data, err := LoadState(restoreDir(r.o.Engine.StateDir), r.o.Folders.Home, r.o.Engine.Context)
	if err != nil {
		return nil, errors.Join(errors.New("the restore set has no valid state of release v2; nothing was changed"), err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != saved {
		return nil, errors.New("the state of release v2 in the restore set differs from the state that the switch saved; nothing was changed")
	}
	return data, nil
}

func (r *Restoration) reviewedOriginal(digest [sha256.Size]byte) ([]byte, error) {
	data, err := platform.ReadDocument(filepath.Join(restoreDir(r.o.Engine.StateDir), "state.json"))
	if err != nil || sha256.Sum256(data) != digest {
		return nil, errors.Join(errSavedChanged, err)
	}
	return data, nil
}

func pendingStop(marks []string) (string, error) {
	lines := []string{}
	for _, mark := range marks {
		lines = append(lines, "An earlier step did not report its end: "+mark+". Open setup, confirm that its command exited, then choose the action again.")
	}
	return strings.Join(lines, "\n"), fmt.Errorf("%w: %s", engine.ErrPending, strings.Join(marks, ", "))
}

func clearPending(stateDir, target string) error {
	st, err := state.Peek(stateDir, target)
	if err != nil || len(st.Pending) == 0 {
		return err
	}
	data, err := encodeState(st)
	if err != nil {
		return err
	}
	return platform.SaveDocument(filepath.Join(stateDir, state.File), data)
}

// PreviewRestore plans restore-v2. It only reads. It is available while
// v2_restore is "switching" or "open".
func PreviewRestore(ctx context.Context, o Options) (*Restoration, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := &Restoration{o: o}
	_, err := r.plan()
	r.reviewed = err == nil
	return r, err
}

// Text returns the preview of the restore.
func (r *Restoration) Text() string {
	if r.Done {
		return afterRestore + "\n"
	}
	e := r.o.Engine
	lines := []string{"Restore release v2 (" + SourceCommit + ", " + SourceRelease + ") from " + restoreDir(e.StateDir) + ":"}
	for _, item := range r.Items {
		lines = append(lines, fmt.Sprintf("  %-8s %s: %s", item.Action, item.Key, item.Detail))
	}
	if len(r.Items) == 0 {
		lines = append(lines, "  The switch changed no target.")
	}
	lines = append(lines, "Then setup publishes the saved schema-1 state of release v2.",
		"The private builds under "+filepath.Join(e.StateDir, "tools")+" and the restore set stay. Delete them yourself when you do not need them.")
	return strings.Join(lines, "\n") + "\n"
}

// Changed returns the keys of the targets that changed after the switch.
// Run keeps them unless their key is in its replace list.
func (r *Restoration) Changed() []RestoreItem {
	return slices.DeleteFunc(slices.Clone(r.Items), func(item RestoreItem) bool { return item.Action != "changed" })
}

// Run gives release v2 back. It writes the saved value to each target that
// holds the value of release v3, removes each target that the switch
// created and its empty created folders, and keeps a target that already
// holds its saved value. A target that changed after the switch stays,
// unless replace names its key: then setup saves the changed content first
// and reports where. A replacement applies only to the content that the
// review showed: when that target changed again, Run stops before its first
// write of a saved value. At the end it publishes the saved schema-1 state,
// exactly as the review read it. A pending mark that is not in Confirmed
// stops it before the first write with engine.ErrPending and the report
// text; it clears the confirmed marks before the first write. When a step
// fails, the schema-2 state stays and the same command can run again.
func (r *Restoration) Run(ctx context.Context, replace []string) (message string, resultErr error) {
	e := r.o.Engine
	release, err := platform.Lock(filepath.Join(e.StateDir, state.LockFile))
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	approved, reviewed, digest := slices.Clone(r.Items), r.reviewed && !r.Done, r.digest
	if reviewed {
		if _, err := r.reviewedOriginal(digest); err != nil {
			return "", err
		}
	}
	set, err := r.plan()
	if err != nil {
		return "", err
	}
	if r.Done {
		return afterRestore, nil
	}
	if !reviewed {
		digest = r.digest
	}
	if slices.ContainsFunc(r.Pending, func(mark string) bool { return !slices.Contains(r.Confirmed, mark) }) {
		return pendingStop(r.Pending)
	}
	if reviewed && !slices.EqualFunc(approved, r.Items, func(a, b RestoreItem) bool {
		return a.Key == b.Key && a.Action == b.Action && a.Replacement == b.Replacement && a.live == b.live && a.saved == b.saved
	}) {
		return "", errors.New("the files changed after the review; run restore-v2 again. Nothing was changed")
	}
	if err := clearPending(e.StateDir, e.Target); err != nil {
		return "", err
	}
	failures, err := r.recoverSwitch(ctx, set)
	if err != nil {
		return "", err
	}
	if len(failures) != 0 {
		return "", fmt.Errorf("restore-v2 stopped. An earlier switch run left an unfinished write. Resolve these items and run restore-v2 again:\n  %s", strings.Join(failures, "\n  "))
	}
	if err := hit("restore-plan"); err != nil {
		return "", err
	}
	if set, err = r.plan(); err != nil {
		return "", err
	}
	for _, item := range r.Items {
		if item.Action == "changed" && slices.Contains(replace, item.Key) && !slices.ContainsFunc(approved, func(shown RestoreItem) bool {
			return shown.Key == item.Key && shown.Action == "changed" && shown.live == item.live
		}) {
			return "", fmt.Errorf("the files changed after the review; run restore-v2 again. Setup did not replace %s", item.Key)
		}
	}
	notes, failures, kept := []string{}, []string{}, map[string]string{}
	for _, item := range r.Items {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		slot := set.manifest.Slots[item.Key]
		switch item.Action {
		case "none", "manual", "blocked":
			continue
		case "changed":
			if !slices.Contains(replace, item.Key) {
				kept[item.Key] = item.live
				continue
			}
			if item.live != targets.Absent {
				copied, err := set.saveEdited(ctx, item, slot)
				if err != nil {
					failures = append(failures, item.Key+": setup cannot save your changed content, so it stays: "+err.Error())
					continue
				}
				notes = append(notes, "Saved your changed content of "+item.Key+" at "+copied)
			}
		}
		if err := set.intend(item.Key, item.live); err != nil {
			return "", err
		}
		if err := hit("restore-slot:" + item.Key); err != nil {
			return "", err
		}
		if err := set.verifySaved(item.Key); err != nil {
			failures = append(failures, item.Key+": "+err.Error())
			continue
		}
		record := state.Record{Resource: slot.Resource, Kind: slot.Kind, Applied: item.live, Next: slot.Saved, Release: true, Before: slot.Saved, Made: slices.Clone(slot.Made)}
		err := set.writer.Release(ctx, item.Key, &memory{state.State{Targets: map[string]state.Record{item.Key: record}}}, targets.Preserve)
		switch {
		case errors.Is(err, targets.ErrChanged):
		case err != nil:
			failures = append(failures, item.Key+": "+err.Error())
		default:
			delete(set.manifest.Restoring, item.Key)
			if err := set.save(); err != nil {
				return "", err
			}
		}
	}
	st, err := state.Peek(e.StateDir, e.Target)
	if err != nil {
		return "", err
	}
	for _, key := range slotOrder(set.manifest.Slots) {
		after := r.classify(set, st, key)
		preserved, decided := kept[key]
		switch {
		case after.Action == "none":
		case after.Action == "manual":
			notes = append(notes, "Not restored: "+key+": "+after.Detail)
		case after.Action == "changed" && decided && after.live == preserved:
			notes = append(notes, "Kept your changed content: "+key)
		case !slices.ContainsFunc(failures, func(failure string) bool { return strings.HasPrefix(failure, key+": ") }):
			failures = append(failures, key+": it does not hold its value of release v2: "+after.Detail)
		}
	}
	if len(failures) != 0 {
		return "", fmt.Errorf("restore-v2 stopped before it published the state of release v2. Release v3 stays active. Resolve these items and run restore-v2 again:\n  %s", strings.Join(failures, "\n  "))
	}
	original, err := r.reviewedOriginal(digest)
	if err != nil {
		return "", err
	}
	if err := hit("restore-publish"); err != nil {
		return "", err
	}
	if err := platform.SaveDocument(filepath.Join(e.StateDir, state.File), original); err != nil {
		return "", err
	}
	return strings.Join(append(notes, afterRestore), "\n"), nil
}

func (s *restoreSet) saveEdited(ctx context.Context, item RestoreItem, slot Slot) (string, error) {
	directory := filepath.Join(s.dir, "edited")
	record := state.Record{Resource: slot.Resource, Kind: slot.Kind}
	saved, _, err := s.writer.Capture(ctx, item.Key, record, directory)
	if err != nil {
		return "", err
	}
	path, isBackup := strings.CutPrefix(saved, "backup:")
	if !isBackup {
		encoded, inline := strings.CutPrefix(saved, "value:")
		value, err := base64.StdEncoding.DecodeString(encoded)
		if !inline || err != nil {
			return "", errors.Join(errors.New("this kind of target has no copy"), err)
		}
		path = filepath.Join(directory, "part-"+strings.ToLower(rand.Text()))
		if err := platform.SaveDocument(path, value); err != nil {
			return "", err
		}
		if written, err := platform.ReadDocument(path); err != nil || !bytes.Equal(written, value) {
			return "", errors.Join(errors.New("the copy differs from your content"), err)
		}
	}
	record.Before = saved
	copied, err := s.writer.InspectRecord(item.Key, &memory{state.State{Targets: map[string]state.Record{item.Key: record}}})
	if err != nil || copied.State == targets.StateUnreadable || copied.Want != item.live || copied.Fingerprint != item.live {
		return "", errors.Join(errors.New("the copy differs from your content"), err)
	}
	if s.manifest.Edited == nil {
		s.manifest.Edited = map[string]string{}
	}
	s.manifest.Edited[item.Key] = saved
	return path, nil
}

func (s *restoreSet) intend(key, replaced string) error {
	if s.manifest.Restoring == nil {
		s.manifest.Restoring = map[string]string{}
	}
	s.manifest.Restoring[key] = replaced
	return s.save()
}

func (r *Restoration) recoverSwitch(ctx context.Context, set *restoreSet) ([]string, error) {
	failures := []string{}
	st, err := state.Peek(r.o.Engine.StateDir, r.o.Engine.Target)
	if err != nil {
		return nil, err
	}
	for _, key := range slotOrder(set.manifest.Slots) {
		record, has := st.Targets[key]
		if _, restoring := set.manifest.Restoring[key]; !has || record.Next == "" || restoring {
			continue
		}
		if record.Release {
			if err := set.writer.Recover(ctx, key, &memory{state.State{Targets: map[string]state.Record{key: record}}}); err != nil {
				failures = append(failures, key+": "+err.Error())
			}
			continue
		}
		if err := set.verifySaved(key); err != nil {
			failures = append(failures, key+": "+err.Error())
			continue
		}
		forward := state.Record{Resource: record.Resource, Kind: record.Kind, Next: record.Next, Before: set.manifest.Slots[key].Saved}
		if err := set.writer.Release(ctx, key, &rollback{memory{state.State{Targets: map[string]state.Record{key: forward}}}, set, key}, targets.Preserve); err != nil {
			failures = append(failures, key+": "+err.Error())
			continue
		}
		if _, intended := set.manifest.Restoring[key]; intended {
			delete(set.manifest.Restoring, key)
			if err := set.save(); err != nil {
				return nil, err
			}
		}
	}
	return failures, nil
}

type rollback struct {
	memory
	set *restoreSet
	key string
}

func (r *rollback) Update(edit func(*state.State) error) error {
	if err := r.memory.Update(edit); err != nil {
		return err
	}
	if record := r.st.Targets[r.key]; record.Release {
		return r.set.intend(r.key, record.Applied)
	}
	return nil
}
