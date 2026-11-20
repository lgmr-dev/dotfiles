package main

import (
	"context"
	"slices"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/v2import"
)

const deferredReason = "runs from the release v2 payload; Update tools moves it to the Homebrew cask"

var deferredDesktop = map[string]string{"desktop.vscode": "tool.vscode", "integration.ghostty": "tool.ghostty", "desktop.openwhispr": "tool.openwhispr"}

type deferredProvider struct {
	engine.Provider
	tool     string
	desktop  bool
	stateDir string
	context  platform.Context
	verified func(ctx context.Context, tool string, st state.State) bool
}

func (d deferredProvider) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	f, err := d.Provider.Inspect(ctx, r, want, rec)
	if err != nil || !want.Present || want.Update {
		return f, err
	}
	statuses := []engine.Status{engine.Missing}
	if d.desktop {
		statuses = append(statuses, engine.Unhealthy)
	}
	st := rec.State()
	if !slices.Contains(statuses, f.Status) || !slices.Contains(v2import.LegacyApplications(d.stateDir, st, d.context), d.tool) || d.verified(ctx, d.tool, st) {
		return f, nil
	}
	f.Status, f.Reason, f.Commands, f.Privileged, f.Replan = engine.Outdated, deferredReason, nil, false, false
	if d.desktop {
		f.Status, f.Reason = engine.Satisfied, ""
	}
	f.Notes = append(f.Notes, r.Name+" "+deferredReason+". Choose tools keeps the v2 payload. The first confirmed run that changes something closes the v2 fallback.")
	return f, nil
}

func (w *wiring) deferred(r catalog.Resource, provider engine.Provider) engine.Provider {
	if w.setup.host.OS != "darwin" {
		return provider
	}
	d := deferredProvider{Provider: provider, tool: r.ID, stateDir: w.setup.stateDir, context: w.setup.host.Context, verified: w.caskVerified}
	if tool, desktop := deferredDesktop[r.ID]; desktop {
		d.tool, d.desktop = tool, true
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
