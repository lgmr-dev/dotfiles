package v2import

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

// RestoreItem is the plan for one saved target. Action is "none" for a
// target that already holds its saved value, "restore" for a target that
// holds the value of release v3 and gets its saved value, "remove" for a
// target that the switch created, "changed" for a target that you edited
// after the switch, and "manual" for a target that setup cannot give back.
type RestoreItem struct {
	Key, Resource, Action, Detail string

	live string
}

// Restoration is the preview of restore-v2. Done is true when the schema-1
// state is active again and nothing is left to do.
type Restoration struct {
	Items []RestoreItem
	Done  bool

	o Options
}

const afterRestore = "Release v2 is active again. Check out " + SourceCommit + " (" + SourceRelease + ") and run its setup."

func (r *Restoration) classify(set *restoreSet, st state.State, key string) RestoreItem {
	slot := set.manifest.Slots[key]
	item := RestoreItem{Key: key, Resource: slot.Resource, Action: "manual"}
	inspect := func(before string) (targets.Live, error) {
		live, err := set.writer.InspectRecord(key, &memory{state.State{Targets: map[string]state.Record{key: {Resource: slot.Resource, Kind: slot.Kind, Before: before}}}})
		if err == nil && live.State == targets.StateUnreadable {
			err = errors.New(live.Detail)
		}
		return live, err
	}
	live, err := inspect(slot.Saved)
	switch {
	case err != nil:
		item.Detail = "setup cannot compare it with its saved value: " + err.Error()
		return item
	case live.Fingerprint == live.Want:
		item.Action, item.Detail = "none", "it holds its value of release v2"
		return item
	case strings.HasPrefix(slot.Saved, "fp:"):
		item.Detail = "setup has no copy of its value of release v2; Update tools of release v2 repairs it"
		return item
	}
	candidates := []string{targets.Absent}
	if record, has := st.Targets[key]; has {
		candidates = append(candidates, record.Applied, record.Next)
	} else if imported, had := set.manifest.Imported.Targets[key]; had && imported.Before != "" {
		if released, err := inspect(imported.Before); err == nil {
			candidates = append(candidates, released.Want)
		}
	}
	switch {
	case !slices.Contains(candidates, live.Fingerprint):
		item.Action, item.Detail = "changed", "it changed after the switch; restore-v2 keeps it unless you choose its replacement"
	case slot.Saved == targets.Absent:
		item.Action, item.Detail = "remove", "the switch created it"
	default:
		item.Action, item.Detail = "restore", "it gets its value of release v2"
	}
	item.live = live.Fingerprint
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
	if _, err := r.original(); err != nil {
		return nil, err
	}
	r.Items = nil
	for _, key := range slotOrder(set.manifest.Slots) {
		r.Items = append(r.Items, r.classify(set, st, key))
	}
	return set, nil
}

func (r *Restoration) original() ([]byte, error) {
	data, err := platform.ReadDocument(filepath.Join(restoreDir(r.o.Engine.StateDir), "state.json"))
	var header struct {
		Schema int `json:"schema"`
	}
	if err == nil {
		err = json.Unmarshal(data, &header)
	}
	if err != nil || header.Schema != 1 {
		return nil, errors.Join(errors.New("the restore set has no readable state of release v2; nothing was changed"), err)
	}
	return data, nil
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
// and reports where. At the end it publishes the saved schema-1 state. When
// a step fails, the schema-2 state stays and the same command can run
// again.
func (r *Restoration) Run(ctx context.Context, replace []string) (message string, resultErr error) {
	e := r.o.Engine
	release, err := platform.Lock(filepath.Join(e.StateDir, state.LockFile))
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	set, err := r.plan()
	if err != nil {
		return "", err
	}
	if r.Done {
		return afterRestore, nil
	}
	notes, failures := []string{}, []string{}
	for _, item := range r.Items {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		slot := set.manifest.Slots[item.Key]
		live, choice := item.live, targets.Preserve
		switch item.Action {
		case "none":
			continue
		case "manual":
			notes = append(notes, "Not restored: "+item.Key+": "+item.Detail)
			continue
		case "changed":
			if !slices.Contains(replace, item.Key) {
				notes = append(notes, "Kept your changed content: "+item.Key)
				continue
			}
			choice, live = targets.Replace, "edited after the switch"
		}
		record := state.Record{Resource: slot.Resource, Kind: slot.Kind, Applied: live, Before: slot.Saved, Made: slices.Clone(slot.Made)}
		err := set.writer.Release(ctx, item.Key, &memory{state.State{Targets: map[string]state.Record{item.Key: record}}}, choice)
		switch {
		case errors.Is(err, targets.ErrChanged):
			notes = append(notes, "Kept your changed content: "+item.Key)
		case err != nil:
			failures = append(failures, item.Key+": "+err.Error())
		}
	}
	if len(failures) != 0 {
		return "", fmt.Errorf("restore-v2 stopped before it published the state of release v2. Release v3 stays active; resolve these items and run restore-v2 again:\n  %s", strings.Join(failures, "\n  "))
	}
	original, err := r.original()
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
