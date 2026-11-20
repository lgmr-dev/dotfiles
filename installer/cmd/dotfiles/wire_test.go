package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/system"
	"github.com/luisgui1757/dotfiles/installer/tools"
)

func wiredSetup(t *testing.T, goos, arch string) setup {
	t.Helper()
	resources, err := catalog.Load(tools.FS)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	host := platform.Host{Context: platform.Context{OS: goos, Arch: arch}}
	if goos == "linux" {
		host.Distro, host.DistroRelease, host.Family, host.Libc = "ubuntu", "24.04", "debian", "glibc"
	}
	s := setup{checkout: filepath.Join(root, "checkout"), executable: filepath.Join(root, "dotfiles"), source: "source-fixture", stateDir: filepath.Join(root, "state"),
		target: goos + "/" + arch + ":" + home, host: host, catalog: resources, homebrewDirectory: func() string { return "" },
		folders: platform.Folders{Home: home, Config: filepath.Join(home, ".config"), Data: filepath.Join(home, ".local", "share"),
			LocalAppData: filepath.Join(home, "AppData", "Local"), AppData: filepath.Join(home, "AppData", "Roaming"), Programs: filepath.Join(home, "Programs")}}
	switch goos {
	case "linux":
		s.apt = &system.APTLocation{APTGet: "/usr/bin/apt-get", APTCache: "/usr/bin/apt-cache", Dpkg: "/usr/bin/dpkg", DpkgQuery: "/usr/bin/dpkg-query", APTMark: "/usr/bin/apt-mark", Sudo: "/usr/bin/sudo"}
		s.dpkgDeb = "/usr/bin/dpkg-deb"
	case "windows":
		s.powershell, s.buildTools = filepath.Join(root, "powershell.exe"), filepath.Join(root, "BuildTools")
	}
	return s
}

func systemResources(t *testing.T, s setup) []catalog.Resource {
	t.Helper()
	menu := []string{}
	for _, r := range s.catalog.Menu(s.host.Context) {
		menu = append(menu, r.ID)
	}
	closure, err := s.catalog.Closure(menu, s.host.Context)
	if err != nil {
		t.Fatal(err)
	}
	resources := []catalog.Resource{}
	for _, id := range closure {
		r, known := s.catalog.Resource(id)
		if !known {
			t.Fatalf("the closure names the unknown resource %s", id)
		}
		resources = append(resources, r)
	}
	return resources
}

func TestEachResourceOfEachSystemRoutesToItsProvider(t *testing.T) {
	everywhere := map[string]string{
		"integration.sentinel-installer": "*hooks.Sentinel", "tool.font": "*hooks.Fonts", "nvim.sync": "*hooks.NvimSync",
		"desktop.vscode": "*hooks.Desktop", "desktop.aerospace": "*hooks.Desktop", "integration.ghostty": "*hooks.Desktop", "desktop.rose-pine": "*hooks.Desktop",
		"desktop.windows-terminal": "*hooks.Desktop", "desktop.openwhispr": "*hooks.Desktop", "desktop.obsidian": "*hooks.Desktop", "desktop.t3-code": "*hooks.Desktop",
		"desktop.zen": "*hooks.Desktop", "desktop.xremap": "*hooks.Desktop",
		"integration.shells": "*hooks.Profiles", "tmux.plugins": "*hooks.Profiles", "integration.git": "*hooks.Profiles", "integration.vscode-managed": "*hooks.Profiles",
		"integration.kiro": "*hooks.Profiles", "integration.t3-code": "*hooks.Profiles", "integration.t3-code-theme": "*hooks.Profiles", "integration.windows-terminal": "*hooks.Profiles",
	}
	special := map[string]map[string]string{
		"darwin": {"infra.apple-clt": "*system.AppleCLT", "tool.compiler": "*system.MacPrerequisites", "tool.make": "*system.MacPrerequisites",
			"tool.clipboard": "*system.MacPrerequisites", "infra.homebrew": "*system.HomebrewBootstrap", "tool.zsh": "*system.MacZsh"},
		"linux":   {"system.keyboard-access": "*system.KeyboardAccess", "tool.clipboard": "main.noted"},
		"windows": {"tool.compiler": "*system.BuildTools", "tool.vcredist": "*system.WindowsVendor"},
	}
	bindings := map[string]string{"homebrew-formula": "*system.Brew", "homebrew-cask": "*system.Brew", "apt": "*system.APT", "windows-app": "*system.WindowsApps", "archive": "*builds.Provider"}
	probes := map[string]map[string]string{
		"darwin": {"tool.vscode": "*system.Brew", "tool.git": "*system.Brew", "app.zen": "*system.Brew", "tool.rg": "*builds.Provider", "config.nvim": "*targets.ConfigProvider", "tool.aerospace": "*builds.Provider"},
		"linux": {"tool.vscode": "*system.APT", "tool.tailscale": "*system.APT", "library.libc6": "*system.APT", "tool.compiler": "*system.APT", "tool.zsh": "*system.APT",
			"tool.ghostty": "*builds.Provider", "font.hack": "*builds.Provider", "config.xremap": "*targets.ConfigProvider"},
		"windows": {"tool.vscode": "*builds.Provider", "tool.clipboard": "*builds.Provider", "tool.make": "*builds.Provider", "tool.git": "*builds.Provider",
			"app.t3-code-windows": "*system.WindowsApps", "config.nvim": "*targets.ConfigProvider"},
	}
	for _, target := range []platform.Context{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "darwin", Arch: "arm64"}, {OS: "windows", Arch: "amd64"}} {
		t.Run(target.OS+"/"+target.Arch, func(t *testing.T) {
			s := wiredSetup(t, target.OS, target.Arch)
			route := newRouter(s)
			routed := map[string]string{}
			for _, r := range systemResources(t, s) {
				want := everywhere[r.ID]
				if other, ok := special[target.OS][r.ID]; ok {
					want = other
				}
				if want == "" {
					want = bindings[r.Bindings[target.OS].Provider]
				}
				if want == "" && r.Action == "config" {
					want = "*targets.ConfigProvider"
				}
				provider, err := route(r)
				if err != nil {
					t.Errorf("%s has no route: %v", r.ID, err)
					continue
				}
				if r.Action == "" {
					if provider != nil {
						t.Errorf("the group %s got the provider %T", r.ID, provider)
					}
					continue
				}
				if want == "" {
					t.Errorf("the table has no row for %s (action %s)", r.ID, r.Action)
					continue
				}
				if got := fmt.Sprintf("%T", provider); got != want {
					t.Errorf("%s routes to %s, want %s", r.ID, got, want)
				}
				routed[r.ID] = fmt.Sprintf("%T", provider)
			}
			for id, want := range probes[target.OS] {
				if routed[id] != want {
					t.Errorf("%s routes to %q, want %s", id, routed[id], want)
				}
			}
			if _, err := route(catalog.Resource{ID: "tool.unknown", Action: "unknown"}); err == nil || !strings.Contains(err.Error(), "tool.unknown") {
				t.Errorf("a resource without a provider must fail with its name: %v", err)
			}
		})
	}
}

func TestMissingAPTToolsMakeOnlyTheAPTResourcesUnknown(t *testing.T) {
	s := wiredSetup(t, "linux", "amd64")
	s.apt, s.aptIssue = nil, "APT requires apt-mark"
	route := newRouter(s)
	for _, id := range []string{"tool.git", "tool.clipboard", "library.libc6"} {
		r, _ := s.catalog.Resource(id)
		provider, err := route(r)
		if err != nil {
			t.Fatal(err)
		}
		finding, err := provider.Inspect(context.Background(), r, engine.Want{Present: true}, readRecords{})
		if err != nil || finding.Status != engine.Unknown || finding.Reason != "APT requires apt-mark" {
			t.Fatalf("%s: %+v %v", id, finding, err)
		}
		if err := provider.Apply(context.Background(), r, finding, readRecords{}); err == nil {
			t.Fatalf("%s was applied without APT", id)
		}
	}
	r, _ := s.catalog.Resource("tool.rg")
	if provider, err := route(r); err != nil || fmt.Sprintf("%T", provider) != "*builds.Provider" {
		t.Fatalf("an archive resource lost its provider: %T %v", provider, err)
	}
}

func TestSelectionOfTheRunReachesTheProfilesAndSentinel(t *testing.T) {
	s := wiredSetup(t, "linux", "amd64")
	w := newWiring(s)
	if w.err != nil {
		t.Fatal(w.err)
	}
	wanted := []string{"tool.codex", "codex"}
	w.selected.set(wanted)
	wanted[0] = "changed"
	for name, got := range map[string][]string{"profiles": w.profiles.Wanted(), "sentinel": w.sentinel.Wanted()} {
		if !slices.Equal(got, []string{"tool.codex", "codex"}) {
			t.Errorf("%s got the selection %v", name, got)
		}
	}
	e := newEngine(s)
	if e.Selection == nil || e.Sweep == nil || e.Leftovers == nil || e.Elevate != nil {
		t.Fatalf("the engine lacks a hook of the wiring, or elevates without sudo: %+v", e)
	}
	s.sudo = "/usr/bin/sudo"
	if newEngine(s).Elevate == nil {
		t.Fatal("the engine has no administrator session with sudo")
	}
}

func TestSweepKeepsUnknownContentAndCheckListsIt(t *testing.T) {
	s := wiredSetup(t, "linux", "amd64")
	if err := newSweep(s)(context.Background(), readRecords{}); err != nil {
		t.Fatal("a sweep without a tools folder failed", err)
	}
	stranger := filepath.Join(s.stateDir, "tools", "tool.rg", "notes.txt")
	preserved := filepath.Join(s.stateDir, "sentinel", "checkout")
	for _, path := range []string{stranger, filepath.Join(preserved, "install.sh")} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("personal"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := newSweep(s)(context.Background(), readRecords{state.State{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stranger); err != nil {
		t.Fatal("the sweep removed content that it does not know", err)
	}
	left, err := newWiring(s).leftovers(context.Background())
	if err != nil || !slices.Equal(left, []string{stranger, preserved}) {
		t.Fatalf("leftovers: %v %v", left, err)
	}
	if got := hooks.SentinelLeftovers(s.stateDir); !slices.Equal(got, []string{preserved}) {
		t.Fatal(got)
	}
	check, err := newEngine(s).Check(context.Background())
	if err != nil || check.Status != "ready" {
		t.Fatalf("%+v %v", check, err)
	}
	for _, path := range []string{stranger, preserved} {
		if !strings.Contains(check.Message, "Note: setup keeps "+path+" and no longer uses it") {
			t.Fatalf("check does not list %s:\n%s", path, check.Message)
		}
	}
}
