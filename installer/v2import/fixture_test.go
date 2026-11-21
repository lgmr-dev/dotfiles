package v2import_test

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
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
	"github.com/luisgui1757/dotfiles/installer/v2import"
)

const (
	planID  = "1111111111111111111111111111111111111111111111111111111111111111"
	shells  = "integration.shells"
	plugins = "tmux.plugins"
	vscode  = "integration.vscode-managed"
)

func digest(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func operation(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

type fx struct {
	t                           *testing.T
	ctx                         platform.Context
	root, home, state, checkout string
	keyboard                    string
	v1                          v2import.State
	catalog                     *catalog.Catalog
	present                     map[string]bool
	failBuild                   map[string]bool
	failUnit                    map[string]int
	units                       map[string]func() []targets.Desired
	legacy                      []string
	nl                          string
	systemChanges               []string
	verify                      func(context.Context) error
	extraKinds                  map[string]targets.Kind
	captureKinds                map[string]targets.Kind
	inspected                   []string
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func link(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func describe(t *testing.T, root string) string {
	t.Helper()
	lines := []string{}
	var walk func(path, relative string)
	walk = func(path, relative string) {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			lines = append(lines, relative+"|absent")
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, relative+"|link|"+target)
		case info.IsDir():
			lines = append(lines, fmt.Sprintf("%s|directory|%o", relative, info.Mode().Perm()))
			children, err := os.ReadDir(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, child := range children {
				walk(filepath.Join(path, child.Name()), relative+"/"+child.Name())
			}
		default:
			lines = append(lines, fmt.Sprintf("%s|file|%o|%q", relative, info.Mode().Perm(), read(t, path)))
		}
	}
	walk(root, ".")
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

type observed struct {
	Path     string
	Snapshot platform.Snapshot
}

func (f *fx) snapshots(paths []string) []observed {
	f.t.Helper()
	result := []observed{}
	for _, path := range paths {
		snapshot, err := platform.SnapshotTree(path)
		if err != nil {
			f.t.Fatal(err)
		}
		result = append(result, observed{path, snapshot})
	}
	return result
}

func (f *fx) recovery(id string) string { return filepath.Join("recovery", planID, id) }

type configTarget struct {
	Destination string            `json:"destination"`
	Original    platform.Snapshot `json:"original"`
	Backup      string            `json:"backup"`
	Created     []string          `json:"created,omitempty"`
}

func (f *fx) configuration(id, provider string, publish map[string]func(path string)) {
	f.t.Helper()
	paths := slices.Sorted(maps.Keys(publish))
	op := operation(id)
	before := v2import.Observation{Provider: provider, Scope: "user", Identity: digest(f.t, paths)}
	earlier := f.snapshots(paths)
	before.Fingerprint = digest(f.t, earlier)
	baseline := []configTarget{}
	for i, path := range paths {
		workspace := filepath.Join(filepath.Dir(path), fmt.Sprintf(".dotfiles-config-%s-%d", op, i))
		target := configTarget{Destination: path, Original: earlier[i].Snapshot, Backup: filepath.Join(workspace, "previous")}
		if target.Original.Kind == "absent" {
			created, err := platform.MissingDirectories(path)
			if err != nil {
				f.t.Fatal(err)
			}
			target.Created = created
		} else {
			before.Present, before.Adoptable = true, true
			if err := os.MkdirAll(workspace, 0700); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Rename(path, target.Backup); err != nil {
				f.t.Fatal(err)
			}
		}
		baseline = append(baseline, target)
		publish[path](path)
	}
	after := before
	after.Present, after.Healthy, after.Fingerprint, after.CompletedOperation = true, true, digest(f.t, f.snapshots(paths)), op
	f.document(filepath.Join(f.state, f.recovery(id), "configuration.json"), map[string]any{"schema": 1, "resource": id, "operation": op, "before": before, "targets": baseline})
	f.v1.Receipts[id] = v2import.Receipt{Ownership: "created", Before: before, After: after, Status: "ready", Recovery: f.recovery(id), OperationID: op, Adopted: before.Present}
}

func (f *fx) document(path string, value any) {
	f.t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	write(f.t, path, string(data)+"\n", 0600)
}

type profileEntry struct {
	Path    string   `json:"path"`
	Block   []byte   `json:"block,omitempty"`
	Absent  bool     `json:"absent"`
	Created []string `json:"created,omitempty"`
}

func v2Block(id, script, nl string) string {
	return nl + "# >>> dotfiles:" + id + " >>>" + nl + strings.ReplaceAll(script, "\n", nl) + nl + "# <<< dotfiles:" + id + " <<<" + nl
}

func (f *fx) blocks(id string, scripts [][2]string) {
	f.t.Helper()
	before := v2import.Observation{Provider: "profile-block", Scope: "user"}
	entries, paths, earlier, live := []profileEntry{}, []string{}, [][]byte{}, [][]byte{}
	for _, item := range scripts {
		path, script := item[0], item[1]
		entry := profileEntry{Path: path}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			entry.Absent = true
			if entry.Created, err = platform.MissingDirectories(path); err != nil {
				f.t.Fatal(err)
			}
		} else if err != nil {
			f.t.Fatal(err)
		}
		block := v2Block(id, script, f.nl)
		if entry.Absent && f.ctx.OS == "windows" {
			data = []byte{0xef, 0xbb, 0xbf}
		}
		write(f.t, path, string(data)+block, 0644)
		entries, paths, earlier, live = append(entries, entry), append(paths, path), append(earlier, nil), append(live, []byte(block))
	}
	before.Identity, before.Fingerprint = digest(f.t, paths), digest(f.t, earlier)
	after := before
	after.Present, after.Healthy, after.Fingerprint = true, true, digest(f.t, live)
	f.document(filepath.Join(f.state, f.recovery(id), "profile.json"), map[string]any{"schema": 1, "resource": id, "before": before, "entries": entries})
	f.v1.Receipts[id] = v2import.Receipt{Ownership: "created", Before: before, After: after, Status: "ready", Recovery: f.recovery(id), OperationID: operation(id)}
}

func (f *fx) fields(id, path string, names []string, earlier, managed map[string]string, content string) {
	f.t.Helper()
	object := func(values map[string]string) []byte {
		if len(values) == 0 {
			return nil
		}
		raw := map[string]json.RawMessage{}
		for name, value := range values {
			raw[name] = json.RawMessage(value)
		}
		data, err := json.Marshal(raw)
		if err != nil {
			f.t.Fatal(err)
		}
		return data
	}
	before := v2import.Observation{Provider: "profile-block", Scope: "user", Present: len(earlier) != 0, Adoptable: len(earlier) != 0,
		Identity: digest(f.t, struct{ Paths, Fields []string }{[]string{path}, names}), Fingerprint: digest(f.t, [][]byte{object(earlier)})}
	after := before
	after.Present, after.Healthy, after.Adoptable, after.Fingerprint = true, true, true, digest(f.t, [][]byte{object(managed)})
	write(f.t, path, content, 0644)
	f.document(filepath.Join(f.state, f.recovery(id), "profile.json"), map[string]any{"schema": 1, "resource": id, "before": before, "fields": names,
		"entries": []profileEntry{{Path: path, Block: object(earlier)}}})
	f.v1.Receipts[id] = v2import.Receipt{Ownership: "created", Before: before, After: after, Status: "ready", Recovery: f.recovery(id), OperationID: operation(id), Adopted: before.Present}
}

func (f *fx) receipt(id, provider, ownership string, present bool) {
	observation := v2import.Observation{Present: present, Healthy: present, Provider: provider, Identity: id, Scope: "machine"}
	f.v1.Receipts[id] = v2import.Receipt{Ownership: ownership, Before: v2import.Observation{Provider: provider, Identity: id, Scope: "machine"}, After: observation, Status: "ready"}
}

func (f *fx) save() []byte {
	f.t.Helper()
	data, err := json.MarshalIndent(f.v1, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	write(f.t, filepath.Join(f.state, "state.json"), string(data)+"\n", 0600)
	return append(data, '\n')
}

func (f *fx) writer() *targets.Writer {
	return &targets.Writer{StateDir: f.state, Kinds: f.kinds()}
}

type fontKind struct{ targets.Kind }

func (k fontKind) Slotted() bool { return true }

func (k fontKind) Observe(d targets.Desired) (string, error) {
	if _, err := os.Stat(d.Path); err != nil {
		return targets.Absent, nil
	}
	return "font:registered", nil
}

func (k fontKind) Save(context.Context, targets.Desired, string) (string, error) {
	return "", errors.New("setup makes no copy of a font registration")
}

func (k fontKind) Baseline(before string) (string, error) {
	return strings.TrimPrefix(before, "fp:"), nil
}

func (f *fx) kinds() map[string]targets.Kind {
	kinds := hooks.ProfileKinds()
	kinds["font"] = fontKind{targets.FileKind("font")}
	maps.Copy(kinds, f.extraKinds)
	return kinds
}

type unitsProvider struct{ f *fx }

func (p unitsProvider) recorded(st state.State, id string) []string {
	keys := []string{}
	for _, key := range slices.Sorted(maps.Keys(st.Targets)) {
		if st.Targets[key].Resource == id {
			keys = append(keys, key)
		}
	}
	return keys
}

func (p unitsProvider) Inspect(_ context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	p.f.inspected = append(p.f.inspected, r.ID)
	st := rec.State()
	rows := []string{fmt.Sprint(want.Present)}
	if !want.Present {
		keys := p.recorded(st, r.ID)
		if len(keys) == 0 {
			return engine.Finding{Status: engine.Absent, Token: "absent"}, nil
		}
		return engine.Finding{Status: engine.Satisfied, Token: strings.Join(keys, "\n")}, nil
	}
	w := p.f.writer()
	changed, work, owned := []string{}, 0, 0
	for _, d := range p.f.units[r.ID]() {
		d.Resource = r.ID
		live, err := w.Inspect(d, rec)
		if err != nil {
			return engine.Finding{}, err
		}
		key := targets.Key(d.Path, d.Slot)
		record, has := st.Targets[key]
		if has && record.Applied != "" {
			owned++
		}
		rows = append(rows, key, live.State, live.Fingerprint, live.Want)
		switch {
		case live.State == targets.StateUnreadable:
			return engine.Finding{Status: engine.Unknown, Reason: live.Detail, Token: live.Detail}, nil
		case live.State == targets.StateChanged && live.Fingerprint != live.Want:
			changed = append(changed, key)
		case live.State == targets.StateApplied && record.Applied == live.Want && record.Next == "" && !record.Release:
		default:
			work++
		}
	}
	finding := engine.Finding{Status: engine.Satisfied, Token: strings.Join(rows, "\n")}
	switch {
	case len(changed) != 0:
		finding.Status, finding.Preserved, finding.Reason = engine.Changed, changed, "content differs: "+strings.Join(changed, ", ")
	case work != 0 && owned == 0:
		finding.Status = engine.Missing
	case work != 0:
		finding.Status, finding.Reason = engine.Unhealthy, "a target differs from its record"
	}
	return finding, nil
}

func (p unitsProvider) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	choice := targets.Preserve
	if approved.Replace {
		choice = targets.Replace
	}
	for i, d := range p.f.units[r.ID]() {
		if p.f.failUnit[r.ID] == i+1 {
			return errors.New("injected failure in the middle of the apply")
		}
		d.Resource = r.ID
		if err := p.f.writer().Converge(ctx, d, rec, choice); err != nil {
			return err
		}
	}
	return nil
}

func (p unitsProvider) Remove(ctx context.Context, r catalog.Resource, _ engine.Finding, rec engine.Records) error {
	for _, key := range p.recorded(rec.State(), r.ID) {
		if err := p.f.writer().Release(ctx, key, rec, targets.Preserve); err != nil {
			return err
		}
	}
	return nil
}

type buildProvider struct{ f *fx }

func (p buildProvider) current(id string) string {
	return filepath.Join(p.f.state, "tools", id, "current")
}

func (p buildProvider) public() string {
	return filepath.Join(p.f.home, ".local", "share", "nvim", "dotfiles-runtime", "nvim.sync", "current")
}

func (p buildProvider) Inspect(_ context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	p.f.inspected = append(p.f.inspected, r.ID)
	current := p.current(r.ID)
	record, has := rec.State().Targets[current]
	target, err := os.Readlink(current)
	built := has && err == nil && record.Applied == target && record.Next == ""
	if !want.Present {
		if !has {
			return engine.Finding{Status: engine.Absent, Token: "absent"}, nil
		}
		return engine.Finding{Status: engine.Satisfied, Token: "built"}, nil
	}
	if !built {
		return engine.Finding{Status: engine.Missing, Token: "missing"}, nil
	}
	if r.ID == hooks.NvimResource {
		live, err := p.f.writer().Inspect(targets.Desired{Resource: r.ID, Kind: "link", Path: p.public(), Link: target}, rec)
		if err != nil {
			return engine.Finding{}, err
		}
		if live.State != targets.StateApplied || live.Fingerprint != live.Want {
			return engine.Finding{Status: engine.Unhealthy, Reason: "the public runtime link does not point at the current runtime", Token: "public:" + live.Fingerprint}, nil
		}
	}
	return engine.Finding{Status: engine.Satisfied, Token: "built:" + target}, nil
}

func (p buildProvider) Apply(ctx context.Context, r catalog.Resource, _ engine.Finding, rec engine.Records) error {
	if p.f.failBuild[r.ID] {
		return errors.New("injected build failure")
	}
	current := p.current(r.ID)
	build := filepath.Join(filepath.Dir(current), "1-abcdef012345-1")
	write(p.f.t, filepath.Join(build, "bin", "tool"), "v3 "+r.ID+"\n", 0755)
	if err := rec.Update(func(s *state.State) error {
		return s.BeginTarget(current, state.Record{Resource: r.ID, Kind: "link", Next: build, Before: targets.Absent})
	}); err != nil {
		return err
	}
	if _, err := os.Lstat(current); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink(build, current); err != nil {
			return err
		}
	}
	if err := rec.Update(func(s *state.State) error { return s.Promote(current) }); err != nil {
		return err
	}
	if r.ID != hooks.NvimResource {
		return nil
	}
	return p.f.writer().Converge(ctx, targets.Desired{Resource: r.ID, Kind: "link", Path: p.public(), Link: build}, rec, targets.Preserve)
}

func (p buildProvider) Remove(context.Context, catalog.Resource, engine.Finding, engine.Records) error {
	return errors.New("the import must never remove a build")
}

type systemProvider struct{ f *fx }

func (p systemProvider) Inspect(_ context.Context, r catalog.Resource, want engine.Want, _ engine.Records) (engine.Finding, error) {
	p.f.inspected = append(p.f.inspected, r.ID)
	switch {
	case p.f.present[r.ID]:
		return engine.Finding{Status: engine.Satisfied, Token: "present"}, nil
	case want.Present:
		return engine.Finding{Status: engine.Missing, Reason: "the system item is not installed", Token: "missing"}, nil
	}
	return engine.Finding{Status: engine.Absent, Token: "absent"}, nil
}

func (p systemProvider) Apply(_ context.Context, r catalog.Resource, _ engine.Finding, _ engine.Records) error {
	p.f.systemChanges = append(p.f.systemChanges, r.ID)
	p.f.present[r.ID] = true
	return nil
}

func (p systemProvider) Remove(_ context.Context, r catalog.Resource, _ engine.Finding, rec engine.Records) error {
	p.f.systemChanges = append(p.f.systemChanges, "remove:"+r.ID)
	return rec.Update(func(s *state.State) error {
		for key, record := range s.Targets {
			if record.Resource == r.ID {
				s.Drop(key)
			}
		}
		return nil
	})
}

func (f *fx) route(r catalog.Resource) (engine.Provider, error) {
	switch {
	case r.Action == "":
		return nil, nil
	case r.Action == "config":
		return &targets.ConfigProvider{Catalog: f.catalog, Context: f.ctx, Folders: f.folders(), Checkout: f.checkout, Writer: f.writer()}, nil
	case r.Bindings[f.ctx.OS].Provider == "archive" || r.ID == hooks.NvimResource:
		return buildProvider{f}, nil
	case f.units[r.ID] != nil:
		return unitsProvider{f}, nil
	}
	return systemProvider{f}, nil
}

func (f *fx) folders() platform.Folders {
	return platform.Folders{Home: f.home, Config: filepath.Join(f.home, ".config"), Zsh: f.home, Data: filepath.Join(f.home, ".local", "share")}
}

func (f *fx) engine() *engine.Engine {
	return &engine.Engine{Catalog: f.catalog, Router: f.route, Context: f.ctx, StateDir: f.state, Target: f.ctx.OS + "/" + f.ctx.Arch + ":" + f.home, Source: "fixture-source"}
}

func (f *fx) options() v2import.Options {
	kinds := f.kinds()
	maps.Copy(kinds, f.captureKinds)
	return v2import.Options{Engine: f.engine(), Folders: f.folders(), Kinds: kinds, KeyboardRule: f.keyboard, VerifyNvim: f.verify}
}

func (f *fx) preview() (*v2import.Proposal, error) {
	return v2import.Preview(context.Background(), f.options(), func(directories []string) { f.legacy = directories })
}

func (f *fx) mustImport() engine.Result {
	f.t.Helper()
	proposal, err := f.preview()
	if err != nil {
		f.t.Fatal(err)
	}
	result, err := proposal.Run(context.Background())
	if err != nil {
		f.t.Fatalf("import failed: %v\n%s", err, result.Message)
	}
	return result
}

func (f *fx) current() state.State {
	f.t.Helper()
	st, err := state.Peek(f.state, f.engine().Target)
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

func archive(os ...string) map[string]catalog.Binding {
	bindings := map[string]catalog.Binding{}
	for _, system := range os {
		bindings[system] = catalog.Binding{Provider: "archive", Package: "pinned"}
	}
	return bindings
}

func newFixture(t *testing.T, system string) *fx {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fixtures of release v2 use symbolic links and POSIX modes; the migrate job proves the import on Windows")
	}
	root, err := platform.ResolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	arch := map[string]string{"linux": "amd64", "darwin": "arm64", "windows": "amd64"}[system]
	f := &fx{t: t, ctx: platform.Context{OS: system, Arch: arch}, root: root, home: filepath.Join(root, "home"), state: filepath.Join(root, "state"),
		checkout: filepath.Join(root, "checkout"), keyboard: filepath.Join(root, "etc", "70-dotfiles-mac-keyboard.rules"),
		nl: "\n", present: map[string]bool{}, failBuild: map[string]bool{}, failUnit: map[string]int{}, units: map[string]func() []targets.Desired{}}
	f.v1 = v2import.State{Schema: 1, Context: v2import.Context{OS: system, Arch: arch}, Target: f.home, Source: "v2-source", Generation: 7,
		Selected: []string{}, Keep: []string{}, Receipts: map[string]v2import.Receipt{}}
	all := []string{"darwin", "linux", "windows"}
	f.catalog = &catalog.Catalog{
		Resources: []catalog.Resource{
			{ID: "neovim", Name: "Neovim", Capability: true, Requires: []string{"config.nvim", "tool.nvim", hooks.NvimResource}},
			{ID: "shell", Name: "Shell", Capability: true, Requires: []string{shells, "tool.zsh"}},
			{ID: "tmux", Name: "tmux", Capability: true, Platforms: []string{"darwin", "linux"}, Requires: []string{"config.tmux", plugins, "tool.tmux"}},
			{ID: "fonts", Name: "Fonts", Capability: true, Requires: []string{hooks.FontResource}},
			{ID: "keyboard", Name: "Mac keyboard", Capability: true, Platforms: []string{"linux"}, Requires: []string{"system.keyboard-access"}},
			{ID: "sentinel", Name: "Sentinel", Capability: true, Requires: []string{hooks.SentinelResource}},
			{ID: "vscode", Name: "VS Code", Capability: true, Requires: []string{"tool.vscode", "desktop.vscode", vscode}},
			{ID: "lazygit", Name: "LazyGit", Capability: true, Requires: []string{"tool.lazygit"}},
			{ID: "config.nvim", Name: "Neovim configuration", Action: "config"},
			{ID: "tool.nvim", Name: "Neovim program", Action: "archive", Bindings: archive(all...)},
			{ID: hooks.NvimResource, Name: "Neovim runtime", Action: "nvim-sync", Requires: []string{"config.nvim", "tool.nvim"}},
			{ID: shells, Name: "Shell profiles", Action: "profile"},
			{ID: "tool.zsh", Name: "Zsh", Action: "package", Bindings: map[string]catalog.Binding{"linux": {Provider: "apt", Package: "zsh"}}},
			{ID: "config.tmux", Name: "tmux configuration", Action: "config", Platforms: []string{"darwin", "linux"}},
			{ID: plugins, Name: "tmux plugins", Action: "profile", Platforms: []string{"darwin", "linux"}},
			{ID: "tool.tmux", Name: "tmux program", Action: "package", Shared: true, Platforms: []string{"darwin", "linux"},
				Bindings: map[string]catalog.Binding{"linux": {Provider: "apt", Package: "tmux"}, "darwin": {Provider: "homebrew-formula", Package: "tmux"}}},
			{ID: hooks.FontResource, Name: "Hack Nerd Font", Action: "font", Requires: []string{hooks.FontArchive}},
			{ID: hooks.FontArchive, Name: "Font archive", Action: "archive", Bindings: archive(all...)},
			{ID: "system.keyboard-access", Name: "Keyboard access", Action: "keyboard", Platforms: []string{"linux"}},
			{ID: hooks.SentinelResource, Name: "Sentinel", Action: "sentinel"},
			{ID: "tool.vscode", Name: "VS Code program", Action: "package", Requires: []string{shells},
				Bindings:         map[string]catalog.Binding{"darwin": {Provider: "homebrew-cask", Package: "visual-studio-code"}, "linux": {Provider: "apt", Package: "code"}, "windows": {Provider: "archive", Package: "pinned"}},
				PlatformRequires: map[string][]string{"darwin": {"infra.homebrew"}}},
			{ID: "infra.homebrew", Name: "Homebrew", Action: "homebrew", Platforms: []string{"darwin"}, Retain: true},
			{ID: "desktop.vscode", Name: "VS Code launcher", Action: "desktop", Requires: []string{"tool.vscode"}},
			{ID: vscode, Name: "VS Code settings", Action: "profile"},
			{ID: "tool.lazygit", Name: "LazyGit program", Action: "archive", Bindings: archive(all...)},
			{ID: "tool.vcredist", Name: "VC++ runtime", Action: "vendor", Platforms: []string{"windows"}},
		},
		Targets: []catalog.Target{
			{Resource: "config.nvim", Source: "nvim", Folder: "config", Path: "nvim", Directory: true, Mode: map[string]string{"windows": "copy"}[system]},
			{Resource: "config.tmux", Source: "tmux/tmux.conf", Folder: "home", Path: ".tmux.conf", Platforms: []string{"darwin", "linux"}},
		},
	}
	write(t, filepath.Join(f.checkout, "nvim", "init.lua"), "-- managed\n", 0644)
	write(t, filepath.Join(f.checkout, "tmux", "tmux.conf"), "set -g prefix C-b\n", 0644)
	if err := os.MkdirAll(f.home, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.state, state.LockFile), "", 0600)
	return f
}

func (f *fx) shellUnits() []targets.Desired {
	root := filepath.Join(f.state, "shells")
	return []targets.Desired{
		{Kind: "file", Path: filepath.Join(root, "env.sh"), Content: []byte("# v3 env\n")},
		{Kind: "file", Path: filepath.Join(root, "init.sh"), Content: []byte("# v3 init\n")},
		{Kind: "block", Path: filepath.Join(f.home, ".zshrc"), Slot: shells, Content: []byte(". v3-init")},
		{Kind: "block", Path: filepath.Join(f.home, ".bashrc"), Slot: shells, Content: []byte(". v3-init")},
		{Kind: "block", Path: filepath.Join(f.home, ".zprofile"), Slot: shells, Content: []byte(". v3-env")},
		{Kind: "file", Path: filepath.Join(f.home, ".config", "v3-added", "settings.conf"), Content: []byte("added by v3\n")},
	}
}

func (f *fx) fontPath() string {
	return filepath.Join(f.home, ".local", "share", "fonts", "Dotfiles-HackNerdFont", "HackNerdFontMono-Regular.ttf")
}

func (f *fx) publicRuntime() string {
	return filepath.Join(f.home, ".local", "share", "nvim", "dotfiles-runtime", "nvim.sync", "current")
}

func keyboardRule(op string) string {
	return "# Managed by dotfiles (Mac keyboard shortcuts), operation " + op + ".\n" +
		"# Lets the user logged in at this seat read keyboards and create a virtual\n" +
		"# keyboard for the xremap key remapper. Unselect the tool to remove it.\n" +
		"KERNEL==\"uinput\", SUBSYSTEM==\"misc\", TAG+=\"uaccess\", OPTIONS+=\"static_node=uinput\"\n" +
		"SUBSYSTEM==\"input\", KERNEL==\"event*\", ENV{ID_INPUT_KEYBOARD}==\"1\", TAG+=\"uaccess\"\n"
}

func (f *fx) common() {
	t := f.t
	write(t, filepath.Join(f.state, "packages", "tool.nvim", "versions", "aaaa", "payload", "bin", "nvim"), "v2 nvim\n", 0755)
	link(t, filepath.Join(f.state, "packages", "tool.nvim", "versions", "aaaa", "payload"), filepath.Join(f.state, "packages", "tool.nvim", "current"))
	f.receipt("tool.nvim", "archive", "created", true)
	write(t, filepath.Join(f.state, "packages", hooks.FontArchive, "versions", "bbbb", "payload", "HackNerdFontMono-Regular.ttf"), "v2 font bytes\n", 0644)
	link(t, filepath.Join(f.state, "packages", hooks.FontArchive, "versions", "bbbb", "payload"), filepath.Join(f.state, "packages", hooks.FontArchive, "current"))
	f.receipt(hooks.FontArchive, "archive", "created", true)
	f.configuration("config.nvim", "configuration", map[string]func(string){
		filepath.Join(f.home, ".config", "nvim"): func(path string) { link(t, filepath.Join(f.checkout, "nvim"), path) }})
	runtime := filepath.Join(f.state, "nvim-sync", "versions", operation("nvim"), "payload")
	write(t, filepath.Join(runtime, "lazy", "plugin.lua"), "-- v2 runtime\n", 0644)
	link(t, runtime, f.publicRuntime())
	f.v1.Receipts[hooks.NvimResource] = v2import.Receipt{Ownership: "created", Status: "ready", OperationID: operation("nvim"),
		Before: v2import.Observation{Provider: "nvim-sync", Identity: f.publicRuntime(), Scope: "user"},
		After:  v2import.Observation{Present: true, Healthy: true, Provider: "nvim-sync", Identity: f.publicRuntime(), Scope: "user", Fingerprint: digest(t, runtime)}}
	f.configuration(hooks.FontResource, "native-font", map[string]func(string){
		f.fontPath(): func(path string) {
			link(t, filepath.Join(f.state, "packages", hooks.FontArchive, "current", "HackNerdFontMono-Regular.ttf"), path)
		}})
	write(t, filepath.Join(f.state, "sentinel", "record.json"), "{\"schema\":1,\"release\":\"v0.4.0\"}\n", 0600)
	write(t, filepath.Join(f.state, "sentinel", "checkout", "install.sh"), "#!/bin/sh\n", 0755)
	write(t, filepath.Join(f.state, "sentinel", "checkout", ".git", "HEAD"), "0123456789012345678901234567890123456789\n", 0644)
	f.receipt(hooks.SentinelResource, "sentinel", "created", true)
	f.receipt("integration.sentinel", "sentinel-handoff", "reused", false)
	f.receipt("skills.sentinel-claude", "sentinel-handoff", "reused", false)
	f.units[hooks.FontResource] = func() []targets.Desired {
		return []targets.Desired{{Kind: "file", Path: f.fontPath(), Content: []byte("v3 font bytes\n"), Mode: 0644}}
	}
	f.v1.Selected = append(f.v1.Selected, "neovim", "fonts", "sentinel", "sentinel-skills")
	f.present[hooks.SentinelResource] = true
}

func linuxFixture(t *testing.T) *fx {
	f := newFixture(t, "linux")
	f.common()
	write(t, filepath.Join(f.home, ".zshrc"), "# personal zshrc\nexport EDITOR=vi\n", 0644)
	f.blocks(shells, [][2]string{
		{filepath.Join(f.state, "shells", "env.sh"), "export PATH=v2"},
		{filepath.Join(f.state, "shells", "init.sh"), "# v2 init"},
		{filepath.Join(f.home, ".zshrc"), ". v2-init"},
		{filepath.Join(f.home, ".bashrc"), ". v2-init"},
	})
	write(t, filepath.Join(f.home, ".zshrc"), read(t, filepath.Join(f.home, ".zshrc"))+"alias personal='after the block'\n", 0644)
	f.units[shells] = f.shellUnits
	write(t, filepath.Join(f.home, ".tmux.conf"), "# my own tmux configuration\n", 0600)
	f.configuration("config.tmux", "configuration", map[string]func(string){
		filepath.Join(f.home, ".tmux.conf"): func(path string) { link(t, filepath.Join(f.checkout, "tmux", "tmux.conf"), path) }})
	f.blocks(plugins, [][2]string{{filepath.Join(f.home, ".tmux.plugins.conf"), "run-shell v2"}})
	write(t, filepath.Join(f.home, ".tmux.plugins.conf"), v2Block(plugins, "run-shell v2\nrun-shell mine", "\n"), 0644)
	f.units[plugins] = func() []targets.Desired {
		return []targets.Desired{{Kind: "block", Path: filepath.Join(f.home, ".tmux.plugins.conf"), Slot: plugins, Content: []byte("run-shell v3")}}
	}
	settings := filepath.Join(f.home, ".config", "Code", "User", "settings.json")
	f.fields(vscode, settings, []string{"workbench.colorTheme", "update.mode"}, map[string]string{"workbench.colorTheme": `"Personal <Dark>"`},
		map[string]string{"workbench.colorTheme": `"Rosé Pine"`, "update.mode": `"none"`},
		"{\n  // personal comment\n  \"editor.fontSize\": 13,\n  \"workbench.colorTheme\": \"Rosé Pine\",\n  \"update.mode\": \"none\",\n}\n")
	f.units[vscode] = func() []targets.Desired {
		return []targets.Desired{
			{Kind: "field", Path: settings, Slot: "workbench.colorTheme", Content: []byte(`"Rosé Pine"`)},
			{Kind: "field", Path: settings, Slot: "update.mode", Content: []byte(`"none"`)},
		}
	}
	op := operation("keyboard")
	write(t, f.keyboard, keyboardRule(op), 0644)
	sum := sha256.Sum256([]byte(keyboardRule(op)))
	f.v1.Receipts["system.keyboard-access"] = v2import.Receipt{Ownership: "created", Status: "ready", OperationID: op,
		Before: v2import.Observation{Provider: "keyboard-access", Identity: f.keyboard, Scope: "user", Privileged: true},
		After:  v2import.Observation{Present: true, Healthy: true, Provider: "keyboard-access", Identity: f.keyboard, Scope: "user", Privileged: true, Fingerprint: hex.EncodeToString(sum[:])}}
	f.receipt("tool.zsh", "apt", "created", true)
	f.receipt("tool.tmux", "apt", "reused", true)
	f.receipt("tool.vscode", "apt", "created", true)
	f.present["tool.vscode"] = true
	f.receipt("tool.flatpak", "apt", "created", true)
	f.receipt("tool.lazygit", "archive", "created", true)
	f.present["tool.zsh"], f.present["tool.tmux"], f.present["system.keyboard-access"] = true, true, true
	f.v1.Selected = append(f.v1.Selected, "shell", "tmux", "keyboard", "vscode")
	f.v1.Keep = []string{"tool.flatpak"}
	f.units["desktop.vscode"] = func() []targets.Desired { return nil }
	f.present["desktop.vscode"] = true
	return f
}

func darwinFixture(t *testing.T) *fx {
	f := newFixture(t, "darwin")
	f.common()
	write(t, filepath.Join(f.home, ".zshrc"), "# personal zshrc\n", 0644)
	f.blocks(shells, [][2]string{
		{filepath.Join(f.state, "shells", "env.sh"), "export PATH=v2"},
		{filepath.Join(f.state, "shells", "init.sh"), "# v2 init"},
		{filepath.Join(f.home, ".zshrc"), ". v2-init"},
		{filepath.Join(f.home, ".bashrc"), ". v2-init"},
	})
	f.units[shells] = f.shellUnits
	application := filepath.Join(f.state, "packages", "tool.vscode", "versions", "cccc", "payload")
	write(t, filepath.Join(application, "Visual Studio Code.app", "Contents", "Resources", "app", "bin", "code"), "#!/bin/sh\n", 0755)
	link(t, application, filepath.Join(f.state, "packages", "tool.vscode", "current"))
	f.receipt("tool.vscode", "archive", "created", true)
	f.configuration("desktop.vscode", "configuration", map[string]func(string){
		filepath.Join(f.home, "Applications", "Visual Studio Code.app"): func(path string) {
			link(t, filepath.Join(f.state, "packages", "tool.vscode", "current", "Visual Studio Code.app"), path)
		}})
	settings := filepath.Join(f.home, "Library", "Application Support", "Code", "User", "settings.json")
	f.fields(vscode, settings, []string{"workbench.colorTheme", "update.mode"}, nil,
		map[string]string{"workbench.colorTheme": `"Rosé Pine"`, "update.mode": `"none"`},
		"{\"workbench.colorTheme\": \"Rosé Pine\", \"update.mode\": \"none\"}\n")
	f.units[vscode] = func() []targets.Desired {
		return []targets.Desired{
			{Kind: "field", Path: settings, Slot: "workbench.colorTheme", Content: []byte(`"Rosé Pine"`)},
			{Kind: "field", Path: settings, Slot: "update.mode", Content: []byte(`"none"`)},
		}
	}
	f.units["desktop.vscode"] = func() []targets.Desired {
		return []targets.Desired{{Kind: "link", Path: filepath.Join(f.home, ".local", "bin", "code"), Link: "/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code"}}
	}
	f.receipt("tool.tmux", "homebrew-formula", "created", true)
	f.receipt("infra.homebrew", "homebrew-infrastructure", "reused", true)
	f.receipt("tool.zsh", "operating-system", "reused", true)
	f.present["tool.tmux"], f.present["tool.zsh"] = true, true
	write(t, filepath.Join(f.home, ".tmux.conf"), "# my own tmux configuration\n", 0600)
	f.configuration("config.tmux", "configuration", map[string]func(string){
		filepath.Join(f.home, ".tmux.conf"): func(path string) { link(t, filepath.Join(f.checkout, "tmux", "tmux.conf"), path) }})
	f.blocks(plugins, [][2]string{{filepath.Join(f.home, ".tmux.plugins.conf"), "run-shell v2"}})
	f.units[plugins] = func() []targets.Desired {
		return []targets.Desired{{Kind: "block", Path: filepath.Join(f.home, ".tmux.plugins.conf"), Slot: plugins, Content: []byte("run-shell v3")}}
	}
	f.v1.Selected = append(f.v1.Selected, "shell", "tmux", "vscode")
	return f
}

func windowsFixture(t *testing.T) *fx {
	f := newFixture(t, "windows")
	f.nl = "\r\n"
	t.Helper()
	write(t, filepath.Join(f.state, "packages", "tool.nvim", "versions", "aaaa", "payload", "bin", "nvim.exe"), "v2 nvim\n", 0755)
	f.receipt("tool.nvim", "archive", "created", true)
	f.configuration("config.nvim", "configuration", map[string]func(string){
		filepath.Join(f.home, ".config", "nvim"): func(path string) {
			write(t, filepath.Join(path, "init.lua"), "-- managed\n", 0644)
		}})
	profile := filepath.Join(f.home, "Documents", "PowerShell", "profile.ps1")
	write(t, profile, "\xef\xbb\xbf# personal profile\r\nSet-Alias ll ls\r\n", 0644)
	f.blocks(shells, [][2]string{
		{filepath.Join(f.state, "shells", "init.ps1"), "$env:PATH = 'v2'"},
		{profile, ". 'init.ps1'"},
		{filepath.Join(f.home, "Documents", "WindowsPowerShell", "profile.ps1"), ". 'init.ps1'"},
	})
	f.receipt("tool.vcredist", "windows-vendor", "created", true)
	f.receipt("tool.vscode", "archive", "created", true)
	f.present["tool.vcredist"] = true
	f.v1.Selected = append(f.v1.Selected, "neovim", "shell", "vscode")
	return f
}
