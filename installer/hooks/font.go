package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/builds"
	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

// FontResource is the resource of the Fonts provider. FontArchive is the
// resource of the private Hack Nerd Font archive. FontSlot is the slot of
// the record of a native font registration: the record has the kind font and
// the key of the font file with this slot.
const (
	FontResource = "tool.font"
	FontArchive  = "font.hack"
	FontSlot     = "registration"
)

const (
	fontDestinationBlocked  = "preexisting font destination is retained; choose another font installation or resolve this exact collision"
	fontRegistrationBlocked = "a preexisting or changed font registration is retained; preserve it and resolve that exact value before installing"
)

// Fonts is the engine.Provider of FontResource. It owns only the twelve
// faces of Hack Nerd Font and their per-user registrations. Each font file
// is a file target, copied from the private archive build. Each native
// registration is a separate record of the kind font: setup publishes its
// intended value before the activation and promotes it after the native
// font engine selects the face with its Nerd Font glyph, so a failed
// activation leaves the record unfinished. Faces that are present without a
// record satisfy the resource and get no file. A destination or a
// registration value that setup did not record stays and blocks the install.
//
// The files go to ~/Library/Fonts on macOS, to the fonts folder of the data
// folder on Linux and to %LOCALAPPDATA%\Microsoft\Windows\Fonts on Windows,
// where the registrations are values of the current-user Fonts key.
//
// StateDir is the installer state folder. Pins holds the effective archive
// pin of each resource of the host. PowerShell is Windows PowerShell.
// FontCache and FontMatch are fc-cache and fc-match on Linux. Runner runs
// the activation. Query runs the read-only inspection; nil uses FontQuery.
type Fonts struct {
	Context                          platform.Context
	Folders                          platform.Folders
	StateDir                         string
	Pins                             map[string]catalog.ArchivePin
	PowerShell, FontCache, FontMatch string
	Runner                           platform.Runner
	Query                            func(context.Context, platform.Command) ([]byte, error)
}

// Kinds returns the kind that the font registrations add to a
// targets.Writer.
func (p *Fonts) Kinds() map[string]targets.Kind {
	return map[string]targets.Kind{"font": fontKind{fonts: p}}
}

func (p *Fonts) writer(probe []fontNativeFace) *targets.Writer {
	return &targets.Writer{StateDir: p.StateDir, Kinds: map[string]targets.Kind{"font": fontKind{fonts: p, probe: probe}}}
}

func (p *Fonts) paths(r catalog.Resource) ([]string, error) {
	if r.ID != FontResource || !filepath.IsAbs(p.StateDir) || filepath.Clean(p.StateDir) != p.StateDir {
		return nil, errors.New("unknown fixed font integration")
	}
	return p.destinations()
}

func (p *Fonts) destinations() ([]string, error) {
	folder := ""
	switch p.Context.OS {
	case "darwin":
		folder = filepath.Join(p.Folders.Home, "Library", "Fonts")
	case "linux":
		folder = p.Folders.Data
		if folder == "" {
			folder = filepath.Join(p.Folders.Home, ".local", "share")
		}
		folder = filepath.Join(folder, "fonts")
	case "windows":
		if !filepath.IsAbs(p.Folders.LocalAppData) {
			return nil, errors.New("font integration requires actual LocalAppData")
		}
		folder = filepath.Join(p.Folders.LocalAppData, "Microsoft", "Windows", "Fonts")
	default:
		return nil, errors.New("unsupported font platform")
	}
	if !filepath.IsAbs(folder) {
		return nil, errors.New("font integration requires the home folder")
	}
	paths := []string{}
	for _, face := range fontFaces() {
		name := "Dotfiles-" + face.File
		if p.Context.OS == "linux" {
			name = filepath.Join("Dotfiles-HackNerdFont", face.File)
		}
		path, err := platform.BindDestination(filepath.Join(folder, name))
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func (p *Fonts) sources() (sources, hashes []string, waiting string, err error) {
	pin, ok := p.Pins[FontArchive]
	if !ok {
		return nil, nil, "", fmt.Errorf("%s has no archive pin for this system", FontArchive)
	}
	build, err := builds.BuildPath(p.StateDir, FontArchive)
	if err != nil {
		return nil, nil, "the Hack Nerd Font archive is not installed: " + err.Error(), nil
	}
	for _, face := range fontFaces() {
		if !slices.Contains(pin.RequiredFiles, face.File) {
			return nil, nil, "", fmt.Errorf("the archive pin of %s does not declare the file %s", FontArchive, face.File)
		}
		source := filepath.Join(build, filepath.FromSlash(face.File))
		hash, err := fontFileHash(source)
		if err != nil {
			return nil, nil, "the Hack Nerd Font archive is not complete: " + err.Error(), nil
		}
		sources, hashes = append(sources, source), append(hashes, hash)
	}
	return sources, hashes, "", nil
}

type fontRegistration struct {
	Registration, Name, Hash string
	Glyph                    bool
}

func fontFingerprint(value fontRegistration) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return "font:" + hex.EncodeToString(sum[:])
}

func (p *Fonts) live(face fontFace, probe fontNativeFace) string {
	if p.Context.OS == "windows" {
		if probe.Registration == "" {
			return targets.Absent
		}
		return fontFingerprint(fontRegistration{probe.Registration, probe.Name, probe.Hash, probe.Glyph})
	}
	if probe.Name != face.PostScript || probe.Hash == "" {
		return targets.Absent
	}
	return fontFingerprint(fontRegistration{"", probe.Name, probe.Hash, probe.Glyph})
}

func (p *Fonts) want(face fontFace, path, hash string) string {
	registration := ""
	if p.Context.OS == "windows" {
		registration = path
	}
	return fontFingerprint(fontRegistration{registration, face.PostScript, hash, true})
}

func fontHealthy(probe []fontNativeFace) bool {
	faces := fontFaces()
	if len(probe) != len(faces) {
		return false
	}
	for i, face := range faces {
		if probe[i].Name != face.PostScript || !probe[i].Glyph || len(probe[i].Hash) != 64 || strings.Trim(probe[i].Hash, "0123456789abcdef") != "" {
			return false
		}
	}
	return true
}

type fontKind struct {
	fonts *Fonts
	probe []fontNativeFace
}

func (k fontKind) face(d targets.Desired) (int, []string, error) {
	paths, err := k.fonts.destinations()
	if err != nil {
		return 0, nil, err
	}
	i := slices.Index(paths, d.Path)
	if i < 0 || d.Slot != FontSlot {
		return 0, nil, fmt.Errorf("%s is not a managed font face", d.Path)
	}
	return i, paths, nil
}

func (k fontKind) Slotted() bool { return true }

func (k fontKind) Observe(d targets.Desired) (string, error) {
	i, paths, err := k.face(d)
	if err != nil {
		return "", err
	}
	probe := k.probe
	if probe == nil {
		if probe, err = k.fonts.query(context.Background(), paths); err != nil {
			return "", err
		}
	}
	return k.fonts.live(fontFaces()[i], probe[i]), nil
}

func (k fontKind) Fingerprint(d targets.Desired) (string, error) {
	i, _, err := k.face(d)
	if err != nil {
		return "", err
	}
	if len(d.Content) != 64 {
		return "", errors.New("a font registration needs the SHA-256 of its font file")
	}
	return k.fonts.want(fontFaces()[i], d.Path, string(d.Content)), nil
}

func (k fontKind) Baseline(before string) (string, error) {
	if before == targets.Absent {
		return targets.Absent, nil
	}
	if fingerprint, ok := strings.CutPrefix(before, "fp:"); ok {
		return fingerprint, nil
	}
	return "", errors.New("the baseline of a font registration must be absence or a fingerprint")
}

func (k fontKind) Recover(context.Context, targets.Desired, state.Record) error { return nil }

func (k fontKind) CannotCopy() bool { return true }

func (k fontKind) Save(_ context.Context, d targets.Desired, _ string) (string, error) {
	return "", fmt.Errorf("setup makes no copy of the font registration of %s; resolve that exact value yourself", d.Path)
}

func (k fontKind) change(ctx context.Context, d targets.Desired, install bool) error {
	i, paths, err := k.face(d)
	if err != nil {
		return err
	}
	probe, err := k.fonts.query(ctx, paths)
	if err != nil {
		return err
	}
	return k.fonts.activate(ctx, []int{i}, paths, fontRegistrations(probe), install)
}

func (k fontKind) Write(ctx context.Context, d targets.Desired, _ string) error {
	return k.change(ctx, d, true)
}

func (k fontKind) Restore(ctx context.Context, d targets.Desired, record state.Record, _ string) error {
	if record.Before != "" && record.Before != targets.Absent {
		return fmt.Errorf("setup has no copy of the earlier font registration of %s", d.Path)
	}
	return k.change(ctx, d, false)
}

func (k fontKind) Discard(string, string) error { return nil }

func fontRegistrations(probe []fontNativeFace) []string {
	values := make([]string, len(probe))
	for i := range probe {
		values[i] = probe[i].Registration
	}
	return values
}

type fontObservation struct {
	paths    []string
	probe    []fontNativeFace
	units    []targets.Desired
	hashes   []string
	waiting  string
	blocked  string
	reused   bool
	work     string
	foreign  []string
	recorded []string
}

func (p *Fonts) observe(ctx context.Context, r catalog.Resource, rec engine.Records) (fontObservation, error) {
	var o fontObservation
	var err error
	if o.paths, err = p.paths(r); err != nil {
		return o, err
	}
	if o.probe, err = p.query(ctx, o.paths); err != nil {
		return o, err
	}
	st := rec.State()
	o.recorded = desktopRecorded(st, r.ID)
	faces := fontFaces()
	for i, value := range fontRegistrations(o.probe) {
		_, has := st.Targets[targets.Key(o.paths[i], FontSlot)]
		if value != "" && (value != o.paths[i] || !has) {
			o.foreign = append(o.foreign, "current-user font registration: "+faces[i].File)
		}
	}
	if len(o.recorded) == 0 && fontHealthy(o.probe) && len(o.foreign) == 0 {
		o.reused = true
		return o, nil
	}
	if len(o.foreign) != 0 {
		o.blocked = fontRegistrationBlocked
	}
	for _, path := range o.paths {
		if _, has := st.Targets[path]; has {
			continue
		}
		live, err := platform.SnapshotTree(path)
		if err != nil {
			return o, err
		}
		if live.Kind != "absent" && o.blocked == "" {
			o.blocked = fontDestinationBlocked
		}
	}
	sources, hashes, waiting, err := p.sources()
	if err != nil {
		return o, err
	}
	o.waiting, o.hashes = waiting, hashes
	if waiting != "" {
		return o, nil
	}
	for i, path := range o.paths {
		o.units = append(o.units, targets.Desired{Resource: r.ID, Kind: "file", Path: path, Tree: sources[i]})
		record := st.Targets[targets.Key(path, FontSlot)]
		want := p.want(faces[i], path, hashes[i])
		switch {
		case record.Next != "" || record.Release:
			o.work = "font publication or native activation is unfinished; apply completes it"
		case record.Applied != want || p.live(faces[i], o.probe[i]) != want:
			if o.work == "" {
				o.work = "the native font engine did not select every managed face and Nerd Font glyph"
			}
		}
	}
	return o, nil
}

func (p *Fonts) keys(paths []string) []string {
	keys := []string{}
	for _, path := range paths {
		keys = append(keys, targets.Key(path, FontSlot))
	}
	return keys
}

func (p *Fonts) withoutQueryTool(st state.State, id string, paths []string) bool {
	if p.Context.OS != "linux" || !filepath.IsAbs(p.FontMatch) || len(desktopRecorded(st, id)) != 0 {
		return false
	}
	if _, err := os.Stat(p.FontMatch); !errors.Is(err, os.ErrNotExist) {
		return false
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}

// Inspect implements engine.Provider. It only reads: it queries the native
// font engine for the twelve faces and compares the font files and the
// registration records. It reports Satisfied without a record when the faces
// are already present, Blocked for a destination or a registration value
// that setup did not record, Missing without a record, Changed for an edited
// font file, and Unhealthy while the activation is unfinished or the engine
// does not select each face. A failed query is an error, which never proves
// absence. The one exception is Linux without fc-match, which the same run
// can install: without a font record and without a managed font file the
// missing files prove absence, and the result is Missing. The token covers
// the font files, the records and the registration values, not the selection
// of the font engine, so it stays the same when fc-match appears.
func (p *Fonts) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	if err := ctx.Err(); err != nil {
		return engine.Finding{}, err
	}
	paths, err := p.paths(r)
	if err != nil {
		return engine.Finding{}, err
	}
	st := rec.State()
	if !want.Present {
		recorded := desktopRecorded(st, r.ID)
		if len(recorded) == 0 {
			return engine.Finding{Status: engine.Absent, Token: desktopToken([][]string{{"false"}})}, nil
		}
		probe, err := p.query(ctx, paths)
		if err != nil {
			return engine.Finding{}, err
		}
		i, err := desktopInspect(p.writer(probe), rec, r.ID, want, nil, nil)
		if err != nil {
			return engine.Finding{}, err
		}
		for index, value := range fontRegistrations(probe) {
			if value != "" && value != paths[index] {
				i.changed = append(i.changed, "current-user font registration: "+fontFaces()[index].File)
			}
		}
		finding := i.released()
		if finding.Status == engine.Changed {
			finding.Reason = "a changed font file or font registration is retained: " + strings.Join(i.changed, ", ")
		}
		return finding, nil
	}
	if p.withoutQueryTool(st, r.ID, paths) {
		i, err := desktopInspect(p.writer(nil), rec, r.ID, want, nil, paths)
		if err != nil {
			return engine.Finding{}, err
		}
		registrations, _ := json.Marshal(make([]string, len(paths)))
		i.rows = append(i.rows, []string{string(registrations)})
		return engine.Finding{Status: engine.Missing, Token: desktopToken(i.rows)}, nil
	}
	o, err := p.observe(ctx, r, rec)
	if err != nil {
		return engine.Finding{}, err
	}
	i, err := desktopInspect(p.writer(o.probe), rec, r.ID, want, o.units, paths, p.keys(paths)...)
	if err != nil {
		return engine.Finding{}, err
	}
	registrations, _ := json.Marshal(fontRegistrations(o.probe))
	i.rows = append(i.rows, []string{string(registrations)})
	if o.reused {
		return engine.Finding{Status: engine.Satisfied, Token: desktopToken(i.rows), Notes: []string{"Hack Nerd Font is already installed for this user. Setup uses it and adds no font file."}}, nil
	}
	if o.blocked != "" {
		return engine.Finding{Status: engine.Blocked, Reason: o.blocked, Preserved: o.foreign, Token: desktopToken(i.rows)}, nil
	}
	if o.work != "" {
		i.work++
	}
	reason := o.work
	if reason == "" {
		reason = "a font file differs from its record"
	}
	return i.wanted(o.waiting, reason), nil
}

func fontClearMark(rec engine.Records, mark string) error {
	return rec.Update(func(s *state.State) error { s.Clear(mark); return nil })
}

func fontNativeFailure(rec engine.Records, mark string, err error) error {
	if _, started := platform.ExitCode(err); started {
		return fmt.Errorf("%w; run setup again and confirm that the command exited", err)
	}
	return errors.Join(err, fontClearMark(rec, mark))
}

// Apply implements engine.Provider. Before the first native command it
// publishes, in one state update, the intended value of each registration
// record and the pending mark "<resource>:activate". On Windows it then
// releases the registrations that setup recorded, so the font files can
// change. It converges the twelve font files, activates the faces in one
// native command, clears the mark and promotes each record whose face the
// native font engine then selects. A
// failed activation leaves the registration records unfinished, and the
// next apply completes them.
func (p *Fonts) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o, err := p.observe(ctx, r, rec)
	if err != nil {
		return err
	}
	switch {
	case o.reused:
		return nil
	case o.blocked != "":
		return errors.New(o.blocked)
	case o.waiting != "":
		return errors.New("font integration requires its verified archive: " + o.waiting)
	}
	all := make([]int, len(o.paths))
	for i := range all {
		all[i] = i
	}
	values := fontRegistrations(o.probe)
	faces := fontFaces()
	mark := r.ID + ":activate"
	if err := rec.Update(func(s *state.State) error {
		for i, path := range o.paths {
			if err := s.BeginTarget(targets.Key(path, FontSlot), state.Record{Resource: r.ID, Kind: "font", Next: p.want(faces[i], path, o.hashes[i]), Before: targets.Absent}); err != nil {
				return err
			}
		}
		s.Mark(mark)
		return nil
	}); err != nil {
		return err
	}
	if p.Context.OS == "windows" && slices.ContainsFunc(values, func(value string) bool { return value != "" }) {
		if err := p.activate(ctx, all, o.paths, values, false); err != nil {
			return fontNativeFailure(rec, mark, err)
		}
		values = make([]string, len(values))
	}
	w := p.writer(nil)
	for _, d := range o.units {
		if err := w.Converge(ctx, d, rec, desktopChoice(approved)); err != nil {
			return errors.Join(err, fontClearMark(rec, mark))
		}
	}
	if err := p.activate(ctx, all, o.paths, values, true); err != nil {
		return fontNativeFailure(rec, mark, err)
	}
	if err := fontClearMark(rec, mark); err != nil {
		return err
	}
	probe, err := p.query(ctx, o.paths)
	if err != nil {
		return err
	}
	ready := []string{}
	for i, path := range o.paths {
		if p.live(faces[i], probe[i]) == p.want(faces[i], path, o.hashes[i]) {
			ready = append(ready, targets.Key(path, FontSlot))
		}
	}
	if err := rec.Update(func(s *state.State) error {
		for _, key := range ready {
			if err := s.Promote(key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if len(ready) != len(o.paths) {
		return errors.New("fonts were published but native consumption is not ready; run setup again after resolving font activation")
	}
	known := append(p.keys(o.paths), o.paths...)
	for _, key := range desktopRecorded(rec.State(), r.ID) {
		if !slices.Contains(known, key) {
			if err := w.Release(ctx, key, rec, desktopChoice(approved)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Remove implements engine.Provider. It releases the native registrations
// that setup recorded, then the font files through the write order, then
// refreshes the font cache on Linux and drops the registration records. A
// registration value that is not the path of the managed file stays and
// stops the removal.
func (p *Fonts) Remove(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	paths, err := p.paths(r)
	if err != nil {
		return err
	}
	recorded := desktopRecorded(rec.State(), r.ID)
	if len(recorded) == 0 {
		return nil
	}
	probe, err := p.query(ctx, paths)
	if err != nil {
		return err
	}
	keys := p.keys(paths)
	owned, unowned := []int{}, []int{}
	st := rec.State()
	for i, key := range keys {
		if !slices.Contains(recorded, key) {
			continue
		}
		if record := st.Targets[key]; record.Applied != "" || record.Release {
			owned = append(owned, i)
		} else {
			unowned = append(unowned, i)
		}
	}
	mark := r.ID + ":release"
	if err := rec.Update(func(s *state.State) error {
		for _, i := range owned {
			if record := s.Targets[keys[i]]; record.Applied != "" && !record.Release {
				if err := s.BeginRelease(keys[i]); err != nil {
					return err
				}
			}
		}
		s.Mark(mark)
		return nil
	}); err != nil {
		return err
	}
	if err := p.activate(ctx, owned, paths, fontRegistrations(probe), false); err != nil {
		return fontNativeFailure(rec, mark, err)
	}
	if err := fontClearMark(rec, mark); err != nil {
		return err
	}
	w := p.writer(nil)
	for _, key := range desktopReleaseOrder(rec.State(), r.ID) {
		if !slices.Contains(keys, key) {
			if err := w.Release(ctx, key, rec, desktopChoice(approved)); err != nil {
				return err
			}
		}
	}
	if p.Context.OS == "linux" {
		if err := p.refreshLinux(ctx, paths); err != nil {
			return err
		}
	}
	if p.Context.OS == "windows" {
		after, err := p.query(ctx, paths)
		if err != nil {
			return err
		}
		for _, i := range owned {
			if after[i].Registration != "" {
				return errors.New("a font registration stays after the removal; run setup again")
			}
		}
	}
	return rec.Update(func(s *state.State) error {
		for _, i := range append(owned, unowned...) {
			s.Drop(keys[i])
		}
		return nil
	})
}
