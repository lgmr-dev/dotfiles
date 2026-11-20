package engine_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/state"
)

func edit(t *testing.T, e *engine.Engine, change func(*state.State)) {
	t.Helper()
	store, err := state.Open(e.StateDir, e.Target)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Update(func(s *state.State) error { change(s); return nil })
	if err = errors.Join(err, store.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckReportsTheInspectionOfARetainedResource(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor", "other"))
	edit(t, e, func(s *state.State) { s.Selected = []string{"editor"} })
	check, err := e.Check(context.Background())
	if err != nil || check.Status != "ready" || operation(t, check.Plan, "tool.other").Action != "retain" {
		t.Fatal(check, err)
	}
	w.resource("tool.other").inspectErr = errors.New("permission denied")
	check, err = e.Check(context.Background())
	if err != nil || check.Status != "needs-action" || !strings.Contains(check.Message, "Other tool (tool.other): setup could not inspect this resource: inspection failed: permission denied") {
		t.Fatal("check hid the failed inspection of a retained resource", check, err)
	}
	if operation(t, check.Plan, "tool.other").Action != "retain" {
		t.Fatal("the retained resource lost its action")
	}
	w.resource("tool.other").inspectErr = nil
	w.resource("tool.other").unknown = true
	if check, err = e.Check(context.Background()); err != nil || check.Status != "needs-action" || !strings.Contains(check.Message, "Other tool (tool.other): setup could not inspect this resource: the provider is busy") {
		t.Fatal("check hid the unknown status of a retained resource", check, err)
	}
	w.resource("tool.other").unknown = false
	w.resource("tool.other").changed = true
	check, err = e.Check(context.Background())
	if err != nil || check.Status != "ready" || !strings.Contains(check.Message, "Note: Other tool (tool.other) stays without a selected tool and has changed content: edited after setup wrote it") {
		t.Fatal("changed content of a retained resource must be a note", check, err)
	}
}

func TestPendingMarksStayWhenTheReviewIsCancelled(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("other"))
	edit(t, e, func(s *state.State) { s.Mark("tool.zsh:install"); s.Mark("tool.git:install") })
	ui := &scriptedUI{t: t, answers: []string{"apply", "confirm", "confirm"}, tools: choose("other", "shell")}
	ui.review = func(context.Context, engine.Proposal, engine.Revise) (bool, error) { return false, nil }
	e.UI = ui
	result, err := e.Run(context.Background())
	if err != nil || result.Status != "cancelled" {
		t.Fatal(result, err)
	}
	if len(ui.menus) != 3 || ui.menus[1].title != "An earlier step did not report its end: tool.git:install. Did its command exit?" || ui.menus[2].title != "An earlier step did not report its end: tool.zsh:install. Did its command exit?" {
		t.Fatal("setup must ask one time for each mark", ui.menus)
	}
	if st := saved(t, e); !slices.Equal(st.Pending, []string{"tool.git:install", "tool.zsh:install"}) || len(w.calls("apply:")) != 1 {
		t.Fatal("a cancelled review cleared the marks", st.Pending)
	}
	ui = &scriptedUI{t: t, answers: []string{"apply", "confirm", "back", "exit"}}
	e.UI = ui
	if result, err := e.Run(context.Background()); err != nil || result.Status != "cancelled" {
		t.Fatal(result, err)
	}
	if st := saved(t, e); len(st.Pending) != 2 {
		t.Fatal("one confirmation cleared a mark before the run", st.Pending)
	}
	ui = &scriptedUI{t: t, answers: []string{"apply", "confirm", "confirm"}, tools: choose("other", "shell")}
	e.UI = ui
	if result, err := e.Run(context.Background()); err != nil || result.Status != "ready" {
		t.Fatal(result, err)
	}
	if st := saved(t, e); len(st.Pending) != 0 || !st.HasSystem("apt", "zsh") {
		t.Fatal("the confirmed run did not clear the marks", st)
	}
}

func TestARunWithoutAChangeLeavesTheFallbackOpen(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	setFallback(t, e, "open")
	request := selection("editor")
	plan, err := e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if engine.FallbackNotice(plan) != "" {
		t.Fatal("a plan without a change must not say that it closes the fallback", engine.FallbackNotice(plan))
	}
	if result, err := e.Apply(context.Background(), request, plan); err != nil || result.Status != "ready" {
		t.Fatal(result, err)
	}
	if saved(t, e).V2Restore != "open" {
		t.Fatal("a run without a change closed the fallback")
	}
	request = selection("editor", "other")
	if plan, err = e.Preview(context.Background(), request); err != nil || !strings.Contains(engine.FallbackNotice(plan), "This closes the v2 fallback") {
		t.Fatal(engine.FallbackNotice(plan), err)
	}
	w.resource("tool.other").present = true
	if result, err := e.Apply(context.Background(), request, plan); err != nil || result.Status != "ready" || slices.Contains(w.calls("apply:"), "tool.other") {
		t.Fatal(result, err)
	}
	if saved(t, e).V2Restore != "open" {
		t.Fatal("a step that was already satisfied closed the fallback")
	}
	request = selection("editor", "other", "shell")
	if plan, err = e.Preview(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	w.resource("tool.zsh").applyErr = errors.New("download failed")
	if _, err := e.Apply(context.Background(), request, plan); err == nil || !slices.Contains(w.calls("apply:"), "tool.zsh") {
		t.Fatal("the fixture step did not run and fail", err)
	}
	if saved(t, e).V2Restore != "" {
		t.Fatal("the first step that ran did not close the fallback")
	}
}
