package engine_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/state"
)

var linux = platform.Context{OS: "linux", Arch: "amd64"}

const target = "linux/amd64:/home/fixture"

type fakeCatalog struct{ resources []catalog.Resource }

func (c *fakeCatalog) Resource(id string) (catalog.Resource, bool) {
	for _, r := range c.resources {
		if r.ID == id {
			return r, true
		}
	}
	return catalog.Resource{}, false
}

func (c *fakeCatalog) Menu(platform.Context) []catalog.Resource {
	menu := []catalog.Resource{}
	for _, r := range c.resources {
		if r.Capability {
			menu = append(menu, r)
		}
	}
	return menu
}

func (c *fakeCatalog) Closure(roots []string, _ platform.Context) ([]string, error) {
	result, marks := []string{}, map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		r, ok := c.Resource(id)
		if !ok {
			return fmt.Errorf("unknown resource %q", id)
		}
		if marks[id] == 1 {
			return fmt.Errorf("dependency cycle at %s", id)
		}
		if marks[id] == 2 {
			return nil
		}
		marks[id] = 1
		dependencies := slices.Clone(r.Requires)
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		marks[id] = 2
		result = append(result, id)
		return nil
	}
	ordered := slices.Clone(roots)
	sort.Strings(ordered)
	for _, root := range ordered {
		if err := visit(root); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func fixtureCatalog() *fakeCatalog {
	return &fakeCatalog{resources: []catalog.Resource{
		{ID: "editor", Name: "Editor", Description: "An editor", Capability: true, Category: "Tools", Requires: []string{"config.editor", "tool.editor"}},
		{ID: "shell", Name: "Shell", Capability: true, Category: "Tools", Requires: []string{"tool.zsh"}},
		{ID: "packages", Name: "Packages", Capability: true, Category: "Tools", Requires: []string{"tool.afterboot"}},
		{ID: "other", Name: "Other", Capability: true, Category: "Tools", Requires: []string{"tool.other"}},
		{ID: "tool.git", Name: "Git", Action: "git", Shared: true},
		{ID: "tool.editor", Name: "Editor program", Action: "archive", Requires: []string{"tool.git"}},
		{ID: "config.editor", Name: "Editor configuration", Action: "config", Requires: []string{"tool.editor"}},
		{ID: "tool.zsh", Name: "Zsh", Action: "zsh", Scope: "machine", Bindings: map[string]catalog.Binding{"linux": {Provider: "apt", Package: "zsh"}}},
		{ID: "infra.boot", Name: "Package manager", Action: "homebrew", Retain: true, ReplanAfter: true},
		{ID: "tool.afterboot", Name: "Packaged tool", Action: "package", Requires: []string{"infra.boot"}},
		{ID: "tool.other", Name: "Other tool", Action: "archive"},
	}}
}

type fakeResource struct {
	present, unhealthy, outdated, changed, unknown bool
	privileged                                     bool
	generation                                     int
	applyErr, removeErr, inspectErr                error
	afterApply                                     func()
}

type world struct {
	writableInspections int
	mutex               sync.Mutex
	home                string
	resources           map[string]*fakeResource
	log                 []string
	replaced            []string
	swept               int
	elevated            int
	released            int
}

func (w *world) resource(id string) *fakeResource {
	if w.resources[id] == nil {
		w.resources[id] = &fakeResource{}
	}
	return w.resources[id]
}

func (w *world) key(id string) string { return filepath.Join(w.home, id) }

func (w *world) calls(prefix string) []string {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	result := []string{}
	for _, entry := range w.log {
		if strings.HasPrefix(entry, prefix) {
			result = append(result, strings.TrimPrefix(entry, prefix))
		}
	}
	return result
}

func (w *world) Inspect(_ context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	f := w.resource(r.ID)
	w.log = append(w.log, "inspect:"+r.ID)
	if rec.Update(func(*state.State) error { return nil }) == nil {
		w.writableInspections++
	}
	if f.inspectErr != nil {
		return engine.Finding{}, f.inspectErr
	}
	finding := engine.Finding{Privileged: f.privileged, Token: fmt.Sprint(f.present, f.unhealthy, f.outdated, f.changed, f.generation)}
	switch {
	case f.unknown:
		finding.Status, finding.Reason = engine.Unknown, "the provider is busy"
	case !f.present && want.Present:
		finding.Status = engine.Missing
	case !f.present:
		finding.Status = engine.Absent
	case f.changed:
		finding.Status, finding.Reason, finding.Preserved = engine.Changed, "edited after setup wrote it", []string{w.key(r.ID)}
	case f.unhealthy:
		finding.Status, finding.Reason = engine.Unhealthy, "its command fails"
	case f.outdated:
		finding.Status, finding.Version, finding.Notes = engine.Outdated, "1.0", []string{r.Name + " 2.0 is available"}
	default:
		finding.Version = "2.0"
	}
	if r.ID == "tool.zsh" {
		finding.Commands = []engine.CommandBlock{{Title: "Remove Zsh", Lines: []string{"sudo apt-get remove zsh"}, By: "you"}}
	}
	return finding, nil
}

func (w *world) Apply(_ context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	f := w.resource(r.ID)
	w.log = append(w.log, "apply:"+r.ID)
	if f.applyErr != nil {
		return f.applyErr
	}
	if approved.Replace {
		w.replaced = append(w.replaced, r.ID)
		f.changed = false
	}
	f.present, f.unhealthy, f.outdated = true, false, false
	if f.afterApply != nil {
		f.afterApply()
	}
	return rec.Update(func(s *state.State) error {
		if r.ID == "tool.zsh" {
			s.AddSystem("apt", "zsh")
			return nil
		}
		if err := s.BeginTarget(w.key(r.ID), state.Record{Resource: r.ID, Kind: "file", Next: "fp:1", Before: "absent"}); err != nil {
			return err
		}
		return s.Promote(w.key(r.ID))
	})
}

func (w *world) Remove(_ context.Context, r catalog.Resource, _ engine.Finding, rec engine.Records) error {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	f := w.resource(r.ID)
	w.log = append(w.log, "remove:"+r.ID)
	if f.removeErr != nil {
		return f.removeErr
	}
	return rec.Update(func(s *state.State) error {
		if r.ID == "tool.zsh" {
			if !f.present {
				s.RemoveSystem("apt", "zsh")
			}
			return nil
		}
		f.present = false
		s.Drop(w.key(r.ID))
		return nil
	})
}

func fixture(t *testing.T) (*engine.Engine, *world) {
	t.Helper()
	root := t.TempDir()
	w := &world{home: filepath.Join(root, "home"), resources: map[string]*fakeResource{}}
	e := &engine.Engine{Catalog: fixtureCatalog(), Context: linux, StateDir: filepath.Join(root, "state"), Target: target, Source: "source-1",
		Router: func(r catalog.Resource) (engine.Provider, error) {
			if r.ID == "tool.unrouted" {
				return nil, errors.New("no provider")
			}
			return w, nil
		},
		Sweep: func(context.Context, engine.Records) error { w.swept++; return nil },
		Elevate: func(context.Context) (func() error, error) {
			w.elevated++
			return func() error { w.released++; return nil }, nil
		}}
	return e, w
}

func apply(t *testing.T, e *engine.Engine, request engine.Request) (engine.Result, error) {
	t.Helper()
	plan, err := e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return e.Apply(context.Background(), request, plan)
}

func mustApply(t *testing.T, e *engine.Engine, request engine.Request) engine.Result {
	t.Helper()
	result, err := apply(t, e, request)
	if err != nil || result.Status != "ready" {
		t.Fatalf("apply failed: %+v %v", result, err)
	}
	return result
}

func selection(ids ...string) engine.Request {
	return engine.Request{Mode: "apply", Selected: ids, SelectSet: true}
}

func actions(plan engine.Plan) []string {
	result := []string{}
	for _, op := range plan.Operations {
		result = append(result, op.Action+":"+op.Resource)
	}
	return result
}

func operation(t *testing.T, plan engine.Plan, id string) engine.Operation {
	t.Helper()
	for _, op := range plan.Operations {
		if op.Resource == id {
			return op
		}
	}
	t.Fatalf("plan has no operation for %s: %v", id, actions(plan))
	return engine.Operation{}
}

func saved(t *testing.T, e *engine.Engine) state.State {
	t.Helper()
	st, err := state.Peek(e.StateDir, e.Target)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
