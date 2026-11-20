package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sync"

	"github.com/luisgui1757/dotfiles/installer/builds"
	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/fetch"
	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/system"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

type selection struct {
	mu     sync.Mutex
	wanted []string
}

func (s *selection) set(wanted []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wanted = slices.Clone(wanted)
}

func (s *selection) get() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.wanted)
}

type readRecords struct{ st state.State }

func (r readRecords) State() state.State { return r.st.Clone() }

func (readRecords) Update(func(*state.State) error) error {
	return errors.New("an inspection only reads; it cannot change the state")
}

type unavailable struct{ reason string }

func (u unavailable) Inspect(ctx context.Context, _ catalog.Resource, _ engine.Want, _ engine.Records) (engine.Finding, error) {
	return engine.Finding{Status: engine.Unknown, Reason: u.reason}, ctx.Err()
}

func (u unavailable) Apply(context.Context, catalog.Resource, engine.Finding, engine.Records) error {
	return errors.New(u.reason)
}

func (u unavailable) Remove(context.Context, catalog.Resource, engine.Finding, engine.Records) error {
	return errors.New(u.reason)
}

type noted struct {
	engine.Provider
	notes []string
}

func (n noted) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	f, err := n.Provider.Inspect(ctx, r, want, rec)
	if err == nil && want.Present && f.Status == engine.Satisfied {
		f.Notes = append(slices.Clone(f.Notes), n.notes...)
	}
	return f, err
}

func download(ctx context.Context, rawURL, sum string, size int64, destination string) (result error) {
	source, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if source.Scheme != "https" || source.User != nil {
		return fmt.Errorf("the reviewed download %s is not a plain https address", rawURL)
	}
	client := &http.Client{CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 || request.URL.Scheme != "https" || request.URL.User != nil {
			return errors.New("unsafe download redirect")
		}
		return nil
	}}
	file, err := os.OpenFile(destination, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		result = errors.Join(result, file.Close())
		if result != nil {
			result = errors.Join(result, os.Remove(destination))
		}
	}()
	name := path.Base(source.Path)
	digest := sha256.New()
	count, err := fetch.DefaultTransfer.Fetch(ctx, client, rawURL, file, digest, platform.Output(ctx), name)
	if err != nil {
		return err
	}
	if count != size || hex.EncodeToString(digest.Sum(nil)) != sum {
		return fmt.Errorf("the download of %s differs from its reviewed size or SHA-256; setup did not use it", name)
	}
	return file.Sync()
}

type wiring struct {
	err      error
	setup    setup
	selected *selection

	sentinel      *hooks.Sentinel
	fonts         *hooks.Fonts
	desktop       *hooks.Desktop
	nvim          *hooks.NvimSync
	profiles      *hooks.Profiles
	configuration *targets.ConfigProvider
	archives      *builds.Provider

	appleCLT         *system.AppleCLT
	macPrerequisites *system.MacPrerequisites
	homebrew         *system.HomebrewBootstrap
	brew             *system.Brew
	macZsh           *system.MacZsh

	apt       engine.Provider
	clipboard engine.Provider
	keyboard  *system.KeyboardAccess

	buildTools  *system.BuildTools
	vendor      *system.WindowsVendor
	windowsApps *system.WindowsApps
}

func homebrewBinding(provider string) bool {
	return provider == "homebrew-formula" || provider == "homebrew-cask"
}

func (w *wiring) sentinelGit(pins map[string]catalog.ArchivePin) string {
	s := w.setup
	switch s.host.OS {
	case "windows":
		return builds.CommandPath(s.stateDir, "tool.git", "git", pins["tool.git"])
	case "darwin":
		if s.homebrew != nil {
			return filepath.Join(s.homebrew.Prefix, "bin", "git")
		}
		return "/opt/homebrew/bin/git"
	}
	return "/usr/bin/git"
}

func (w *wiring) nvimCommands(pins map[string]catalog.ArchivePin) (tools, directories []string, err error) {
	s := w.setup
	closure, err := s.catalog.Closure([]string{hooks.NvimResource}, s.host.Context)
	if err != nil {
		return nil, nil, err
	}
	formula := false
	for _, id := range closure {
		if _, pinned := pins[id]; pinned {
			tools = append(tools, id)
		}
		r, _ := s.catalog.Resource(id)
		formula = formula || r.Bindings[s.host.OS].Provider == "homebrew-formula"
	}
	if s.host.OS == "darwin" && formula {
		directory := "/opt/homebrew/bin"
		if s.homebrew != nil {
			directory = s.homebrew.CommandDirectory()
		}
		directories = append(directories, directory)
	}
	directories = append(directories, s.host.SystemCommandDirectories...)
	if s.host.OS != "windows" {
		directories = append(directories, "/usr/bin", "/bin", "/usr/sbin", "/sbin")
	}
	if s.powershell != "" {
		directories = append(directories, filepath.Dir(s.powershell))
	}
	return tools, directories, nil
}

func newWiring(s setup) *wiring {
	w := &wiring{setup: s, selected: &selection{}}
	if s.catalog == nil {
		w.err = errors.New("the provider wiring requires the catalog")
		return w
	}
	pins, err := builds.HostPins(s.catalog.Pins, s.host)
	if err != nil {
		w.err = err
		return w
	}
	runner := platform.Exec{}
	w.archives = &builds.Provider{StateDir: s.stateDir, Pins: pins, Runner: runner, DpkgDeb: s.dpkgDeb, Launcher: s.executable, LauncherSHA256: s.source}
	w.fonts = &hooks.Fonts{Context: s.host.Context, Folders: s.folders, StateDir: s.stateDir, Pins: pins, PowerShell: s.powershell,
		FontCache: "/usr/bin/fc-cache", FontMatch: "/usr/bin/fc-match", Runner: runner}
	kinds := map[string]targets.Kind{}
	maps.Copy(kinds, hooks.ProfileKinds())
	maps.Copy(kinds, hooks.SentinelKinds())
	maps.Copy(kinds, w.fonts.Kinds())
	writer := &targets.Writer{StateDir: s.stateDir, Kinds: kinds}
	w.configuration = &targets.ConfigProvider{Catalog: s.catalog, Context: s.host.Context, Folders: s.folders, Checkout: s.checkout, Writer: writer}
	w.profiles = &hooks.Profiles{Catalog: s.catalog, Context: s.host.Context, Folders: s.folders, Checkout: s.checkout, StateDir: s.stateDir, Writer: writer,
		Pins: pins, KiroHome: s.sentinel.KiroHome, Homebrew: s.homebrewDirectory, Wanted: w.selected.get, LegacyCommandDirectories: legacyCommandDirectories(s)}
	shell := "/bin/bash"
	if s.host.OS == "windows" {
		shell = s.powershell
	}
	w.sentinel = hooks.NewSentinel(s.host, s.folders.Home, s.stateDir, w.sentinelGit(pins), shell)
	w.sentinel.Locations, w.sentinel.Wanted = s.sentinel, w.selected.get
	w.desktop = &hooks.Desktop{Context: s.host.Context, Folders: s.folders, StateDir: s.stateDir, Pins: pins, Catalog: s.catalog,
		CommandDirectory: filepath.Join(s.folders.Home, ".local", "bin"), PowerShell: s.powershell, Runner: runner}
	runtime, err := hooks.NvimRuntimeDirectory(s.host.Context, s.folders)
	if err != nil {
		w.err = err
		return w
	}
	tools, directories, err := w.nvimCommands(pins)
	if err != nil {
		w.err = err
		return w
	}
	w.nvim = &hooks.NvimSync{Context: s.host.Context, Checkout: s.checkout, StateDir: s.stateDir, RuntimeDirectory: runtime, Pins: pins,
		Tools: tools, Directories: directories, Runner: runner}
	switch s.host.OS {
	case "darwin":
		w.appleCLT, w.macPrerequisites, w.macZsh = system.NewAppleCLT(), system.NewMacPrerequisites(), system.NewMacZsh()
		w.homebrew, w.brew = system.NewHomebrewBootstrap(s.folders.Home), system.NewBrew(s.folders.Home)
		w.desktop.Casks = w.brew
		w.profiles.VSCodeCask = func(st state.State) bool {
			r, known := s.catalog.Resource("tool.vscode")
			if !known {
				return false
			}
			_, err := w.brew.Application(context.Background(), r, readRecords{st})
			return err == nil
		}
	case "linux":
		w.keyboard = system.NewKeyboardAccess(s.apt, s.aptIssue, filepath.Join(s.stateDir, "keyboard-access"))
		w.keyboard.Authenticate = s.authenticate
		if s.apt == nil {
			w.apt = unavailable{s.aptIssue}
			w.clipboard = w.apt
			break
		}
		apt := system.NewAPT(*s.apt, s.catalog.Pins, s.host.Arch, filepath.Join(s.stateDir, "apt"))
		apt.Authenticate, apt.Download = s.authenticate, download
		w.apt = apt
		w.clipboard = noted{apt, s.clipboard.Disclosures()}
	case "windows":
		var compiler catalog.WindowsBuildToolsPin
		if s.catalog.Pins.BuildTools != nil {
			compiler = *s.catalog.Pins.BuildTools
		}
		var runtime catalog.WindowsVendorPin
		if s.catalog.Pins.VCRedist != nil {
			runtime = *s.catalog.Pins.VCRedist
		}
		w.buildTools = system.NewBuildTools(s.stateDir, s.powershell, s.buildTools, compiler, runner, download)
		w.vendor = system.NewWindowsVendor(s.stateDir, s.powershell, runtime, runner, download)
		w.windowsApps = system.NewWindowsApps(s.stateDir, s.powershell, s.folders.LocalAppData, s.catalog.Pins.WindowsApps, runner, download)
		w.buildTools.Issue, w.vendor.Issue, w.windowsApps.Issue = s.windowsIssue, s.windowsIssue, s.windowsIssue
		w.nvim.CompilerEnvironment = w.buildTools.CompilerEnvironment
	}
	return w
}

func (w *wiring) route(r catalog.Resource) (engine.Provider, error) {
	if w.err != nil {
		return nil, fmt.Errorf("setup has no provider for %s: %w", r.ID, w.err)
	}
	goos := w.setup.host.OS
	binding := r.Bindings[goos].Provider
	switch {
	case r.Action == "sentinel":
		return w.sentinel, nil
	case r.ID == hooks.FontResource:
		return w.fonts, nil
	case hooks.DesktopResource(r.ID):
		return w.desktop, nil
	case r.ID == "infra.apple-clt" && goos == "darwin":
		return w.appleCLT, nil
	case r.ID == hooks.NvimResource:
		return w.nvim, nil
	case r.ID == "tool.compiler" && goos == "windows":
		return w.buildTools, nil
	case goos == "darwin" && (r.ID == "tool.compiler" || r.ID == "tool.make" || r.ID == "tool.clipboard"):
		return w.macPrerequisites, nil
	case r.ID == "infra.homebrew" && goos == "darwin":
		return w.homebrew, nil
	case goos == "darwin" && homebrewBinding(binding):
		return w.brew, nil
	case r.ID == "system.keyboard-access" && goos == "linux":
		return w.keyboard, nil
	case goos == "linux" && binding == "apt" && r.ID == "tool.clipboard":
		return w.clipboard, nil
	case goos == "linux" && binding == "apt":
		return w.apt, nil
	case goos == "windows" && r.ID == "tool.vcredist":
		return w.vendor, nil
	case goos == "windows" && binding == "windows-app":
		return w.windowsApps, nil
	case goos == "darwin" && r.ID == "tool.zsh":
		return w.macZsh, nil
	case binding == "archive":
		return w.archives, nil
	case r.Action == "config":
		return w.configuration, nil
	case hooks.ProfileResource(r.ID):
		return w.profiles, nil
	case r.Action == "":
		return nil, nil
	}
	return nil, fmt.Errorf("%s has no provider on %s; this checkout cannot install, check or remove it", r.ID, goos)
}

func (w *wiring) sweep(ctx context.Context, rec engine.Records) error {
	removed, _, err := builds.Sweep(ctx, w.setup.stateDir, rec.State())
	for _, build := range removed {
		engine.ReportDetail(ctx, "Removed the unused private build %s", build)
	}
	return err
}

func (w *wiring) leftovers(ctx context.Context) ([]string, error) {
	st, err := state.Peek(w.setup.stateDir, w.setup.target)
	if err != nil {
		return nil, err
	}
	left, err := builds.Leftovers(ctx, w.setup.stateDir, st)
	return append(left, hooks.SentinelLeftovers(w.setup.stateDir)...), err
}

func newEngine(s setup) *engine.Engine {
	w := newWiring(s)
	e := &engine.Engine{Catalog: s.catalog, Router: w.route, Context: s.host.Context, StateDir: s.stateDir, Target: s.target, Source: s.source,
		Sweep: w.sweep, Selection: w.selected.set, Leftovers: w.leftovers}
	if s.sudo != "" {
		e.Elevate = sudoSession(s.sudo, s.authenticate)
	}
	return e
}
