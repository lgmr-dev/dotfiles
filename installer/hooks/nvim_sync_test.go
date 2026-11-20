package hooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/builds"
	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

type nvimFixture struct {
	*sentinelEnv
	provider *hooks.NvimSync
	checkout string
	phases   []platform.Command
	checks   []platform.Command
	phase    func(step int, runtime string) error
	check    error
}

var nvimResource = catalog.Resource{ID: hooks.NvimResource, Action: "nvim-sync"}

func nvimEnvironment(c platform.Command, name string) string {
	value := ""
	for _, entry := range c.Env {
		if rest, ok := strings.CutPrefix(entry, name+"="); ok {
			value = rest
		}
	}
	return value
}

func (f *nvimFixture) Run(_ context.Context, c platform.Command) ([]byte, error) {
	f.phases = append(f.phases, c)
	runtime := nvimEnvironment(c, "DOTFILES_NVIM_RUNTIME")
	if !strings.HasPrefix(runtime, f.state+string(filepath.Separator)) {
		f.t.Fatal("a phase left the state folder", runtime)
	}
	if f.phase != nil {
		if err := f.phase(len(f.phases), runtime); err != nil {
			return nil, err
		}
	}
	return nil, os.WriteFile(filepath.Join(runtime, "owned"), []byte("locked runtime"), 0600)
}

func (f *nvimFixture) query(_ context.Context, c platform.Command) ([]byte, error) {
	f.checks = append(f.checks, c)
	if !slices.Contains(c.Args, "--clean") || slices.Contains(c.Args, "-u") || !slices.Contains(c.Args, "+lua require('util.sync_check').run()") {
		f.t.Fatal("the inspection can run an install", c.Args)
	}
	if !slices.Contains(c.Env, "PIP_CONFIG_FILE="+strings.ToLower(os.DevNull)) || !slices.Contains(c.Env, "DOTFILES_NVIM_SYNC=") || slices.Contains(c.Env, "DOTFILES_NVIM_SYNC=1") {
		f.t.Fatal("the inspection got provisioning authority or outside pip configuration", c.Env)
	}
	return nil, f.check
}

func (f *nvimFixture) tool(id, version string, pin catalog.ArchivePin) string {
	f.t.Helper()
	directory := filepath.Join(f.state, "tools", id)
	build := filepath.Join(directory, version+"-"+builds.Digest(pin, nil)[:12]+"-1")
	sentinelWrite(f.t, filepath.Join(build, "bin", "tool"), "#!/bin/sh\n", 0755)
	manifest, err := json.Marshal(builds.Manifest{Schema: 1, Resource: id, Version: version, Digest: builds.Digest(pin, nil), Tree: platform.Snapshot{Kind: "directory", Hash: strings.Repeat("0", 64)}})
	if err != nil {
		f.t.Fatal(err)
	}
	sentinelWrite(f.t, build+".manifest.json", string(manifest), 0600)
	current := filepath.Join(directory, "current")
	if err := os.Remove(current); err != nil && !errors.Is(err, os.ErrNotExist) {
		f.t.Fatal(err)
	}
	if err := os.Symlink(build, current); err != nil {
		f.t.Fatal(err)
	}
	return build
}

var (
	nvimPin = catalog.ArchivePin{Version: "0.12.0", SHA256: strings.Repeat("a", 64), Commands: map[string]string{"nvim": "bin/nvim"}, BinDirs: []string{"bin"}}
	nodePin = catalog.ArchivePin{Version: "24.0.0", SHA256: strings.Repeat("b", 64), Commands: map[string]string{"node": "bin/node"}, BinDirs: []string{"bin"}}
)

func newNvimFixture(t *testing.T) *nvimFixture {
	t.Helper()
	e := newSentinelEnv(t)
	f := &nvimFixture{sentinelEnv: e, checkout: filepath.Join(e.home, "repository")}
	sentinelWrite(t, filepath.Join(f.checkout, "nvim", "init.lua"), "-- fixture\n", 0644)
	sentinelWrite(t, filepath.Join(f.checkout, "nvim", "lazy-lock.json"), "{}\n", 0644)
	sentinelWrite(t, filepath.Join(f.checkout, "nvim", "mason-registry", "registry.json"), "[]\n", 0644)
	runtime, err := hooks.NvimRuntimeDirectory(platform.Context{OS: "linux", Arch: "amd64"}, platform.Folders{Home: e.home})
	if err != nil {
		t.Fatal(err)
	}
	f.provider = &hooks.NvimSync{Context: platform.Context{OS: "linux", Arch: "amd64"}, Checkout: f.checkout, StateDir: e.state, RuntimeDirectory: runtime,
		Pins: map[string]catalog.ArchivePin{hooks.NvimExecutable: nvimPin, "tool.node": nodePin}, Tools: []string{hooks.NvimExecutable, "tool.node"},
		Directories: []string{"/usr/bin", "/bin"}, Runner: f, Query: f.query}
	f.tool(hooks.NvimExecutable, "0.12.0", nvimPin)
	f.tool("tool.node", "24.0.0", nodePin)
	return f
}

func (f *nvimFixture) inspect(present bool) engine.Finding {
	f.t.Helper()
	return desktopInspect(f.t, f.provider, f.sentinelEnv, hooks.NvimResource, present)
}

func (f *nvimFixture) apply() error {
	f.t.Helper()
	return f.provider.Apply(context.Background(), nvimResource, f.inspect(true), f)
}

func (f *nvimFixture) current() string {
	f.t.Helper()
	target, err := os.Readlink(filepath.Join(f.state, "tools", hooks.NvimResource, "current"))
	if err != nil {
		f.t.Fatal(err)
	}
	return target
}

func (f *nvimFixture) public() string {
	return filepath.Join(f.home, ".local", "share", "nvim", "dotfiles-runtime", hooks.NvimResource, "current")
}

func (f *nvimFixture) runtimes() []string {
	entries, err := os.ReadDir(filepath.Join(f.state, "tools", hooks.NvimResource))
	if err != nil {
		return nil
	}
	names := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}

func TestNvimSyncRunsTheLockedPhasesInANewBuildAndPublishesBothLinks(t *testing.T) {
	f := newNvimFixture(t)
	if finding := f.inspect(true); finding.Status != engine.Missing || len(f.phases) != 0 || len(f.checks) != 0 || len(f.runtimes()) != 0 {
		t.Fatal("the inspection of a missing runtime ran a command", finding)
	}
	if err := f.apply(); err != nil {
		t.Fatal(err)
	}
	build := f.current()
	if !regexp.MustCompile(`^[0-9a-f]{12}-[0-9a-f]{12}-1$`).MatchString(filepath.Base(build)) || filepath.Dir(build) != filepath.Join(f.state, "tools", hooks.NvimResource) {
		t.Fatal("the runtime is not a private build", build)
	}
	if len(f.phases) != 3 || len(f.checks) != 1 {
		t.Fatal("unexpected commands", len(f.phases), len(f.checks))
	}
	nvim := filepath.Join(f.state, "tools", hooks.NvimExecutable, "0.12.0-"+builds.Digest(nvimPin, nil)[:12]+"-1")
	node := filepath.Join(f.state, "tools", "tool.node", "24.0.0-"+builds.Digest(nodePin, nil)[:12]+"-1")
	config := filepath.Join(f.checkout, "nvim")
	prefix := []string{"--headless", "-i", "NONE", "--cmd", "lua vim.opt.rtp:prepend(vim.env.DOTFILES_NVIM_CONFIG)", "-u", filepath.Join(config, "init.lua")}
	steps := [][]string{
		{"+Lazy! restore", "+lua if vim.v.errmsg ~= '' then vim.cmd('cquit 1') end", "+qa"},
		{"+lua require('lazy').load({ plugins = { 'nvim-treesitter' } })", "+lua if vim.v.errmsg ~= '' then vim.cmd('cquit 1') end", "+qa"},
		{"+lua require('util.mason_tools').run_checked('MasonToolsInstallSync')", "+cquit 1"},
	}
	for i, c := range f.phases {
		if c.Path != filepath.Join(nvim, "bin", "nvim") || !slices.Equal(c.Args, append(slices.Clone(prefix), steps[i]...)) {
			t.Fatal("unexpected phase command", i, c.Path, c.Args)
		}
		for name, want := range map[string]string{
			"PATH":                 strings.Join([]string{filepath.Join(nvim, "bin"), filepath.Join(node, "bin"), "/usr/bin", "/bin"}, string(os.PathListSeparator)),
			"DOTFILES_NVIM_CONFIG": config, "DOTFILES_NVIM_RUNTIME": build, "DOTFILES_NVIM_LOCKFILE": filepath.Join(build, ".lazy-lock-0.json"), "DOTFILES_NVIM_SYNC": "1",
			"XDG_CONFIG_HOME": f.checkout, "HOME": filepath.Join(build, ".home"), "XDG_DATA_HOME": filepath.Join(build, ".data"), "XDG_STATE_HOME": filepath.Join(build, ".state"),
			"XDG_CACHE_HOME": filepath.Join(build, ".cache"), "CARGO_HOME": filepath.Join(build, ".cargo"), "RUSTUP_HOME": filepath.Join(build, ".rustup"), "NVIM_APPNAME": "nvim",
			"PIP_CONFIG_FILE": strings.ToLower(os.DevNull), "PYTHONDONTWRITEBYTECODE": "1", "VIMINIT": "", "NPM_CONFIG_USERCONFIG": filepath.Join(build, ".home", ".npmrc"),
			"DOTFILES_TREESITTER_SYNC_INSTALL": map[bool]string{true: "1"}[i == 1],
		} {
			if got := nvimEnvironment(c, name); got != want {
				t.Fatal("unexpected phase environment", i, name, got)
			}
		}
	}
	if f.checks[0].Path != f.phases[0].Path || nvimEnvironment(f.checks[0], "DOTFILES_NVIM_RUNTIME") != build || nvimEnvironment(f.checks[0], "PYTHONDONTWRITEBYTECODE") != "1" {
		t.Fatal("the candidate was not checked from its final path", f.checks[0])
	}
	if sentinelRead(t, filepath.Join(build, ".lazy-lock-0.json")) != "{}\n" {
		t.Fatal("the private lock differs from the reviewed lock")
	}
	var manifest builds.Manifest
	if err := json.Unmarshal([]byte(sentinelRead(t, build+".manifest.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Resource != hooks.NvimResource || !slices.Equal(manifest.Uses, []string{nvim, node}) || manifest.Tree.Kind != "directory" || !strings.HasPrefix(filepath.Base(build), manifest.Version+"-"+manifest.Digest[:12]+"-") {
		t.Fatal("unexpected manifest", manifest)
	}
	if target, err := os.Readlink(f.public()); err != nil || target != build {
		t.Fatal("the public runtime link does not point at the new build", target, err)
	}
	st := f.State()
	if record := st.Targets[f.public()]; record.Kind != "link" || record.Resource != hooks.NvimResource || record.Applied == "" || record.Next != "" {
		t.Fatal("the public runtime link is not a recorded link target", record)
	}
	if record := st.Targets[filepath.Join(f.state, "tools", hooks.NvimResource, "current")]; record.Kind != "link" || record.Applied != build {
		t.Fatal("the current link has no record", record)
	}
	if finding := f.inspect(true); finding.Status != engine.Satisfied || len(f.phases) != 3 || len(f.checks) != 2 {
		t.Fatal("the published runtime is not satisfied", finding)
	}
	if left, err := builds.Leftovers(context.Background(), f.state, st); err != nil || len(left) != 0 {
		t.Fatal("the sweep does not know the runtime", left, err)
	}
	if err := os.Remove(f.public()); err != nil {
		t.Fatal(err)
	}
	if finding := f.inspect(true); finding.Status != engine.Unhealthy || !strings.Contains(finding.Reason, "public runtime link") {
		t.Fatal("a missing public link is not reported", finding)
	}
	if err := f.apply(); err != nil || len(f.phases) != 3 || f.current() != build {
		t.Fatal("the repair of the link built a new runtime", err, len(f.phases))
	}
}

func TestNvimSyncFollowsTheConfigurationTheLockTheRegistryAndTheToolBuilds(t *testing.T) {
	f := newNvimFixture(t)
	if err := f.apply(); err != nil {
		t.Fatal(err)
	}
	seen := []string{f.current()}
	changes := map[string]func(){
		"configuration": func() { sentinelWrite(t, filepath.Join(f.checkout, "nvim", "init.lua"), "-- edited\n", 0644) },
		"lock":          func() { sentinelWrite(t, filepath.Join(f.checkout, "nvim", "lazy-lock.json"), "{\"a\":1}\n", 0644) },
		"registry": func() {
			sentinelWrite(t, filepath.Join(f.checkout, "nvim", "mason-registry", "registry.json"), "[1]\n", 0644)
		},
		"tool build": func() {
			pin := nodePin
			pin.SHA256 = strings.Repeat("c", 64)
			f.provider.Pins["tool.node"] = pin
			f.tool("tool.node", "24.1.0", pin)
		},
	}
	for _, name := range []string{"configuration", "lock", "registry", "tool build"} {
		changes[name]()
		finding := f.inspect(true)
		if finding.Status != engine.Unhealthy || finding.Reason != "Neovim runtime does not match the current configuration" {
			t.Fatal("a changed input is not reported", name, finding)
		}
		if err := f.provider.Apply(context.Background(), nvimResource, finding, f); err != nil {
			t.Fatal(name, err)
		}
		build := f.current()
		if slices.Contains(seen, build) || !sentinelExists(seen[len(seen)-1]) {
			t.Fatal("the update did not use a new build, or it deleted the earlier build", name, build)
		}
		if target, _ := os.Readlink(f.public()); target != build {
			t.Fatal("the public runtime link stays at the earlier build", name)
		}
		seen = append(seen, build)
		if after := f.inspect(true); after.Status != engine.Satisfied {
			t.Fatal(name, after)
		}
	}
	if len(f.phases) != 15 {
		t.Fatal("unexpected phase count", len(f.phases))
	}
	pin := nodePin
	pin.SHA256 = strings.Repeat("d", 64)
	f.provider.Pins["tool.node"] = pin
	if finding := f.inspect(true); finding.Status != engine.Outdated || finding.Reason != "the build of tool.node changes with its reviewed pin" {
		t.Fatal("a reviewed pin change of a tool is not reported", finding)
	}
}

func TestNvimSyncFailedPhaseLeavesAnUnfinishedBuildAndTheNextRunUsesANewFolder(t *testing.T) {
	f := newNvimFixture(t)
	f.phase = func(step int, _ string) error {
		if step == 2 {
			return errors.New("fixture: parser install failed")
		}
		return nil
	}
	err := f.apply()
	if err == nil || !strings.Contains(err.Error(), "fixture: parser install failed") || !strings.Contains(err.Error(), "the unfinished runtime stays at") || len(f.phases) != 2 {
		t.Fatal(err, len(f.phases))
	}
	first := f.runtimes()
	if len(first) != 1 || !strings.HasSuffix(first[0], "-1") || sentinelExists(filepath.Join(f.state, "tools", hooks.NvimResource, first[0]+".manifest.json")) ||
		sentinelExists(f.public()) || len(f.State().Targets) != 0 {
		t.Fatal("a failed build was published", first, f.State().Targets)
	}
	if finding := f.inspect(true); finding.Status != engine.Missing {
		t.Fatal(finding)
	}
	f.phase = nil
	if err := f.apply(); err != nil {
		t.Fatal(err)
	}
	if build := f.current(); !strings.HasSuffix(build, "-2") || len(f.runtimes()) != 2 || len(f.phases) != 5 {
		t.Fatal("the next run did not use a new folder", build, f.runtimes())
	}
	if left, err := builds.Leftovers(context.Background(), f.state, f.State()); err != nil || !slices.Equal(left, []string{filepath.Join(f.state, "tools", hooks.NvimResource, first[0])}) {
		t.Fatal("check does not list the unfinished build", left, err)
	}
}

func TestNvimSyncDoesNotPublishACandidateThatFailsItsCheckOrASourceThatChanged(t *testing.T) {
	f := newNvimFixture(t)
	f.check = errors.New("fixture: a parser is missing")
	if err := f.apply(); err == nil || !strings.Contains(err.Error(), "fixture: a parser is missing") || sentinelExists(f.public()) || len(f.State().Targets) != 0 {
		t.Fatal("a candidate that failed its check was published", err)
	}
	f.check = nil
	f.phase = func(step int, _ string) error {
		if step == 6 {
			sentinelWrite(t, filepath.Join(f.checkout, "nvim", "init.lua"), "-- edited during the sync\n", 0644)
		}
		return nil
	}
	if err := f.apply(); err == nil || !strings.Contains(err.Error(), "Neovim source changed during synchronization") || sentinelExists(f.public()) {
		t.Fatal("a runtime of an earlier source was published", err)
	}
	f.phase = nil
	if err := f.apply(); err != nil {
		t.Fatal(err)
	}
	f.check = errors.New("fixture: verification failed")
	if finding := f.inspect(true); finding.Status != engine.Unhealthy || !strings.Contains(finding.Reason, "fixture: verification failed") {
		t.Fatal("a failed verification is not a health issue", finding)
	}
	f.check = nil
	sentinelWrite(t, filepath.Join(f.current(), "lazy", "plugin.lua"), "-- added later\n", 0644)
	if finding := f.inspect(true); finding.Status != engine.Unhealthy || finding.Reason != "the current Neovim runtime differs from its manifest" {
		t.Fatal("a changed runtime is not reported", finding)
	}
	if finding := f.inspect(false); finding.Status != engine.Changed || !slices.Equal(finding.Preserved, []string{f.current()}) {
		t.Fatal("a changed runtime is not preserved at removal", finding)
	}
}

func TestNvimSyncRefusesAPreexistingRuntimeAndARedirectedParent(t *testing.T) {
	f := newNvimFixture(t)
	foreign := filepath.Join(f.home, "foreign runtime")
	sentinelWrite(t, filepath.Join(foreign, "owned"), "personal", 0600)
	current := filepath.Join(f.state, "tools", hooks.NvimResource, "current")
	if err := os.MkdirAll(filepath.Dir(current), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, current); err != nil {
		t.Fatal(err)
	}
	finding := f.inspect(true)
	if finding.Status != engine.Blocked || !strings.Contains(finding.Reason, "refusing to acquire a pre-existing Neovim runtime") {
		t.Fatal("a pre-existing runtime link is not blocked", finding)
	}
	if err := f.provider.Apply(context.Background(), nvimResource, finding, f); err == nil || len(f.phases) != 0 {
		t.Fatal("setup acquired a pre-existing runtime", err)
	}
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	sentinelWrite(t, f.public(), "personal", 0600)
	if finding := f.inspect(true); finding.Status != engine.Changed || !slices.Equal(finding.Preserved, []string{f.public()}) {
		t.Fatal("personal content at the public link is not preserved", finding)
	}
	if err := os.Remove(f.public()); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(f.home, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.home, ".local", "share", "nvim")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(f.home, ".local", "share", "nvim")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.provider.Inspect(context.Background(), nvimResource, engine.Want{Present: true}, f); err == nil || !strings.Contains(err.Error(), "Neovim runtime parent was redirected") {
		t.Fatal("a redirected runtime parent was accepted", err)
	}
}

func TestNvimSyncRemovalReleasesTheLinksAndKeepsTheRuntimeForTheSweep(t *testing.T) {
	f := newNvimFixture(t)
	if finding := f.inspect(false); finding.Status != engine.Absent {
		t.Fatal(finding)
	}
	if err := f.apply(); err != nil {
		t.Fatal(err)
	}
	build := f.current()
	personal := filepath.Join(filepath.Dir(filepath.Dir(f.public())), "personal.txt")
	sentinelWrite(t, personal, "personal", 0600)
	finding := f.inspect(false)
	if finding.Status != engine.Satisfied {
		t.Fatal(finding)
	}
	if err := f.provider.Remove(context.Background(), nvimResource, finding, f); err != nil {
		t.Fatal(err)
	}
	if sentinelExists(f.public()) || sentinelExists(filepath.Join(f.state, "tools", hooks.NvimResource, "current")) || len(f.State().Targets) != 0 || !sentinelExists(build) || sentinelRead(t, personal) != "personal" {
		t.Fatal("the removal left a link or a record, or deleted the runtime or personal content", f.State().Targets)
	}
	if after := f.inspect(false); after.Status != engine.Absent {
		t.Fatal(after)
	}
	removed, left, err := builds.Sweep(context.Background(), f.state, f.State())
	if err != nil || !slices.Equal(removed, []string{build}) || len(left) != 0 || sentinelExists(build) {
		t.Fatal("the sweep did not remove the unused runtime", removed, left, err)
	}
	if err := f.apply(); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(f.home, "other runtime")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.public()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, f.public()); err != nil {
		t.Fatal(err)
	}
	changed := f.inspect(false)
	if changed.Status != engine.Changed || !slices.Equal(changed.Preserved, []string{f.public()}) {
		t.Fatal("a public link that the owner changed is not preserved", changed)
	}
	if err := f.provider.Remove(context.Background(), nvimResource, changed, f); !errors.Is(err, targets.ErrChanged) {
		t.Fatal(err)
	}
}

func TestNvimSyncEnvironmentControls(t *testing.T) {
	f := newNvimFixture(t)
	f.provider.Environment = []string{"RUSTUP_HOME=relative"}
	if _, err := f.provider.Inspect(context.Background(), nvimResource, engine.Want{Present: true}, f); err == nil || !strings.Contains(err.Error(), "explicit absolute managed Rust home") {
		t.Fatal("a relative Rust home was accepted", err)
	}
	f.provider.Environment = []string{"LD_PRELOAD=/tmp/x"}
	if _, err := f.provider.Inspect(context.Background(), nvimResource, engine.Want{Present: true}, f); err == nil || !strings.Contains(err.Error(), "unexpected Neovim sync environment control") {
		t.Fatal("an unexpected variable was accepted", err)
	}
	rust := filepath.Join(f.home, "rust home")
	f.provider.Environment = []string{"RUSTUP_HOME=" + rust}
	ready := false
	f.provider.CompilerEnvironment = func(context.Context) (map[string]string, error) {
		if !ready {
			return nil, errors.New("compiler is not installed")
		}
		return map[string]string{"PATH": "compiler", "INCLUDE": "sdk"}, nil
	}
	if err := f.apply(); err == nil || !strings.Contains(err.Error(), "compiler is not installed") || len(f.phases) != 0 {
		t.Fatal("a phase ran before the compiler prerequisite", err)
	}
	ready = true
	if err := f.apply(); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.phases {
		managed := ""
		for _, entry := range c.Env {
			if rest, ok := strings.CutPrefix(entry, "PATH="); ok && managed == "" {
				managed = rest
			}
		}
		if nvimEnvironment(c, "PATH") != "compiler"+string(os.PathListSeparator)+managed || nvimEnvironment(c, "INCLUDE") != "sdk" || nvimEnvironment(c, "RUSTUP_HOME") != rust {
			t.Fatal("the compiler environment lost the managed command folders or the Rust home", c.Env)
		}
	}
	if nvimEnvironment(f.checks[0], "INCLUDE") != "" {
		t.Fatal("the verification got the compiler environment")
	}
}

func TestNvimRuntimeDirectoryUsesTheDataLayoutOfTheTarget(t *testing.T) {
	root := t.TempDir()
	folders := platform.Folders{Home: root, LocalAppData: filepath.Join(root, "local"), Data: filepath.Join(root, "xdg")}
	for _, target := range []platform.Context{{OS: "darwin", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}, {OS: "windows", Arch: "amd64"}} {
		name := "nvim"
		if target.OS == "windows" {
			name += "-data"
		}
		path, err := hooks.NvimRuntimeDirectory(target, folders)
		if err != nil || path != filepath.Join(folders.Data, name, "dotfiles-runtime") {
			t.Fatal("wrong target data layout", target, path, err)
		}
	}
	folders.Data = ""
	if path, err := hooks.NvimRuntimeDirectory(platform.Context{OS: "windows", Arch: "amd64"}, folders); err != nil || path != filepath.Join(folders.LocalAppData, "nvim-data", "dotfiles-runtime") {
		t.Fatal(path, err)
	}
	folders.Data = "relative"
	if _, err := hooks.NvimRuntimeDirectory(platform.Context{OS: "linux", Arch: "amd64"}, folders); err == nil {
		t.Fatal("a relative data folder was accepted")
	}
}
