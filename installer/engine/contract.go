// Package engine plans and runs setup: it computes the closure of the
// selected, kept and retained resources, derives one action for each resource
// from an inspection, shows the plan and runs the approved steps in order.
// It holds the contracts between the run loop, the providers and the user
// interface.
package engine

import (
	"context"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

// Status is the result of an inspection of one resource.
type Status int

// Satisfied: present, healthy and current. Missing: wanted and not present.
// Outdated: present and healthy, and the checkout or the vendor has a newer
// reviewed version; a provider reports it in each mode when it knows.
// Unhealthy: present, and its check failed. Changed: content differs from its
// record, and setup keeps it. Unknown: the inspection could not decide; it
// never proves absence. Blocked: the operator or another resource must act
// first. Absent: not wanted and not present.
const (
	Satisfied Status = iota
	Missing
	Outdated
	Unhealthy
	Changed
	Unknown
	Blocked
	Absent
)

func (s Status) String() string {
	names := []string{"satisfied", "missing", "outdated", "unhealthy", "changed", "unknown", "blocked", "absent"}
	if s < 0 || int(s) >= len(names) {
		return "invalid"
	}
	return names[s]
}

// Want is what the run asks of a resource. Present is false for a deselected
// resource that the run can release. Update asks for the reviewed pins of the
// checkout, and for the latest release where a provider follows a channel.
type Want struct {
	Present bool
	Update  bool
}

// CommandBlock is a copy-ready set of commands. By is "setup" when setup runs
// the lines after approval and "you" when the operator runs them.
type CommandBlock struct {
	Title string
	Lines []string
	By    string
}

// Finding is the inspection of one resource. Reason is one line for the
// review screen. Preserved lists changed content that setup keeps. Privileged
// means that the step needs sudo or elevation. Pending means that Apply uses
// a pending mark. Replan means that success makes a provider available, so
// the pass stops after the step. Replace is set only by the engine, on the
// approved finding that Apply receives: the operator approved the replacement
// of changed content, and the provider saves that content before it replaces
// it. Token is a digest of what the provider
// observed of the resource itself, not of the health of its dependencies:
// Apply and Remove run only when a new Inspect gives the same Token.
type Finding struct {
	Status     Status
	Reason     string
	Version    string
	Notes      []string
	Preserved  []string
	Commands   []CommandBlock
	Privileged bool
	Pending    bool
	Replan     bool
	Replace    bool
	Token      string
}

// Records is the state as a provider uses it. Update fails during a preview
// and a check, which only read.
type Records interface {
	State() state.State
	Update(func(*state.State) error) error
}

// Provider converges one kind of resource. Inspect only reads. Apply
// installs, updates or repairs and then verifies. Remove releases the owned
// content; for a system package it removes nothing and deletes the system
// record only when the package is absent.
type Provider interface {
	Inspect(ctx context.Context, r catalog.Resource, want Want, rec Records) (Finding, error)
	Apply(ctx context.Context, r catalog.Resource, approved Finding, rec Records) error
	Remove(ctx context.Context, r catalog.Resource, approved Finding, rec Records) error
}

// Router returns the provider of a resource.
type Router func(catalog.Resource) (Provider, error)

// Catalog is what the engine needs from the loaded catalog. Closure returns
// requirements before their consumers. Menu returns the selectable entries of
// a system in their display order.
type Catalog interface {
	Resource(id string) (catalog.Resource, bool)
	Closure(roots []string, ctx platform.Context) ([]string, error)
	Menu(ctx platform.Context) []catalog.Resource
}

// SystemRecord returns the state.System provider and item name of the system
// package or vendor application of a resource. A package binding gives them:
// apt, homebrew-formula (brew), homebrew-cask (cask) and windows-app
// (windows), with the package name. A system provider of a resource without
// such a binding records the resource ID under its own provider key, which
// SystemRecorded finds.
func SystemRecord(r catalog.Resource, ctx platform.Context) (provider, name string, ok bool) {
	binding, bound := r.Bindings[ctx.OS]
	if !bound {
		return "", "", false
	}
	key := map[string]string{"apt": "apt", "homebrew-formula": "brew", "homebrew-cask": "cask", "windows-app": "windows"}[binding.Provider]
	return key, binding.Package, key != ""
}

// SystemRecorded reports that the state has a system record of the resource.
func SystemRecorded(st state.State, r catalog.Resource, ctx platform.Context) bool {
	if provider, name, ok := SystemRecord(r, ctx); ok {
		return st.HasSystem(provider, name)
	}
	for provider := range st.System {
		if st.HasSystem(provider, r.ID) {
			return true
		}
	}
	return false
}

// Recorded reports that the state has a target record or a system record of
// the resource.
func Recorded(st state.State, r catalog.Resource, ctx platform.Context) bool {
	for _, record := range st.Targets {
		if record.Resource == r.ID {
			return true
		}
	}
	return SystemRecorded(st, r, ctx)
}
