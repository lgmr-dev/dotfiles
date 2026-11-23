package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/state"
	"github.com/lgmr-dev/dotfiles/installer/targets"
)

const (
	profileShells   = "integration.shells"
	profileTmux     = "tmux.plugins"
	profileGit      = "integration.git"
	profileVSCode   = "integration.vscode-managed"
	profileKiro     = "integration.kiro"
	profileT3       = "integration.t3-code"
	profileT3Theme  = "integration.t3-code-theme"
	profileTerminal = "integration.windows-terminal"
)

// Profiles is the engine.Provider of the profile resources: the shell
// loaders and their generated init files, the tmux plugins block, the Git
// include block, and the managed fields of the VS Code, Kiro CLI, T3 Code and
// Windows Terminal settings. It owns marked blocks, named JSON fields and its
// generated files only: each other byte of a personal file stays.
//
// Checkout is the physical path of the repository. StateDir is the installer
// state folder. Writer needs the kinds of ProfileKinds. Pins holds the
// effective archive pin of each resource of the host. KiroHome is the value
// of KIRO_HOME. Homebrew returns the Homebrew command folder, or an empty
// text without Homebrew. Wanted returns the closure of the selected and kept
// resources of the run: the generated shell content and the Git block depend
// on it. The wiring sets Wanted before a run; without it the selection is
// empty. VSCodeCask reports, on macOS, that the VS Code application of the
// Homebrew cask verifies: that application updates itself, so setup releases
// the update.mode field. While the field has a record and the cask application
// does not verify, a VS Code payload of release v2 is the active application,
// and the field stays managed. Without VSCodeCask the field is released.
// LegacyCommandDirectories are the command folders of the payloads of release
// v2 that stay active after the import, such as the macOS applications that
// move to a cask later: the shell profile keeps them on PATH, after the
// managed folders of this release.
type Profiles struct {
	Catalog  *catalog.Catalog
	Context  platform.Context
	Folders  platform.Folders
	Checkout string
	StateDir string
	Writer   *targets.Writer
	Pins     map[string]catalog.ArchivePin
	KiroHome string
	Homebrew func() string
	Wanted   func() []string

	VSCodeCask func(st state.State) bool

	LegacyCommandDirectories []string
}

// ProfileResource reports that Profiles is the provider of the resource.
func ProfileResource(id string) bool {
	return slices.Contains([]string{profileShells, profileTmux, profileGit, profileVSCode, profileKiro, profileT3, profileT3Theme, profileTerminal}, id)
}

// ProfileKinds returns the kinds that the profile resources add to a
// targets.Writer. The field kind accepts comments and trailing commas in the
// VS Code user settings. The Git block goes to the start of the Git
// configuration, which must be UTF-8. The slot TerminalSlot is the projection
// of the reviewed Windows Terminal settings.
func ProfileKinds() map[string]targets.Kind {
	return map[string]targets.Kind{
		"field":                 targets.SlotKind("field", targets.Field{JSONC: vscodeSettingsFile}),
		"block#" + profileGit:   targets.SlotKind("block", gitSlot{targets.Block{Top: true}}),
		"field#" + TerminalSlot: targets.SlotKind("field", terminalSlot{}),
	}
}

type profileField struct{ name, value string }

func profileFields(path string, fields []profileField) []targets.Desired {
	units := make([]targets.Desired, len(fields))
	for i, field := range fields {
		units[i] = targets.Desired{Kind: "field", Path: path, Slot: field.name, Content: []byte(field.value)}
	}
	return units
}

func profileRead(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("profile is not a regular file; preserve its redirect and use explicit migration")
	}
	if info.Size() > platform.MaxProfileBytes {
		return nil, errors.New("profile exceeds its read bound")
	}
	return os.ReadFile(path)
}

func (p *Profiles) wanted() []string {
	if p.Wanted == nil {
		return nil
	}
	return p.Wanted()
}

func (p *Profiles) recorded(st state.State, id string) []string {
	keys := []string{}
	for _, key := range slices.Sorted(maps.Keys(st.Targets)) {
		if st.Targets[key].Resource == id {
			keys = append(keys, key)
		}
	}
	slices.SortStableFunc(keys, func(a, b string) int {
		return len(st.Targets[a].Made) - len(st.Targets[b].Made)
	})
	return keys
}

func (p *Profiles) stale(st state.State, id string, units []targets.Desired) []string {
	return slices.DeleteFunc(p.recorded(st, id), func(key string) bool {
		return slices.ContainsFunc(units, func(d targets.Desired) bool { return targets.Key(d.Path, d.Slot) == key })
	})
}

func (p *Profiles) samePath(a, b string) bool {
	return a == b || p.Context.OS == "windows" && strings.EqualFold(a, b)
}

func (p *Profiles) moved(st state.State, id string, units []targets.Desired) string {
	for _, key := range p.recorded(st, id) {
		if !slices.ContainsFunc(units, func(d targets.Desired) bool { return p.samePath(targets.Key(d.Path, d.Slot), key) }) {
			return strings.TrimSuffix(key, "#"+units[0].Slot)
		}
	}
	return ""
}

func (p *Profiles) plan(id string, st state.State) ([]targets.Desired, string, error) {
	if p.Catalog == nil || p.Writer == nil || !filepath.IsAbs(p.StateDir) || !filepath.IsAbs(p.Folders.Home) || !filepath.IsAbs(p.Checkout) {
		return nil, "", errors.New("the profile provider requires the catalog, the writer and the absolute home, checkout and state folders")
	}
	var units []targets.Desired
	var err error
	blocked := ""
	switch id {
	case profileShells:
		units, err = p.shellUnits(p.wanted(), st)
	case profileTmux:
		units, err = p.tmuxUnits()
	case profileGit:
		units, blocked, err = p.gitUnits(p.wanted())
	case profileVSCode:
		units = p.vscodeUnits(st)
	case profileKiro:
		units = p.kiroUnits()
	case profileT3:
		units = p.t3Units()
	case profileT3Theme:
		units, err = p.t3ThemeUnits()
	case profileTerminal:
		units, err = p.terminalUnits()
	default:
		err = fmt.Errorf("%s is not a profile resource", id)
	}
	if err != nil {
		return nil, "", err
	}
	seen := map[string]bool{}
	for i := range units {
		if units[i].Path, err = platform.BindDestination(units[i].Path); err != nil {
			return nil, "", err
		}
		for _, key := range p.recorded(st, id) {
			if saved := strings.TrimSuffix(key, "#"+units[i].Slot); saved != key && p.samePath(saved, units[i].Path) {
				units[i].Path = saved
			}
		}
		units[i].Resource = id
		units[i].Key = targets.Key(units[i].Path, units[i].Slot)
		if seen[units[i].Key] {
			return nil, "", errors.New("invalid or duplicate profile path")
		}
		seen[units[i].Key] = true
	}
	switch id {
	case profileGit:
		issues := []string{blocked, gitCheckoutOverlap(p.Checkout, units[0].Path)}
		if p.moved(st, id, units) != "" {
			issues = append(issues, "Git's global configuration path changed. Remove the saved Git integration through setup before selecting Git at the new path.")
		}
		blocked = strings.Join(slices.DeleteFunc(issues, func(issue string) bool { return issue == "" }), " ")
	case profileKiro:
		if saved := p.moved(st, id, units); saved != "" {
			blocked = fmt.Sprintf("saved Kiro CLI settings target %s is not the current location %s; run setup with the KIRO_HOME used when it was installed to update or remove it", saved, units[0].Path)
		}
	}
	return units, blocked, nil
}

// Desired returns the units of a profile resource for the current selection,
// with physical paths, in write order. It reads the records to keep the Bash
// login file that an earlier run chose. It writes nothing.
func (p *Profiles) Desired(id string, rec engine.Records) ([]targets.Desired, error) {
	units, _, err := p.plan(id, rec.State())
	return units, err
}

func profileToken(rows [][]string) string {
	encoded, _ := json.Marshal(rows)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// Inspect implements engine.Provider. It only reads. For a wanted resource
// it reports Satisfied when each unit matches its record and the content of
// the selection, Changed when a unit holds other content, Blocked when a
// source or a discovered path stops the write, and Missing or Unhealthy when
// a write is necessary. For a released resource it reports Absent without a
// record, Changed for a recorded unit that holds other content, and Satisfied
// when the release can run. Preserved lists the changed units. Token covers
// the observed fingerprint of each unit. On macOS the VS Code resource adds
// the note "update.mode: none" while the live setting has that value, because
// the application then does not update itself.
func (p *Profiles) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	if err := ctx.Err(); err != nil {
		return engine.Finding{}, err
	}
	if p.Writer == nil {
		return engine.Finding{}, errors.New("the profile provider requires the writer")
	}
	st := rec.State()
	rows := [][]string{{fmt.Sprint(want.Present)}}
	unreadable, changed := []string{}, []string{}
	work, owned := 0, 0
	released := func(key string) error {
		live, err := p.Writer.InspectRecord(key, rec)
		if err != nil {
			return err
		}
		rows = append(rows, []string{key, live.State, live.Fingerprint, live.Want})
		switch {
		case live.State == targets.StateUnreadable:
			unreadable = append(unreadable, live.Detail)
		case live.State == targets.StateChanged && st.Targets[key].Applied != "":
			changed = append(changed, key)
		}
		work++
		return nil
	}
	var notes []string
	if r.ID == profileVSCode {
		notes = p.vscodeNotes()
	}
	if !want.Present {
		for _, key := range p.recorded(st, r.ID) {
			if err := released(key); err != nil {
				return engine.Finding{}, err
			}
		}
		finding := engine.Finding{Status: engine.Satisfied, Notes: notes, Token: profileToken(rows)}
		switch {
		case work == 0:
			finding.Status = engine.Absent
		case len(unreadable) != 0:
			finding.Status, finding.Reason = engine.Unknown, strings.Join(unreadable, "; ")
		case len(changed) != 0:
			finding.Status, finding.Preserved = engine.Changed, changed
			finding.Reason = "content changed after setup wrote it: " + strings.Join(changed, ", ")
		}
		return finding, nil
	}
	units, blocked, err := p.plan(r.ID, st)
	if err != nil {
		return engine.Finding{}, err
	}
	if blocked != "" {
		return engine.Finding{Status: engine.Blocked, Reason: blocked, Notes: notes, Token: profileToken(append(rows, []string{blocked}))}, nil
	}
	for _, d := range units {
		live, err := p.Writer.Inspect(d, rec)
		if err != nil {
			return engine.Finding{}, err
		}
		rows = append(rows, []string{d.Key, d.Kind, live.State, live.Fingerprint, live.Want})
		record, has := st.Targets[d.Key]
		if has && record.Applied != "" {
			owned++
		}
		switch {
		case live.State == targets.StateUnreadable:
			unreadable = append(unreadable, live.Detail)
		case live.State == targets.StateChanged && live.Fingerprint != live.Want:
			changed = append(changed, d.Key)
		case live.Satisfied && record.Kind == d.Kind:
		default:
			work++
		}
	}
	for _, key := range p.stale(st, r.ID, units) {
		owned++
		if err := released(key); err != nil {
			return engine.Finding{}, err
		}
	}
	finding := engine.Finding{Status: engine.Satisfied, Notes: notes, Token: profileToken(rows)}
	switch {
	case len(unreadable) != 0:
		finding.Status, finding.Reason = engine.Unknown, strings.Join(unreadable, "; ")
	case len(changed) != 0:
		finding.Status, finding.Preserved = engine.Changed, changed
		finding.Reason = "existing content differs from the selected profile content: " + strings.Join(changed, ", ")
	case work != 0 && owned == 0:
		finding.Status = engine.Missing
	case work != 0:
		finding.Status, finding.Reason = engine.Unhealthy, "a profile block, field or generated file needs the current content"
	}
	return finding, nil
}

func profileChoice(approved engine.Finding) targets.Choice {
	if approved.Replace {
		return targets.Replace
	}
	return targets.Preserve
}

func profileChange(err error) error {
	if errors.Is(err, targets.ErrChanged) {
		return fmt.Errorf("profile block changed after approval: %w", err)
	}
	return err
}

func profileSweep(before state.State, keys []string, rec engine.Records) {
	after := rec.State()
	for _, key := range keys {
		if _, kept := after.Targets[key]; !kept {
			platform.RemoveCreatedDirectories(before.Targets[key].Made)
		}
	}
}

func (p *Profiles) lock(st state.State, keys []string, units []targets.Desired) (func() error, error) {
	paths := []string{}
	add := func(path string) {
		if !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	for _, d := range units {
		if d.Kind == "field" || d.Resource == profileGit {
			add(d.Path)
		}
	}
	for _, key := range keys {
		record := st.Targets[key]
		if i := strings.LastIndexByte(key, '#'); i > 0 && (record.Kind == "field" || record.Resource == profileGit) {
			add(key[:i])
		}
	}
	return settingsLocks(p.StateDir, paths)
}

// Apply implements engine.Provider. It releases each recorded unit that the
// resource no longer has, such as the VS Code update.mode field on macOS, and
// then converges each unit through the write order. With approved.Replace it
// saves changed content first and replaces it. For Windows Terminal settings
// that hold no reviewed value it records the startup profile and the
// containers as the baseline first, so a removal restores them. While it
// writes a settings file or the Git configuration it holds the "<file>.lock"
// folder, which stops a write of the application at the same time.
func (p *Profiles) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) (result error) {
	st := rec.State()
	units, blocked, err := p.plan(r.ID, st)
	if err != nil {
		return err
	}
	if blocked != "" {
		return errors.New(blocked)
	}
	stale := p.stale(st, r.ID, units)
	release, err := p.lock(st, stale, units)
	if err != nil {
		return err
	}
	defer func() {
		result = errors.Join(result, release())
		profileSweep(st, stale, rec)
	}()
	choice := profileChoice(approved)
	for _, key := range stale {
		if err := p.Writer.Release(ctx, key, rec, choice); err != nil {
			return profileChange(err)
		}
	}
	for _, d := range units {
		if d.Slot == TerminalSlot {
			if err := terminalBaseline(d, rec); err != nil {
				return err
			}
		}
		if err := p.Writer.Converge(ctx, d, rec, choice); err != nil {
			return profileChange(err)
		}
	}
	return nil
}

// Remove implements engine.Provider. It releases each recorded unit of the
// resource through the write order, which restores the first baseline. It
// does not need the checkout or the selection.
func (p *Profiles) Remove(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) (result error) {
	if p.Writer == nil {
		return errors.New("the profile provider requires the writer")
	}
	st := rec.State()
	keys := p.recorded(st, r.ID)
	release, err := p.lock(st, keys, nil)
	if err != nil {
		return err
	}
	defer func() {
		result = errors.Join(result, release())
		profileSweep(st, keys, rec)
	}()
	for _, key := range keys {
		if err := p.Writer.Release(ctx, key, rec, profileChoice(approved)); err != nil {
			return profileChange(err)
		}
	}
	return nil
}
