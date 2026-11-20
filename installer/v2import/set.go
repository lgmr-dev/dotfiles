package v2import

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

// RestoreFolder is the name of the restore set in the state folder. It holds
// the original schema-1 state, the manifest and the live value of each
// target that the switch changed.
const RestoreFolder = "v2-restore"

// Slot is the saved live value of one target before its first change in the
// switch. Saved is "absent", an inline value, a copy in the restore set, or
// "fp:" and a fingerprint for a unit that setup cannot copy. Made lists what
// the switch created for an absent unit.
type Slot struct {
	Resource string   `json:"resource"`
	Kind     string   `json:"kind"`
	Saved    string   `json:"saved"`
	Made     []string `json:"made,omitempty"`
}

// Manifest describes the restore set. Folder names the subfolder with the
// copies. Imported is the schema-2 state that the import published: a target
// whose record still equals its imported record has no change yet. Deferred
// and Legacy keep the macOS applications that stay on release v2. Restoring
// holds, for each slot that restore-v2 started to give back, the fingerprint
// of the value that it replaces: a later run of restore-v2 completes that
// slot also when the first run stopped in the middle of its write.
type Manifest struct {
	Schema    int               `json:"schema"`
	Source    string            `json:"source"`
	Folder    string            `json:"folder"`
	Deferred  []string          `json:"deferred,omitempty"`
	Legacy    []string          `json:"legacy,omitempty"`
	Imported  state.State       `json:"imported"`
	Slots     map[string]Slot   `json:"slots"`
	Restoring map[string]string `json:"restoring,omitempty"`
}

type restoreSet struct {
	mu       sync.Mutex
	dir      string
	tools    string
	writer   *targets.Writer
	manifest Manifest
}

func restoreDir(stateDir string) string { return filepath.Join(stateDir, RestoreFolder) }

func loadManifest(stateDir string) (Manifest, error) {
	var manifest Manifest
	data, err := platform.ReadDocument(filepath.Join(restoreDir(stateDir), "manifest.json"))
	if err != nil {
		return manifest, err
	}
	if err := platform.Decode(data, &manifest); err != nil {
		return manifest, err
	}
	if manifest.Schema != 1 || manifest.Source != SourceCommit || !filepath.IsLocal(manifest.Folder) || filepath.Base(manifest.Folder) != manifest.Folder {
		return manifest, errors.New("the manifest of the restore set is invalid")
	}
	if manifest.Slots == nil {
		manifest.Slots = map[string]Slot{}
	}
	return manifest, nil
}

func (s *restoreSet) save() error {
	data, err := json.MarshalIndent(s.manifest, "", "  ")
	if err != nil {
		return err
	}
	return platform.SaveDocument(filepath.Join(s.dir, "manifest.json"), append(data, '\n'))
}

func openRestoreSet(o Options) (*restoreSet, error) {
	manifest, err := loadManifest(o.Engine.StateDir)
	if err != nil {
		return nil, fmt.Errorf("cannot read the restore set %s: %w", restoreDir(o.Engine.StateDir), err)
	}
	return &restoreSet{dir: restoreDir(o.Engine.StateDir), tools: filepath.Join(o.Engine.StateDir, "tools"), writer: o.writer(), manifest: manifest}, nil
}

func createRestoreSet(o Options, mapping Mapping, imported state.State) error {
	dir := restoreDir(o.Engine.StateDir)
	if err := platform.PrepareDirectory(dir); err != nil {
		return err
	}
	if err := platform.SaveDocument(filepath.Join(dir, "state.json"), mapping.Original); err != nil {
		return err
	}
	set := &restoreSet{dir: dir, manifest: Manifest{Schema: 1, Source: SourceCommit, Folder: "saved-" + strings.ToLower(rand.Text()),
		Deferred: mapping.Deferred, Legacy: mapping.Legacy, Imported: imported, Slots: map[string]Slot{}}}
	return set.save()
}

func sameRecord(a, b state.Record) bool {
	return a.Resource == b.Resource && a.Kind == b.Kind && a.Applied == b.Applied && a.Next == b.Next && a.Release == b.Release && a.Before == b.Before && slices.Equal(a.Made, b.Made)
}

func changedTargets(before, after state.State) []string {
	keys := []string{}
	for key := range after.Targets {
		if earlier, had := before.Targets[key]; !had || !sameRecord(earlier, after.Targets[key]) {
			keys = append(keys, key)
		}
	}
	for key := range before.Targets {
		if _, has := after.Targets[key]; !has {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

func (s *restoreSet) capture(ctx context.Context, key string, record state.Record, before state.State) error {
	if within(s.tools, key) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	imported, wasImported := s.manifest.Imported.Targets[key]
	earlier, had := before.Targets[key]
	untouched := had == wasImported && (!had || sameRecord(earlier, imported))
	if _, saved := s.manifest.Slots[key]; saved && !untouched {
		return nil
	}
	if !untouched {
		return fmt.Errorf("%s changed in an earlier run and is not in the restore set %s; examine it, then run restore-v2 or setup again", key, s.dir)
	}
	saved, made, err := s.writer.Capture(ctx, key, record, filepath.Join(s.dir, s.manifest.Folder))
	if err != nil {
		return fmt.Errorf("cannot save %s for restore-v2; nothing was changed there: %w", key, err)
	}
	s.manifest.Slots[key] = Slot{Resource: record.Resource, Kind: record.Kind, Saved: saved, Made: made}
	return s.save()
}

type captureRecords struct {
	engine.Records
	ctx context.Context
	set *restoreSet
}

func (c captureRecords) Update(edit func(*state.State) error) error {
	return c.Records.Update(func(s *state.State) error {
		before := s.Clone()
		if err := edit(s); err != nil {
			return err
		}
		for _, key := range changedTargets(before, *s) {
			record, has := s.Targets[key]
			if !has {
				record = before.Targets[key]
			}
			if err := c.set.capture(c.ctx, key, record, before); err != nil {
				return err
			}
		}
		return nil
	})
}

type captureProvider struct {
	engine.Provider
	set *restoreSet
}

func (c captureProvider) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	return c.Provider.Apply(ctx, r, approved, captureRecords{rec, ctx, c.set})
}

func (c captureProvider) Remove(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	return c.Provider.Remove(ctx, r, approved, captureRecords{rec, ctx, c.set})
}

func (s *restoreSet) router(inner engine.Router) engine.Router {
	return func(r catalog.Resource) (engine.Provider, error) {
		provider, err := inner(r)
		if err != nil || provider == nil {
			return provider, err
		}
		return captureProvider{provider, s}, nil
	}
}

type deferCatalog struct {
	engine.Catalog
	deferred []string
}

func (c deferCatalog) Closure(roots []string, ctx platform.Context) ([]string, error) {
	full, err := c.Catalog.Closure(roots, ctx)
	return slices.DeleteFunc(full, func(id string) bool { return slices.Contains(c.deferred, id) }), err
}

func (o Options) switchEngine(set *restoreSet) *engine.Engine {
	e := *o.Engine
	e.Catalog = deferCatalog{o.Engine.Catalog, set.manifest.Deferred}
	e.Router = set.router(o.Engine.Router)
	e.Switch, e.UI = true, nil
	return &e
}

func existingV2Folders(stateDir string) []string {
	folders := []string{}
	for _, name := range engine.V2Folders {
		if name == RestoreFolder {
			continue
		}
		if _, err := os.Lstat(filepath.Join(stateDir, name)); err == nil {
			folders = append(folders, filepath.Join(stateDir, name))
		}
	}
	return folders
}

func slotOrder(slots map[string]Slot) []string {
	keys := slices.Sorted(maps.Keys(slots))
	slices.SortStableFunc(keys, func(a, b string) int { return len(slots[a].Made) - len(slots[b].Made) })
	return keys
}
