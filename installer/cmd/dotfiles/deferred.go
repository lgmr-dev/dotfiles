package main

import (
	"context"
	"slices"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/state"
	"github.com/lgmr-dev/dotfiles/installer/v2import"
)

const (
	deferredReason         = "runs from the release v2 payload; Update tools moves it to the Homebrew cask"
	deferredHomebrewReason = "is necessary only for the applications that run from the release v2 payload; Update tools installs it and moves them to their Homebrew casks"
	deferredNote           = ". Choose tools keeps the v2 payload. The first confirmed run that changes something closes the v2 fallback."
)

var deferredDesktop = map[string]string{"desktop.vscode": "tool.vscode", "integration.ghostty": "tool.ghostty", "desktop.openwhispr": "tool.openwhispr"}

type deferredProvider struct {
	engine.Provider
	tool     string
	desktop  bool
	homebrew bool
	catalog  *catalog.Catalog
	wanted   func() []string
	stateDir string
	context  platform.Context
	verified func(ctx context.Context, tool string, st state.State) bool
	absent   func(ctx context.Context) bool
}

func (d deferredProvider) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	f, err := d.Provider.Inspect(ctx, r, want, rec)
	if err != nil || !want.Present || want.Update {
		return f, err
	}
	st := rec.State()
	if d.homebrew {
		if f.Status != engine.Missing || !d.onlyLegacyNeedHomebrew(ctx, st) {
			return f, nil
		}
		f.Status, f.Reason, f.Commands, f.Privileged, f.Replan = engine.Outdated, deferredHomebrewReason, nil, false, false
		f.Notes = append(f.Notes, r.Name+" "+deferredHomebrewReason+deferredNote)
		return f, nil
	}
	statuses := []engine.Status{engine.Missing}
	if d.desktop {
		statuses = append(statuses, engine.Unhealthy)
	} else if f.Status == engine.Unknown && d.absent(ctx) {
		statuses = append(statuses, engine.Unknown)
	}
	if !slices.Contains(statuses, f.Status) || !d.legacy(ctx, d.tool, st) {
		return f, nil
	}
	f.Status, f.Reason, f.Commands, f.Privileged, f.Replan = engine.Outdated, deferredReason, nil, false, false
	if d.desktop {
		f.Status, f.Reason = engine.Satisfied, ""
	}
	f.Notes = append(f.Notes, r.Name+" "+deferredReason+deferredNote)
	return f, nil
}

func (d deferredProvider) legacy(ctx context.Context, tool string, st state.State) bool {
	return slices.Contains(v2import.LegacyApplications(d.stateDir, st, d.context), tool) && !d.verified(ctx, tool, st)
}

func (d deferredProvider) onlyLegacyNeedHomebrew(ctx context.Context, st state.State) bool {
	needed := false
	for _, id := range d.wanted() {
		r, _ := d.catalog.Resource(id)
		if !slices.Contains(r.Dependencies(d.context), "infra.homebrew") {
			continue
		}
		if !d.legacy(ctx, id, st) {
			return false
		}
		needed = true
	}
	return needed
}

func (w *wiring) deferred(r catalog.Resource, provider engine.Provider) engine.Provider {
	if w.setup.host.OS != "darwin" {
		return provider
	}
	d := deferredProvider{Provider: provider, tool: r.ID, stateDir: w.setup.stateDir, context: w.setup.host.Context, verified: w.caskVerified, absent: w.homebrewAbsent}
	if tool, desktop := deferredDesktop[r.ID]; desktop {
		d.tool, d.desktop = tool, true
	} else if r.ID == "infra.homebrew" {
		d.homebrew, d.catalog, d.wanted = true, w.setup.catalog, w.selected.get
	} else if !slices.Contains([]string{"tool.vscode", "tool.ghostty", "tool.openwhispr"}, r.ID) {
		return provider
	}
	return d
}

func (w *wiring) caskVerified(ctx context.Context, tool string, st state.State) bool {
	r, known := w.setup.catalog.Resource(tool)
	if !known || w.brew == nil {
		return false
	}
	_, err := w.brew.Application(ctx, r, readRecords{st})
	return err == nil
}

func (w *wiring) homebrewAbsent(ctx context.Context) bool {
	location, err := w.brew.Discover(ctx)
	return err == nil && location == nil
}
