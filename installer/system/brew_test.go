package system

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/state"
)

type brewFixture struct {
	t          *testing.T
	d          *Brew
	rec        *macTestRecords
	runner     *macTestRunner
	root       string
	location   HomebrewLocation
	installed  map[string]brewPackage
	outdated   map[string]string
	broken     map[string]bool
	metadata   map[string]any
	checkErr   error
	signature  error
	running    bool
	queries    [][]string
	states     []state.State
	onRun      func(args []string) error
	discovered bool
}

func newBrewFixture(t *testing.T) *brewFixture {
	t.Helper()
	root := macTestRoot(t)
	prefix := filepath.Join(root, "homebrew")
	f := &brewFixture{t: t, d: NewBrew(filepath.Join(root, "home")), rec: newMacTestRecords(), runner: &macTestRunner{}, root: root,
		location:  HomebrewLocation{Program: filepath.Join(prefix, "bin", "brew"), Prefix: prefix, Cellar: filepath.Join(prefix, "Cellar")},
		installed: map[string]brewPackage{}, outdated: map[string]string{}, broken: map[string]bool{}, metadata: map[string]any{}, discovered: true}
	if f.d.appdir != "/Applications" {
		t.Fatal("applications must go to /Applications", f.d.appdir)
	}
	f.d.appdir = filepath.Join(root, "Applications")
	for _, directory := range []string{f.d.appdir, filepath.Join(f.d.Home, "Applications"), filepath.Join(prefix, "bin")} {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	f.d.Discover = func(context.Context) (*HomebrewLocation, error) {
		if !f.discovered {
			return nil, nil
		}
		return &f.location, nil
	}
	f.d.Query, f.d.Runner = f.query, f.runner
	f.runner.run = func(c platform.Command, _ string) ([]byte, error) {
		f.states = append(f.states, f.rec.State())
		if c.Path != f.location.Program {
			t.Fatal("a mutation used another program", c.Path)
		}
		if f.onRun == nil {
			return nil, nil
		}
		return nil, f.onRun(c.Args)
	}
	return f
}

func (f *brewFixture) inventory() []byte {
	formulae, casks := []any{}, []any{}
	for name, pkg := range f.installed {
		if token, cask := strings.CutPrefix(name, "cask/"); cask {
			casks = append(casks, map[string]any{"token": token, "full_token": pkg.Source, "installed": pkg.Version, "depends_on": map[string]any{"formula": pkg.Dependencies}})
			continue
		}
		dependencies := []any{}
		for _, dependency := range pkg.Dependencies {
			dependencies = append(dependencies, map[string]string{"full_name": dependency})
		}
		linked := ""
		if pkg.Healthy {
			linked = pkg.Version
		}
		formulae = append(formulae, map[string]any{"name": name, "full_name": pkg.Source, "pinned": pkg.Held, "linked_keg": linked,
			"installed": []any{map[string]any{"version": pkg.Version, "installed_on_request": !pkg.Automatic, "runtime_dependencies": dependencies}}})
	}
	data, err := json.Marshal(map[string]any{"formulae": formulae, "casks": casks})
	if err != nil {
		f.t.Fatal(err)
	}
	return data
}

func (f *brewFixture) query(_ context.Context, program string, args ...string) ([]byte, error) {
	f.queries = append(f.queries, append([]string{program}, args...))
	switch program {
	case "/usr/bin/codesign":
		return nil, f.signature
	case "/usr/bin/osascript":
		if f.running {
			return []byte("running\n"), nil
		}
		return []byte("not-running\n"), nil
	case "brew":
	default:
		if strings.HasPrefix(program, filepath.Join(f.location.Prefix, "bin")+"/") {
			return []byte("fixture version\n"), f.checkErr
		}
		f.t.Fatal("unexpected query", program, args)
	}
	switch {
	case slices.Equal(args, []string{"info", "--json=v2", "--installed"}):
		return f.inventory(), nil
	case len(args) == 4 && slices.Equal(args[:3], []string{"info", "--json=v2", "--cask"}):
		return json.Marshal(map[string]any{"casks": []any{f.metadata[args[3]]}})
	case len(args) == 4 && slices.Equal(args[:3], []string{"outdated", "--json=v2", "--formula"}):
		list := []any{}
		if current, ok := f.outdated[args[3]]; ok {
			list = append(list, map[string]string{"name": args[3], "current_version": current})
		}
		data, _ := json.Marshal(map[string]any{"formulae": list, "casks": []any{}})
		if len(list) != 0 {
			return data, macTestExit(1)
		}
		return data, nil
	case len(args) > 2 && args[0] == "linkage" && args[1] == "--test":
		for _, source := range args[2:] {
			if f.broken[source] {
				return []byte("broken linkage of " + source), macTestExit(1)
			}
		}
		return nil, nil
	}
	f.t.Fatal("unexpected brew query", args)
	return nil, nil
}

func brewTestFormula(id, name string) catalog.Resource {
	return catalog.Resource{ID: id, Name: name, Action: "package", Scope: "machine", PlatformRequires: map[string][]string{"darwin": {"infra.homebrew"}},
		Bindings: map[string]catalog.Binding{"darwin": {Provider: "homebrew-formula", Package: name}}}
}

func (f *brewFixture) inspect(r catalog.Resource, want engine.Want) engine.Finding {
	f.t.Helper()
	found, err := f.d.Inspect(context.Background(), r, want, f.rec)
	if err != nil {
		f.t.Fatal(err)
	}
	return found
}

func (f *brewFixture) apply(r catalog.Resource, want engine.Want) error {
	return f.d.Apply(context.Background(), r, f.inspect(r, want), f.rec)
}

func (f *brewFixture) mutations() [][]string {
	result := [][]string{}
	for _, c := range f.runner.commands {
		result = append(result, c.Args)
	}
	return result
}

func (f *brewFixture) line(cask bool, args ...string) string {
	environment := slices.Clone(brewProcessControls)
	if cask {
		environment = append(environment, "HOMEBREW_CASK_OPTS=", "HOMEBREW_NO_SUDO=1")
	}
	return macTestLine(platform.Command{Path: f.location.Program, Args: args, Env: environment})
}

func TestMacShownCommandLineQuotesEachWord(t *testing.T) {
	got := macosShellLine(platform.Command{Path: "/opt/home brew/bin/brew", Env: []string{"LC_ALL=C", "EMPTY=", "NOTE=it's here"},
		Args: []string{"install", "--cask", "--appdir=/Applications", "T3 Code (Beta)", "$HOME", ""}})
	want := `LC_ALL=C EMPTY= NOTE='it'"'"'s here' '/opt/home brew/bin/brew' install --cask --appdir=/Applications 'T3 Code (Beta)' '$HOME' ''`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func brewTestBlocks(found engine.Finding, by string) []string {
	lines := []string{}
	for _, block := range found.Commands {
		if block.By == by {
			lines = append(lines, block.Lines...)
		}
	}
	return lines
}

const brewInventoryFixture = `{"formulae":[
	{"name":"first","full_name":"first","installed":[{"version":"1.0","installed_on_request":true,"runtime_dependencies":[{"full_name":"sample/tap/shared","pkg_version":"1.0"}]}]},
	{"name":"shared","full_name":"sample/tap/shared","linked_keg":"1.0","installed":[{"version":"1.0","installed_on_request":false,"runtime_dependencies":[]}]},
	{"name":"orphan","full_name":"orphan","keg_only":true,"pinned":true,"installed":[{"version":"1.0","installed_on_request":false,"runtime_dependencies":[]},{"version":"2.0","installed_on_request":false,"runtime_dependencies":[]}]}
],"casks":[{"token":"outside","full_token":"outside","installed":"2.0","depends_on":{"formula":["sample/tap/shared"],"macos":">= :sonoma"}}]}`

func TestBrewInventoryParsesFormulaeAndCasks(t *testing.T) {
	run := func(_ context.Context, program string, args ...string) ([]byte, error) {
		if program != "brew" || !reflect.DeepEqual(args, []string{"info", "--json=v2", "--installed"}) {
			t.Fatal("unexpected inventory command", program, args)
		}
		return []byte(brewInventoryFixture), nil
	}
	installed, err := brewInventory(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]brewPackage{
		"first":        {Name: "first", Version: "1.0", Source: "first", Dependencies: []string{"shared"}},
		"shared":       {Name: "shared", Version: "1.0", Source: "sample/tap/shared", Automatic: true, Healthy: true, Dependencies: []string{}},
		"orphan":       {Name: "orphan", Version: "1.0,2.0", Source: "orphan", Automatic: true, Held: true, Healthy: true, Dependencies: []string{}},
		"cask/outside": {Name: "cask/outside", Version: "2.0", Source: "outside", Healthy: true, Dependencies: []string{"shared"}},
	}
	if !reflect.DeepEqual(installed, want) {
		t.Fatalf("got %+v\nwant %+v", installed, want)
	}
}

func TestBrewMissingRuntimeMetadataUsesTheRuntimeQuery(t *testing.T) {
	data := strings.Replace(brewInventoryFixture, `"runtime_dependencies":[{"full_name":"sample/tap/shared","pkg_version":"1.0"}]`, `"runtime_dependencies":null`, 1)
	queried := false
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "info" {
			return []byte(data), nil
		}
		if !reflect.DeepEqual(args, []string{"deps", "--installed", "--formula", "first"}) {
			t.Fatal("not an actual runtime dependency query", args)
		}
		queried = true
		return []byte("sample/tap/shared\n"), nil
	}
	installed, err := brewInventory(context.Background(), run)
	if err != nil || !queried || !reflect.DeepEqual(installed["first"].Dependencies, []string{"shared"}) {
		t.Fatal(installed, err)
	}
}

func TestBrewInventoryRefusesMissingCollectionsAndAmbiguousIdentities(t *testing.T) {
	for _, data := range []string{`{}`, `{"formulae":[],"casks":null}`, `not json`, strings.Replace(brewInventoryFixture, `"name":"orphan","full_name":"orphan"`, `"name":"shared","full_name":"shared"`, 1), strings.ReplaceAll(brewInventoryFixture, "sample/tap/shared", "../../shared"), strings.Replace(brewInventoryFixture, `"installed":"2.0"`, `"installed":""`, 1)} {
		run := func(context.Context, string, ...string) ([]byte, error) { return []byte(data), nil }
		if _, err := brewInventory(context.Background(), run); err == nil {
			t.Fatal("invalid native inventory was accepted", data)
		}
	}
	run := func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("native refusal") }
	if _, err := brewInventory(context.Background(), run); err == nil {
		t.Fatal("a failed inspection gave an inventory")
	}
	for _, name := range []string{"", "a/b", "../x", "A", "tap/owner/name/extra"} {
		if _, err := brewShortName(name); err == nil {
			t.Fatal("invalid package identity accepted", name)
		}
	}
}

func TestBrewInstallsAMissingFormulaWithAPendingMarkAndARecord(t *testing.T) {
	f := newBrewFixture(t)
	git := brewTestFormula("tool.git", "git")
	f.installed["outside"] = brewPackage{Name: "outside", Source: "outside", Version: "1", Healthy: true}
	missing := f.inspect(git, engine.Want{Present: true})
	if missing.Status != engine.Missing || !missing.Pending || missing.Privileged || missing.Replan || !slices.Equal(brewTestBlocks(missing, "setup"), []string{f.line(false, "install", "--formula", "git")}) || len(brewTestBlocks(missing, "you")) != 0 {
		t.Fatal(missing)
	}
	f.onRun = func(args []string) error {
		f.installed["git"] = brewPackage{Name: "git", Source: "git", Version: "2.50", Healthy: true}
		return nil
	}
	if err := f.d.Apply(context.Background(), git, missing, f.rec); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.mutations(), [][]string{{"install", "--formula", "git"}}) {
		t.Fatal(f.mutations())
	}
	if shown := brewTestBlocks(missing, "setup"); len(shown) != 1 || shown[0] != macTestLine(f.runner.commands[0]) {
		t.Fatal("the approved line differs from the command that ran", shown, macTestLine(f.runner.commands[0]))
	}
	for _, control := range brewProcessControls {
		if !slices.Contains(f.runner.commands[0].Env, control) {
			t.Fatal("the brew command lacks a process control", control)
		}
	}
	if !slices.Contains(brewProcessControls, "HOMEBREW_NO_AUTOREMOVE=1") || !slices.Contains(brewProcessControls, "HOMEBREW_NO_AUTO_UPDATE=1") || !slices.Contains(brewProcessControls, "HOMEBREW_NO_INSTALL_CLEANUP=1") || !slices.Contains(brewProcessControls, "HOMEBREW_NO_INSTALL_UPGRADE=1") {
		t.Fatal("a process control that protects other packages is missing", brewProcessControls)
	}
	during := f.states[0]
	if !slices.Equal(during.Pending, []string{"tool.git:brew-install"}) || !during.HasSystem("brew", "git") {
		t.Fatal("the mark and the provisional record must exist before brew starts", during.Pending, during.System)
	}
	if st := f.rec.State(); len(st.Pending) != 0 || !st.HasSystem("brew", "git") || st.HasSystem("brew", "outside") {
		t.Fatal("unexpected records after the install", st.Pending, st.System)
	}
	if provider, name, ok := engine.SystemRecord(git, platform.Context{OS: "darwin", Arch: "arm64"}); !ok || !f.rec.State().HasSystem(provider, name) {
		t.Fatal("the engine cannot find the record", provider, name)
	}
	after := f.inspect(git, engine.Want{Present: true})
	if after.Status != engine.Satisfied || after.Version != "2.50" || after.Pending || len(after.Commands) != 0 {
		t.Fatal(after)
	}
}

func TestBrewMarkLifecycleOnFailure(t *testing.T) {
	git := brewTestFormula("tool.git", "git")
	f := newBrewFixture(t)
	f.onRun = func([]string) error { return errors.New("fork failed") }
	if err := f.apply(git, engine.Want{Present: true}); err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatal(err)
	}
	if st := f.rec.State(); len(st.Pending) != 0 || st.HasSystem("brew", "git") {
		t.Fatal("a command that did not start kept its mark or its provisional record", st.Pending, st.System)
	}
	f = newBrewFixture(t)
	f.onRun = func([]string) error { return macTestExit(1) }
	if err := f.apply(git, engine.Want{Present: true}); err == nil || !strings.Contains(err.Error(), "confirm that the command exited") {
		t.Fatal(err)
	}
	if st := f.rec.State(); !slices.Equal(st.Pending, []string{"tool.git:brew-install"}) || !st.HasSystem("brew", "git") {
		t.Fatal("a failed command lost its mark or its provisional record", st.Pending, st.System)
	}
	f = newBrewFixture(t)
	f.onRun = func([]string) error {
		f.installed["git"] = brewPackage{Name: "git", Source: "git", Version: "2.50"}
		return nil
	}
	if err := f.apply(git, engine.Want{Present: true}); err == nil || !strings.Contains(err.Error(), "activation checks") || len(f.rec.State().Pending) != 1 {
		t.Fatal("an unlinked new formula passed, or an unverified install cleared its mark", err)
	}
	f = newBrewFixture(t)
	missing := f.inspect(git, engine.Want{Present: true})
	f.installed["git"] = brewPackage{Name: "git", Source: "git", Version: "2.50", Healthy: true}
	if err := f.d.Apply(context.Background(), git, missing, f.rec); err == nil || len(f.runner.commands) != 0 || f.rec.State().HasSystem("brew", "git") {
		t.Fatal("a package that appeared after the preview got a record", err)
	}
}

func TestBrewUsesAPresentFormulaWithoutARecordAndNeverRepairsIt(t *testing.T) {
	f := newBrewFixture(t)
	tmux := brewTestFormula("tool.tmux", "tmux")
	f.installed["tmux"] = brewPackage{Name: "tmux", Source: "tmux", Version: "3.5", Healthy: true}
	f.outdated["tmux"] = "3.6"
	found := f.inspect(tmux, engine.Want{Present: true, Update: true})
	if found.Status != engine.Satisfied || len(found.Commands) != 0 {
		t.Fatal("an unrecorded formula must be used as it is", found)
	}
	for _, query := range f.queries {
		if slices.Contains(query, "outdated") {
			t.Fatal("setup asked for an update of a formula without a record")
		}
	}
	if !slices.ContainsFunc(f.queries, func(q []string) bool {
		return slices.Equal(q, []string{filepath.Join(f.location.Prefix, "bin", "tmux"), "-V"})
	}) {
		t.Fatal("the command check did not use the Homebrew prefix", f.queries)
	}
	f.checkErr = errors.New("dyld: library not loaded")
	found = f.inspect(tmux, engine.Want{Present: true})
	if found.Status != engine.Blocked || !strings.Contains(found.Reason, "setup did not install this package") || len(brewTestBlocks(found, "setup")) != 0 || !slices.Equal(brewTestBlocks(found, "you"), []string{f.line(false, "reinstall", "--formula", "tmux")}) {
		t.Fatal("direct repair of an unrecorded formula", found)
	}
	if err := f.d.Apply(context.Background(), tmux, engine.Finding{Status: engine.Unhealthy}, f.rec); err == nil || len(f.runner.commands) != 0 {
		t.Fatal("an unrecorded formula was repaired", err)
	}
	if err := f.d.Apply(context.Background(), tmux, engine.Finding{Status: engine.Outdated}, f.rec); err == nil || len(f.runner.commands) != 0 {
		t.Fatal("an unrecorded formula was updated", err)
	}
	if _, err := f.d.Inspect(context.Background(), brewTestFormula("tool.other", "other"), engine.Want{Present: true}, f.rec); err == nil || !strings.Contains(err.Error(), "lacks its native command health check") {
		t.Fatal("a formula without a command check was accepted", err)
	}
}

func TestBrewUpdatesAndRepairsOnlyARecordedFormula(t *testing.T) {
	f := newBrewFixture(t)
	git := brewTestFormula("tool.git", "git")
	f.installed["git"] = brewPackage{Name: "git", Source: "git", Version: "2.50", Healthy: true}
	if err := f.rec.Update(func(s *state.State) error { s.AddSystem("brew", "git"); return nil }); err != nil {
		t.Fatal(err)
	}
	if found := f.inspect(git, engine.Want{Present: true, Update: true}); found.Status != engine.Satisfied {
		t.Fatal(found)
	}
	f.outdated["git"] = "2.51"
	if found := f.inspect(git, engine.Want{Present: true}); found.Status != engine.Satisfied {
		t.Fatal("an Apply request must not ask Homebrew for updates", found)
	}
	outdated := f.inspect(git, engine.Want{Present: true, Update: true})
	if outdated.Status != engine.Outdated || !outdated.Pending || !slices.Equal(brewTestBlocks(outdated, "setup"), []string{f.line(false, "upgrade", "--formula", "git")}) || !strings.Contains(outdated.Reason, "2.51") {
		t.Fatal(outdated)
	}
	f.onRun = func(args []string) error {
		delete(f.outdated, "git")
		f.installed["git"] = brewPackage{Name: "git", Source: "git", Version: "2.51", Healthy: true}
		return nil
	}
	if err := f.d.Apply(context.Background(), git, outdated, f.rec); err != nil {
		t.Fatal(err)
	}
	if shown := brewTestBlocks(outdated, "setup"); len(shown) != 1 || shown[0] != macTestLine(f.runner.commands[0]) {
		t.Fatal("the approved line differs from the command that ran", shown)
	}
	if !reflect.DeepEqual(f.mutations(), [][]string{{"upgrade", "--formula", "git"}}) || !slices.Equal(f.states[0].Pending, []string{"tool.git:brew-update"}) || len(f.rec.State().Pending) != 0 {
		t.Fatal(f.mutations(), f.states[0].Pending)
	}
	if found := f.inspect(git, engine.Want{Present: true, Update: true}); found.Status != engine.Satisfied || found.Version != "2.51" {
		t.Fatal(found)
	}

	f.outdated["git"] = "2.52"
	pinned := f.installed["git"]
	pinned.Held = true
	f.installed["git"] = pinned
	found := f.inspect(git, engine.Want{Present: true, Update: true})
	if found.Status != engine.Satisfied || len(found.Notes) != 1 || !strings.Contains(found.Notes[0], "pinned") {
		t.Fatal("a pinned formula must keep its pin", found)
	}
	if err := f.d.Apply(context.Background(), git, engine.Finding{Status: engine.Outdated}, f.rec); err == nil || len(f.runner.commands) != 1 {
		t.Fatal("a pinned formula was updated", err)
	}

	f = newBrewFixture(t)
	f.installed["git"] = brewPackage{Name: "git", Source: "git", Version: "2.50"}
	if err := f.rec.Update(func(s *state.State) error { s.AddSystem("brew", "git"); return nil }); err != nil {
		t.Fatal(err)
	}
	unhealthy := f.inspect(git, engine.Want{Present: true})
	if unhealthy.Status != engine.Unhealthy || !slices.Equal(brewTestBlocks(unhealthy, "setup"), []string{f.line(false, "reinstall", "--formula", "git")}) {
		t.Fatal(unhealthy)
	}
	f.onRun = func(args []string) error {
		if args[0] == "link" {
			f.installed["git"] = brewPackage{Name: "git", Source: "git", Version: "2.50", Healthy: true}
		}
		return nil
	}
	if err := f.d.Apply(context.Background(), git, unhealthy, f.rec); err != nil {
		t.Fatal(err)
	}
	if shown := brewTestBlocks(unhealthy, "setup"); len(shown) != 1 || shown[0] != macTestLine(f.runner.commands[0]) {
		t.Fatal("the approved line differs from the command that ran", shown)
	}
	if !reflect.DeepEqual(f.mutations(), [][]string{{"reinstall", "--formula", "git"}, {"link", "--formula", "git"}}) || !slices.Equal(f.states[0].Pending, []string{"tool.git:brew-repair"}) || len(f.rec.State().Pending) != 0 {
		t.Fatal("a keg that reinstall keeps unlinked needs one link command", f.mutations())
	}
}

func TestBrewShowsTheRemovalBlockOnlyForARecordedItemAndNeverRemoves(t *testing.T) {
	f := newBrewFixture(t)
	git, tmux := brewTestFormula("tool.git", "git"), brewTestFormula("tool.tmux", "tmux")
	zen := catalog.Resource{ID: "app.zen", Name: "Zen", Action: "package", Bindings: map[string]catalog.Binding{"darwin": {Provider: "homebrew-cask", Package: "zen"}}}
	f.installed["git"] = brewPackage{Name: "git", Source: "git", Version: "2.50", Healthy: true}
	f.installed["tmux"] = brewPackage{Name: "tmux", Source: "tmux", Version: "3.5", Healthy: true}
	f.installed["cask/zen"] = brewPackage{Name: "cask/zen", Source: "zen", Version: "1", Healthy: true}
	if err := f.rec.Update(func(s *state.State) error { s.AddSystem("brew", "git"); s.AddSystem("cask", "zen"); return nil }); err != nil {
		t.Fatal(err)
	}
	absent := engine.Want{}
	recorded := f.inspect(git, absent)
	if recorded.Status != engine.Satisfied || !slices.Equal(brewTestBlocks(recorded, "you"), []string{f.line(false, "uninstall", "git")}) || len(brewTestBlocks(recorded, "setup")) != 0 || len(recorded.Notes) == 0 {
		t.Fatal(recorded)
	}
	if selected := f.inspect(git, engine.Want{Present: true}); len(selected.Commands) != 0 {
		t.Fatal("a needed item shows a removal command", selected)
	}
	if unrecorded := f.inspect(tmux, absent); unrecorded.Status != engine.Satisfied || len(unrecorded.Commands) != 0 {
		t.Fatal("an unrecorded item shows a removal command", unrecorded)
	}
	cask := f.inspect(zen, absent)
	if cask.Status != engine.Satisfied || !slices.Equal(brewTestBlocks(cask, "you"), []string{f.line(true, "uninstall", "--cask", "zen")}) {
		t.Fatal(cask)
	}
	for _, r := range []catalog.Resource{git, zen} {
		if err := f.d.Remove(context.Background(), r, f.inspect(r, absent), f.rec); err == nil || !strings.Contains(err.Error(), "setup never removes a system package") {
			t.Fatal("a present package lost its record", r.ID, err)
		}
	}
	if st := f.rec.State(); !st.HasSystem("brew", "git") || !st.HasSystem("cask", "zen") || len(f.runner.commands) != 0 {
		t.Fatal("Remove changed the system or the records", st.System, f.mutations())
	}
	delete(f.installed, "git")
	delete(f.installed, "cask/zen")
	for _, r := range []catalog.Resource{git, zen} {
		gone := f.inspect(r, absent)
		if gone.Status != engine.Absent || len(gone.Commands) != 0 {
			t.Fatal(gone)
		}
		if err := f.d.Remove(context.Background(), r, gone, f.rec); err != nil {
			t.Fatal(err)
		}
	}
	if st := f.rec.State(); st.HasSystem("brew", "git") || st.HasSystem("cask", "zen") || len(f.runner.commands) != 0 {
		t.Fatal("the record of an absent package stayed, or Remove ran a command", st.System)
	}
}

func brewMaintenanceFixture(t *testing.T, recorded bool) *brewFixture {
	t.Helper()
	f := newBrewFixture(t)
	f.installed["shared"] = brewPackage{Name: "shared", Source: "shared", Version: "1.0", Automatic: true, Healthy: true}
	f.installed["consumer"] = brewPackage{Name: "consumer", Source: "sample/tap/consumer", Version: "1.0", Healthy: true, Dependencies: []string{"shared"}}
	f.installed["unrelated"] = brewPackage{Name: "unrelated", Source: "unrelated", Version: "1.0", Healthy: true}
	f.installed["cask/app"] = brewPackage{Name: "cask/app", Source: "app", Version: "1.0", Healthy: true, Dependencies: []string{"shared"}}
	if recorded {
		if err := f.rec.Update(func(s *state.State) error { s.AddSystem("brew", "sample/tap/consumer"); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	f.onRun = func(args []string) error {
		switch {
		case args[0] == "install":
			f.installed["git"] = brewPackage{Name: "git", Source: "git", Version: "2.50", Healthy: true, Dependencies: []string{"shared"}}
			shared := f.installed["shared"]
			shared.Version = "2.0"
			f.installed["shared"] = shared
			f.broken["sample/tap/consumer"] = true
		case slices.Equal(args, []string{"reinstall", "--formula", "sample/tap/consumer"}):
			delete(f.broken, "sample/tap/consumer")
		default:
			t.Fatal("unexpected mutation", args)
		}
		return nil
	}
	return f
}

func TestBrewMaintenanceReinstallsOnlyABrokenRecordedConsumer(t *testing.T) {
	git := brewTestFormula("tool.git", "git")
	f := brewMaintenanceFixture(t, true)
	if err := f.apply(git, engine.Want{Present: true}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.mutations(), [][]string{{"install", "--formula", "git"}, {"reinstall", "--formula", "sample/tap/consumer"}}) {
		t.Fatal(f.mutations())
	}
	if !slices.Equal(f.states[1].Pending, []string{"tool.git:brew-install"}) || len(f.rec.State().Pending) != 0 {
		t.Fatal("the mark must cover the maintenance of the consumers", f.states[1].Pending)
	}
	if st := f.rec.State(); st.HasSystem("brew", "shared") {
		t.Fatal("a dependency got a record", st.System)
	}
	checked := false
	for _, query := range f.queries {
		if len(query) > 3 && query[1] == "linkage" {
			checked = true
			for _, source := range query[3:] {
				if source == "unrelated" || source == "app" || strings.HasPrefix(source, "cask/") {
					t.Fatal("the linkage check went beyond the affected formulae", query)
				}
			}
		}
	}
	if !checked {
		t.Fatal("no linkage check ran")
	}

	for _, change := range []string{"cask-version", "still-broken", "pinned"} {
		f = newBrewFixture(t)
		f.installed["shared"] = brewPackage{Name: "shared", Source: "shared", Version: "1.0", Automatic: true, Healthy: true}
		f.installed["consumer"] = brewPackage{Name: "consumer", Source: "consumer", Version: "1.0", Healthy: true, Held: change == "pinned", Dependencies: []string{"shared"}}
		f.installed["cask/app"] = brewPackage{Name: "cask/app", Source: "app", Version: "1.0", Healthy: true, Dependencies: []string{"shared"}}
		if err := f.rec.Update(func(s *state.State) error { s.AddSystem("brew", "consumer"); return nil }); err != nil {
			t.Fatal(err)
		}
		f.onRun = func(args []string) error {
			if args[0] == "install" {
				f.installed["git"] = brewPackage{Name: "git", Source: "git", Version: "2.50", Healthy: true, Dependencies: []string{"shared"}}
				shared := f.installed["shared"]
				shared.Version = "2.0"
				f.installed["shared"] = shared
				if change == "cask-version" {
					f.installed["cask/app"] = brewPackage{Name: "cask/app", Source: "app", Version: "2.0", Healthy: true, Dependencies: []string{"shared"}}
				} else {
					f.broken["consumer"] = true
				}
			}
			return nil
		}
		err := f.apply(git, engine.Want{Present: true})
		if err == nil || !slices.Equal(f.rec.State().Pending, []string{"tool.git:brew-install"}) {
			t.Fatal("unverified maintenance cleared the mark", change, err)
		}
		wantCommands := map[string]int{"cask-version": 1, "still-broken": 2, "pinned": 1}[change]
		if len(f.runner.commands) != wantCommands {
			t.Fatal(change, f.mutations(), err)
		}
	}
}

func TestBrewMaintenanceReportsABrokenUnrecordedConsumerAndNeverRepairsIt(t *testing.T) {
	f := brewMaintenanceFixture(t, false)
	err := f.apply(brewTestFormula("tool.git", "git"), engine.Want{Present: true})
	line := f.line(false, "reinstall", "--formula", "sample/tap/consumer")
	if err == nil || !strings.Contains(err.Error(), "consumer") || !strings.Contains(err.Error(), "broken linkage of sample/tap/consumer") || !strings.Contains(err.Error(), line) {
		t.Fatal("the report must name the formula, the fault and the copy-ready command", err)
	}
	var report *BrewDependentError
	if !errors.As(err, &report) || report.Name != "consumer" || report.Block.By != "you" || !slices.Equal(report.Block.Lines, []string{line}) {
		t.Fatal("the report lacks its copy-ready command block for the operator", report)
	}
	if !reflect.DeepEqual(f.mutations(), [][]string{{"install", "--formula", "git"}}) {
		t.Fatal("setup repaired a formula without a record", f.mutations())
	}
	if st := f.rec.State(); !slices.Equal(st.Pending, []string{"tool.git:brew-install"}) || st.HasSystem("brew", "sample/tap/consumer") || st.HasSystem("brew", "consumer") {
		t.Fatal("the stopped step cleared its mark or recorded the consumer", st.Pending, st.System)
	}
}

func TestBrewReportsABrokenDependentUntilALinkageCheckPassesAfterTheMarkIsCleared(t *testing.T) {
	git := brewTestFormula("tool.git", "git")
	for _, recorded := range []bool{false, true} {
		f := brewMaintenanceFixture(t, recorded)
		install, repairs := f.onRun, 0
		f.onRun = func(args []string) error {
			if args[0] == "reinstall" {
				if repairs++; repairs == 1 {
					return nil
				}
			}
			return install(args)
		}
		if err := f.apply(git, engine.Want{Present: true}); err == nil || !slices.Equal(f.rec.State().Pending, []string{"tool.git:brew-install"}) {
			t.Fatal("a broken dependent passed, or the step cleared its mark", recorded, err)
		}
		line := f.line(false, "reinstall", "--formula", "sample/tap/consumer")
		by := map[bool]string{false: "you", true: "setup"}[recorded]
		var found engine.Finding
		for _, confirmed := range []bool{false, true} {
			if confirmed {
				if err := f.rec.Update(func(s *state.State) error { s.Clear("tool.git:brew-install"); return nil }); err != nil {
					t.Fatal(err)
				}
			}
			found = f.inspect(git, engine.Want{Present: true})
			if found.Status != engine.Unhealthy || !strings.Contains(found.Reason, "consumer") || !slices.Equal(brewTestBlocks(found, by), []string{line}) || len(found.Commands) != 1 {
				t.Fatal("the broken dependent is not reported", recorded, confirmed, found)
			}
		}
		err := f.d.Apply(context.Background(), git, found, f.rec)
		if recorded {
			if err != nil || !reflect.DeepEqual(f.mutations(), [][]string{{"install", "--formula", "git"}, {"reinstall", "--formula", "sample/tap/consumer"}, {"reinstall", "--formula", "sample/tap/consumer"}}) || len(f.rec.State().Pending) != 0 {
				t.Fatal("setup did not repair its broken dependent", err, f.mutations(), f.rec.State().Pending)
			}
		} else {
			var report *BrewDependentError
			if !errors.As(err, &report) || report.Name != "consumer" || len(f.runner.commands) != 1 {
				t.Fatal("setup repaired a formula without a record, or lost the report", err, f.mutations())
			}
			if still := f.inspect(git, engine.Want{Present: true}); still.Status != engine.Unhealthy || !strings.Contains(still.Reason, "consumer") {
				t.Fatal("the report ended before a linkage check passed", still)
			}
			delete(f.broken, "sample/tap/consumer")
			if err := f.apply(git, engine.Want{Present: true}); err != nil || len(f.runner.commands) != 1 || len(f.rec.State().Linkage) != 0 {
				t.Fatal("the passing check after the repair was not recorded", err, f.mutations(), f.rec.State().Linkage)
			}
		}
		if healthy := f.inspect(git, engine.Want{Present: true}); healthy.Status != engine.Satisfied || len(healthy.Commands) != 0 {
			t.Fatal("a healthy dependent still blocks the formula", recorded, healthy)
		}
	}
}

func brewDueFixture(t *testing.T) *brewFixture {
	t.Helper()
	f := brewMaintenanceFixture(t, false)
	f.installed["git"] = brewPackage{Name: "git", Source: "git", Version: "2.50", Healthy: true, Dependencies: []string{"shared"}}
	if err := f.rec.Update(func(s *state.State) error {
		s.AddSystem("brew", "git")
		s.Linkage = []string{"tool.git"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestBrewClearsADueLinkageCheckThatPassesWithoutAReinstall(t *testing.T) {
	git := brewTestFormula("tool.git", "git")
	f := brewDueFixture(t)
	f.broken["unrelated"] = true
	due := f.inspect(git, engine.Want{Present: true})
	if due.Status != engine.Unhealthy || !strings.Contains(due.Reason, "linkage check of its dependents is due") || len(due.Commands) != 0 {
		t.Fatal("a due linkage check is not reported", due)
	}
	if err := f.d.Apply(context.Background(), git, due, f.rec); err != nil {
		t.Fatal(err)
	}
	if len(f.runner.commands) != 0 {
		t.Fatal("setup changed a healthy formula to record a passing check", f.mutations())
	}
	if st := f.rec.State(); len(st.Linkage) != 0 || len(st.Pending) != 0 {
		t.Fatal("the passing check stayed due", st.Linkage, st.Pending)
	}
	if healthy := f.inspect(git, engine.Want{Present: true}); healthy.Status != engine.Satisfied || len(healthy.Commands) != 0 {
		t.Fatal("a recorded passing check still makes the formula unhealthy", healthy)
	}
	for _, query := range f.queries {
		if len(query) > 3 && query[1] == "linkage" && slices.Contains(query[3:], "unrelated") {
			t.Fatal("the linkage check went beyond the formulae that the step could change", query)
		}
	}
}

func TestBrewReportsABrokenDependentOfADeselectedFormulaUntilACheckPasses(t *testing.T) {
	git := brewTestFormula("tool.git", "git")
	f := brewDueFixture(t)
	f.broken["sample/tap/consumer"] = true
	line := f.line(false, "reinstall", "--formula", "sample/tap/consumer")
	blocked := f.inspect(git, engine.Want{})
	if blocked.Status != engine.Blocked || !strings.Contains(blocked.Reason, "consumer") || !slices.Equal(brewTestBlocks(blocked, "you"), []string{line}) {
		t.Fatal("a deselected formula hides its broken dependent", blocked)
	}
	delete(f.broken, "sample/tap/consumer")
	if kept := f.inspect(git, engine.Want{}); kept.Status != engine.Satisfied {
		t.Fatal("a repaired dependent still blocks the deselected formula", kept)
	}
	delete(f.installed, "git")
	absent := f.inspect(git, engine.Want{})
	if absent.Status != engine.Absent {
		t.Fatal(absent)
	}
	if err := f.d.Remove(context.Background(), git, absent, f.rec); err != nil {
		t.Fatal(err)
	}
	if st := f.rec.State(); len(st.Linkage) != 0 || st.HasSystem("brew", "git") {
		t.Fatal("the release kept the record or the due check", st.Linkage, st.System)
	}
	if len(f.runner.commands) != 0 {
		t.Fatal("setup changed a deselected formula", f.mutations())
	}
}

func TestBrewWithoutHomebrewIsUnknownAndFindsANewHomebrew(t *testing.T) {
	f := newBrewFixture(t)
	git := brewTestFormula("tool.git", "git")
	f.discovered = false
	for _, want := range []engine.Want{{Present: true}, {}} {
		found := f.inspect(git, want)
		if found.Status != engine.Unknown || !strings.Contains(found.Reason, "install Homebrew") || len(f.queries) != 0 {
			t.Fatal("a missing Homebrew must never prove absence", found)
		}
	}
	if err := f.d.Apply(context.Background(), git, engine.Finding{Status: engine.Missing}, f.rec); err == nil || len(f.runner.commands) != 0 {
		t.Fatal(err)
	}
	if err := f.d.Remove(context.Background(), git, engine.Finding{Status: engine.Absent}, f.rec); err == nil {
		t.Fatal("a record was deleted without an inventory")
	}
	f.discovered = true
	if found := f.inspect(git, engine.Want{Present: true}); found.Status != engine.Missing {
		t.Fatal("the provider did not find the new Homebrew", found)
	}
	f.d = NewBrew(f.d.Home)
	f.d.Discover = func(context.Context) (*HomebrewLocation, error) { return nil, errors.New("brew --prefix failed") }
	if found := f.inspect(git, engine.Want{Present: true}); found.Status != engine.Unknown || found.Reason != "brew --prefix failed" {
		t.Fatal(found)
	}
	if _, err := f.d.Inspect(context.Background(), catalog.Resource{ID: "tool.other", Bindings: map[string]catalog.Binding{"darwin": {Provider: "archive", Package: "tool.other"}}}, engine.Want{Present: true}, f.rec); err != nil {
		t.Fatal("without Homebrew the binding is not read", err)
	}
}
