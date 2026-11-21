package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/system"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

type deferredCasks struct {
	specs     map[string]system.CaskSpec
	installed map[string]string
	root      string
	homebrew  *missingHomebrew
	issue     string
}

func (c *deferredCasks) Application(_ context.Context, r catalog.Resource, _ engine.Records) (string, error) {
	if path := c.installed[r.ID]; path != "" {
		return path, nil
	}
	return "", errors.New(c.specs[r.ID].Name + " has no verified application bundle")
}

func (c *deferredCasks) Cask(_ context.Context, r catalog.Resource) (system.CaskSpec, error) {
	return c.specs[r.ID], nil
}

func (c *deferredCasks) Inspect(_ context.Context, r catalog.Resource, want engine.Want, _ engine.Records) (engine.Finding, error) {
	switch {
	case c.homebrew != nil && !c.homebrew.installed:
		return engine.Finding{Status: engine.Unknown, Reason: "install Homebrew, then run setup again to review its package inventory", Token: "unavailable"}, nil
	case c.issue != "":
		return engine.Finding{Status: engine.Unknown, Reason: c.issue, Token: "unreadable"}, nil
	case c.installed[r.ID] != "":
		return engine.Finding{Status: engine.Satisfied, Token: "installed"}, nil
	case want.Present:
		return engine.Finding{Status: engine.Missing, Reason: "install the official native app through Homebrew", Token: "missing",
			Commands: []engine.CommandBlock{{Title: "Install", Lines: []string{"brew install --cask " + c.specs[r.ID].Token}, By: "setup"}}}, nil
	}
	return engine.Finding{Status: engine.Absent, Token: "missing"}, nil
}

func (c *deferredCasks) Apply(_ context.Context, r catalog.Resource, _ engine.Finding, rec engine.Records) error {
	application := filepath.Join(c.root, c.specs[r.ID].Application)
	if err := os.MkdirAll(application, 0700); err != nil {
		return err
	}
	c.installed[r.ID] = application
	return rec.Update(func(s *state.State) error { s.AddSystem("cask", c.specs[r.ID].Token); return nil })
}

func (c *deferredCasks) Remove(context.Context, catalog.Resource, engine.Finding, engine.Records) error {
	return nil
}

func deferredAction(plan engine.Plan, id string) string {
	for _, op := range plan.Operations {
		if op.Resource == id {
			return op.Action + ":" + op.Finding.Status.String()
		}
	}
	return ""
}

type missingHomebrew struct{ installed bool }

func (h *missingHomebrew) Discover(context.Context) (*system.HomebrewLocation, error) {
	if !h.installed {
		return nil, nil
	}
	return &system.HomebrewLocation{Program: "/opt/homebrew/bin/brew", Prefix: "/opt/homebrew", Cellar: "/opt/homebrew/Cellar"}, nil
}

func (h *missingHomebrew) Inspect(_ context.Context, _ catalog.Resource, want engine.Want, _ engine.Records) (engine.Finding, error) {
	switch {
	case h.installed:
		return engine.Finding{Status: engine.Satisfied, Token: "installed"}, nil
	case want.Present:
		return engine.Finding{Status: engine.Missing, Reason: "install Homebrew", Token: "missing", Privileged: true,
			Commands: []engine.CommandBlock{{Title: "Install Homebrew", Lines: []string{"/bin/bash install.sh"}, By: "setup"}}}, nil
	}
	return engine.Finding{Status: engine.Absent, Token: "missing"}, nil
}

func (h *missingHomebrew) Apply(context.Context, catalog.Resource, engine.Finding, engine.Records) error {
	h.installed = true
	return nil
}

func (h *missingHomebrew) Remove(context.Context, catalog.Resource, engine.Finding, engine.Records) error {
	return nil
}

type deferredMac struct {
	setup        setup
	wiring       *wiring
	engine       *engine.Engine
	casks        *deferredCasks
	homebrew     *missingHomebrew
	applications map[string][4]string
	legacy       map[string]string
}

func newDeferredMac(t *testing.T, formulae ...string) deferredMac {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses symbolic links")
	}
	s := wiredSetup(t, "darwin", "arm64")
	root, err := platform.ResolvePath(filepath.Dir(s.stateDir))
	if err != nil {
		t.Fatal(err)
	}
	s.stateDir, s.folders.Home = filepath.Join(root, "state"), filepath.Join(root, "home")
	s.target = "darwin/arm64:" + s.folders.Home
	w := newWiring(s)
	if w.err != nil {
		t.Fatal(w.err)
	}
	m := deferredMac{setup: s, wiring: w, homebrew: &missingHomebrew{}, legacy: map[string]string{}, applications: map[string][4]string{
		"tool.vscode":     {"desktop.vscode", "Visual Studio Code.app", "VS Code", "Code"},
		"tool.ghostty":    {"integration.ghostty", "Ghostty.app", "Ghostty", "ghostty"},
		"tool.openwhispr": {"desktop.openwhispr", "OpenWhispr.app", "OpenWhispr", "OpenWhispr"},
	}}
	m.casks = &deferredCasks{specs: system.MacCasks(s.folders.Home, "/opt/homebrew"), installed: map[string]string{}, root: filepath.Join(root, "Applications"), homebrew: m.homebrew}
	w.brew.Discover = m.homebrew.Discover
	homebrew, _ := s.catalog.Resource("infra.homebrew")
	resources := []catalog.Resource{homebrew, {ID: "apps", Name: "Applications", Capability: true, Requires: append([]string{"desktop.vscode", "integration.ghostty", "desktop.openwhispr"}, formulae...)}}
	for _, formula := range formulae {
		r, _ := s.catalog.Resource(formula)
		resources = append(resources, r)
	}
	for tool, application := range m.applications {
		if real, _ := s.catalog.Resource(tool); !slices.Contains(real.Dependencies(s.host.Context), "infra.homebrew") {
			t.Fatalf("the catalog no longer installs %s through Homebrew", tool)
		}
		resources = append(resources,
			catalog.Resource{ID: tool, Name: application[2], Action: "package", Bindings: map[string]catalog.Binding{"darwin": {Provider: "homebrew-cask", Package: m.casks.specs[tool].Token}},
				PlatformRequires: map[string][]string{"darwin": {"infra.homebrew"}}},
			catalog.Resource{ID: application[0], Name: application[2] + " launcher", Action: "desktop", Requires: []string{tool}})
	}
	fixture := &catalog.Catalog{Resources: resources}
	desktop := &hooks.Desktop{Context: s.host.Context, Folders: s.folders, StateDir: s.stateDir, Catalog: fixture, Casks: m.casks, CommandDirectory: filepath.Join(s.folders.Home, ".local", "bin")}
	route := func(r catalog.Resource) (engine.Provider, error) {
		wired, err := w.route(r)
		if err != nil {
			t.Fatal(err)
		}
		d, wrapped := wired.(deferredProvider)
		switch {
		case !wrapped && slices.Contains(formulae, r.ID):
			return m.casks, nil
		case !wrapped:
			t.Fatalf("%s must route through the deferred decision: %T", r.ID, wired)
		case r.ID == "infra.homebrew":
			d.Provider = m.homebrew
		case d.desktop:
			d.Provider = desktop
		default:
			d.Provider = m.casks
		}
		d.verified = func(_ context.Context, tool string, _ state.State) bool { return m.casks.installed[tool] != "" }
		return d, nil
	}
	m.engine = &engine.Engine{Catalog: fixture, Router: route, Context: s.host.Context, StateDir: s.stateDir, Target: s.target, Source: "fixture", Selection: w.selected.set}

	store, err := state.Open(s.stateDir, s.target)
	if err != nil {
		t.Fatal(err)
	}
	rec := storeRecordsFixture{store}
	for tool, application := range m.applications {
		payload := filepath.Join(s.stateDir, "packages", tool, "versions", "aaaa", "payload")
		writeExecutable(t, filepath.Join(payload, application[1], "Contents", "MacOS", application[3]))
		if err := os.Symlink(payload, filepath.Join(s.stateDir, "packages", tool, "current")); err != nil {
			t.Fatal(err)
		}
		m.legacy[tool] = filepath.Join(s.folders.Home, "Applications", application[1])
		unit := targets.Desired{Resource: application[0], Kind: "link", Path: m.legacy[tool], Link: filepath.Join(s.stateDir, "packages", tool, "current", application[1])}
		if err := (&targets.Writer{StateDir: s.stateDir}).Converge(context.Background(), unit, rec, targets.Preserve); err != nil {
			t.Fatal(err)
		}
	}
	err = store.Update(func(st *state.State) error { st.Selected = []string{"apps"}; return nil })
	if err = errors.Join(err, store.Close()); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDeferredMacApplicationsPassCheckAndMoveToTheirCasksWithUpdate(t *testing.T) {
	m := newDeferredMac(t)
	s, e, casks, applications := m.setup, m.engine, m.casks, m.applications
	if provider, err := m.wiring.route(catalog.Resource{ID: "app.zen", Action: "package", Bindings: map[string]catalog.Binding{"darwin": {Provider: "homebrew-cask", Package: "zen"}}}); err != nil || provider != engine.Provider(m.wiring.brew) {
		t.Fatalf("each other cask keeps its provider: %T %v", provider, err)
	}

	check, err := e.Check(context.Background())
	if err != nil || check.Status != "ready" {
		t.Fatalf("a deferred application is not a failure of check: %+v %v", check, err)
	}
	for _, application := range applications {
		for _, name := range []string{application[2], application[2] + " launcher"} {
			if note := "Note: " + name + " runs from the release v2 payload; Update tools moves it to the Homebrew cask. Choose tools keeps the v2 payload. The first confirmed run that changes something closes the v2 fallback."; !strings.Contains(check.Message, note) {
				t.Fatalf("check lacks %q:\n%s", note, check.Message)
			}
		}
	}
	if note := "Note: Homebrew package infrastructure " + deferredHomebrewReason; !strings.Contains(check.Message, note) {
		t.Fatalf("check lacks %q:\n%s", note, check.Message)
	}
	plan, err := e.Preview(context.Background(), engine.Request{Mode: "apply"})
	if err != nil {
		t.Fatal(err)
	}
	if got := deferredAction(plan, "infra.homebrew"); got != "keep:outdated" {
		t.Fatalf("apply must not install Homebrew only for the v2 payloads: %s", got)
	}
	for tool, application := range applications {
		if got := deferredAction(plan, tool); got != "keep:outdated" {
			t.Fatalf("apply must keep %s on release v2: %s", tool, got)
		}
		if got := deferredAction(plan, application[0]); got != "keep:satisfied" {
			t.Fatalf("apply must keep %s: %s", application[0], got)
		}
	}
	request := engine.Request{Mode: "update"}
	if plan, err = e.Preview(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := deferredAction(plan, "infra.homebrew"); got != "install:missing" {
		t.Fatalf("update must plan the Homebrew install: %s", got)
	}
	for tool, application := range applications {
		if got := deferredAction(plan, tool); got != "pending:unknown" {
			t.Fatalf("update must wait for Homebrew to inspect the cask of %s: %s", tool, got)
		}
		if got := deferredAction(plan, application[0]); !strings.HasPrefix(got, "pending:") {
			t.Fatalf("update must wait for the cask to move %s: %s", application[0], got)
		}
	}
	result, err := e.Apply(context.Background(), request, plan)
	if !errors.Is(err, engine.ErrReplan) || !m.homebrew.installed {
		t.Fatalf("the Homebrew bootstrap must end with a review of the casks: %+v %v", result, err)
	}
	if plan, err = e.Preview(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	for tool, application := range applications {
		if got := deferredAction(plan, tool); got != "install:missing" {
			t.Fatalf("update must plan the cask install of %s: %s", tool, got)
		}
		if got := deferredAction(plan, application[0]); !strings.HasPrefix(got, "repair:") {
			t.Fatalf("update must plan the move of %s: %s", application[0], got)
		}
	}
	for attempt := 0; ; attempt++ {
		result, err := e.Apply(context.Background(), request, plan)
		if (errors.Is(err, engine.ErrPlanChanged) || errors.Is(err, engine.ErrReplan)) && attempt < 3 {
			if plan, err = e.Preview(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err != nil || result.Status != "ready" {
			t.Fatalf("update: %+v %v", result, err)
		}
		break
	}
	if !m.homebrew.installed {
		t.Fatal("update must install Homebrew")
	}
	for command, tool := range map[string]string{"code": "tool.vscode", "ghostty": "tool.ghostty"} {
		target, err := os.Readlink(filepath.Join(s.folders.Home, ".local", "bin", command))
		if want := casks.specs[tool].CommandTarget(casks.installed[tool]); err != nil || target != want {
			t.Fatalf("%s must point into the cask application: %q, want %q (%v)", command, target, want, err)
		}
	}
	st, err := state.Peek(s.stateDir, s.target)
	if err != nil {
		t.Fatal(err)
	}
	for tool, path := range m.legacy {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the desktop integration must release the legacy link of %s: %v", tool, err)
		}
		if _, recorded := st.Targets[path]; recorded {
			t.Fatalf("the legacy link of %s stays recorded", tool)
		}
	}
	check, err = e.Check(context.Background())
	if err != nil || check.Status != "ready" || strings.Contains(check.Message, "release v2 payload") {
		t.Fatalf("after the move check needs no note: %+v %v", check, err)
	}
}

func TestHomebrewIsMissingWhenAnotherSelectedToolNeedsIt(t *testing.T) {
	m := newDeferredMac(t, "tool.tmux")
	check, err := m.engine.Check(context.Background())
	if err != nil || check.Status != "needs-action" || !strings.Contains(check.Message, "Homebrew package infrastructure (infra.homebrew): required by selection") {
		t.Fatalf("a selected formula needs Homebrew now: %+v %v", check, err)
	}
	plan, err := m.engine.Preview(context.Background(), engine.Request{Mode: "apply"})
	if err != nil {
		t.Fatal(err)
	}
	if got := deferredAction(plan, "infra.homebrew"); got != "install:missing" {
		t.Fatalf("apply must install Homebrew for the formula: %s", got)
	}
}

func TestAnInspectionErrorOfACaskIsNotDeferred(t *testing.T) {
	m := newDeferredMac(t)
	m.homebrew.installed, m.casks.issue = true, "brew info --cask failed: exit status 1"
	check, err := m.engine.Check(context.Background())
	if err != nil || check.Status != "needs-action" || !strings.Contains(check.Message, "Ghostty (tool.ghostty): setup could not inspect this resource: "+m.casks.issue) {
		t.Fatalf("an inspection error must stay in check: %+v %v", check, err)
	}
	plan, err := m.engine.Preview(context.Background(), engine.Request{Mode: "apply"})
	if err != nil {
		t.Fatal(err)
	}
	for tool := range m.applications {
		if got := deferredAction(plan, tool); got != "pending:unknown" {
			t.Fatalf("an inspection error of %s must stay unknown: %s", tool, got)
		}
	}
}

func TestABrokenLegacyPayloadIsNotDeferred(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses symbolic links")
	}
	s := wiredSetup(t, "darwin", "arm64")
	casks := &deferredCasks{specs: system.MacCasks(s.folders.Home, "/opt/homebrew"), installed: map[string]string{}}
	link := filepath.Join(s.folders.Home, "Applications", "Ghostty.app")
	if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(s.stateDir, "packages", "tool.ghostty", "current", "Ghostty.app"), link); err != nil {
		t.Fatal(err)
	}
	st := state.State{Targets: map[string]state.Record{link: {Resource: "integration.ghostty", Kind: "link", Applied: "link:abc", Before: "absent"}}}
	d := deferredProvider{Provider: casks, tool: "tool.ghostty", stateDir: s.stateDir, context: s.host.Context,
		verified: func(context.Context, string, state.State) bool { return false }}
	finding, err := d.Inspect(context.Background(), catalog.Resource{ID: "tool.ghostty", Name: "Ghostty"}, engine.Want{Present: true}, readRecords{st})
	if err != nil || finding.Status != engine.Missing {
		t.Fatalf("without its payload the application is missing: %+v %v", finding, err)
	}
	executable := filepath.Join(s.stateDir, "packages", "tool.ghostty", "current", "Ghostty.app", "Contents", "MacOS", "ghostty")
	if err := os.MkdirAll(filepath.Dir(executable), 0700); err != nil {
		t.Fatal(err)
	}
	finding, err = d.Inspect(context.Background(), catalog.Resource{ID: "tool.ghostty", Name: "Ghostty"}, engine.Want{Present: true}, readRecords{st})
	if err != nil || finding.Status != engine.Missing {
		t.Fatalf("without its bundle executable the application is missing: %+v %v", finding, err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0600); err != nil {
		t.Fatal(err)
	}
	finding, err = d.Inspect(context.Background(), catalog.Resource{ID: "tool.ghostty", Name: "Ghostty"}, engine.Want{Present: true}, readRecords{st})
	if err != nil || finding.Status != engine.Missing {
		t.Fatalf("with a bundle executable that cannot run the application is missing: %+v %v", finding, err)
	}
	if err := os.Chmod(executable, 0700); err != nil {
		t.Fatal(err)
	}
	finding, err = d.Inspect(context.Background(), catalog.Resource{ID: "tool.ghostty", Name: "Ghostty"}, engine.Want{Present: true}, readRecords{st})
	if err != nil || finding.Status != engine.Outdated || finding.Reason != deferredReason || len(finding.Commands) != 0 {
		t.Fatalf("with its payload the application is deferred: %+v %v", finding, err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(s.stateDir, "packages", "tool.ghostty", "versions", "gone", "payload", "Ghostty.app"), link); err != nil {
		t.Fatal(err)
	}
	finding, err = d.Inspect(context.Background(), catalog.Resource{ID: "tool.ghostty", Name: "Ghostty"}, engine.Want{Present: true}, readRecords{st})
	if err != nil || finding.Status != engine.Missing {
		t.Fatalf("with a launcher that points at nothing the application is missing: %+v %v", finding, err)
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

type storeRecordsFixture struct{ store *state.Store }

func (r storeRecordsFixture) State() state.State                         { return r.store.View() }
func (r storeRecordsFixture) Update(edit func(*state.State) error) error { return r.store.Update(edit) }
