package engine_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/state"
)

type menuCall struct {
	title   string
	labels  []string
	initial []string
}

type scriptedUI struct {
	t       *testing.T
	menus   []menuCall
	answers []string
	tools   func(engine.ToolMenu) ([]string, error)
	review  func(context.Context, engine.Proposal, engine.Revise) (bool, error)
	reviews []engine.Proposal
	reports []engine.Result
	begun   int
}

func (ui *scriptedUI) Choose(_ context.Context, title string, choices []engine.Choice, initial []string, multiple bool) ([]string, error) {
	labels := []string{}
	for _, choice := range choices {
		labels = append(labels, choice.Label)
	}
	ui.menus = append(ui.menus, menuCall{title, labels, initial})
	if multiple || len(ui.answers) == 0 {
		ui.t.Fatalf("unexpected menu %q", title)
	}
	answer := ui.answers[0]
	ui.answers = ui.answers[1:]
	if answer == "back" && !slices.ContainsFunc(choices, func(c engine.Choice) bool { return c.ID == "back" }) {
		return nil, engine.ErrBack
	}
	return []string{answer}, nil
}

func (ui *scriptedUI) ChooseTools(_ context.Context, menu engine.ToolMenu) ([]string, error) {
	if ui.tools == nil {
		ui.t.Fatal("unexpected tool menu")
	}
	return ui.tools(menu)
}

func (ui *scriptedUI) Review(ctx context.Context, c engine.Catalog, proposal engine.Proposal, revise engine.Revise) (bool, error) {
	if _, ok := c.Resource("tool.git"); !ok {
		ui.t.Fatal("the review received no catalog")
	}
	ui.reviews = append(ui.reviews, proposal)
	if ui.review == nil {
		return true, nil
	}
	return ui.review(ctx, proposal, revise)
}

func (ui *scriptedUI) BeginApply(ctx context.Context) (context.Context, error) {
	ui.begun++
	return ctx, nil
}

func (ui *scriptedUI) Report(result engine.Result) error {
	ui.reports = append(ui.reports, result)
	return nil
}

func choose(ids ...string) func(engine.ToolMenu) ([]string, error) {
	return func(engine.ToolMenu) ([]string, error) { return ids, nil }
}

func waitReady(t *testing.T, menu engine.ToolMenu) engine.ToolStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status := menu.Status()
		if status.Ready {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatal("tool status did not become ready", status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFirstRunOpensTheToolMenuAndAppliesTheReviewedPlan(t *testing.T) {
	e, w := fixture(t)
	ui := &scriptedUI{t: t}
	ui.tools = func(menu engine.ToolMenu) ([]string, error) {
		if menu.Title != "Choose the tools you want installed" || len(menu.Choices) != 4 || menu.Choices[0] != (engine.Choice{ID: "editor", Label: "Editor", Detail: "An editor", Category: "Tools"}) || len(menu.Installed) != 0 {
			t.Fatal(menu)
		}
		if status := menu.Status(); !status.Ready || status.Summary != "" {
			t.Fatal("a first run has a tool status", status)
		}
		return []string{"editor"}, nil
	}
	e.UI = ui
	result, err := e.Run(context.Background())
	if err != nil || result.Status != "ready" {
		t.Fatal(result, err)
	}
	if len(ui.menus) != 0 || ui.begun != 1 || len(ui.reviews) != 1 || len(ui.reports) != 1 || ui.reports[0].Status != "ready" {
		t.Fatal(ui.menus, ui.begun, len(ui.reviews), ui.reports)
	}
	if got := actions(ui.reviews[0].Plan); !slices.Equal(got, []string{"install:tool.git", "install:tool.editor", "install:config.editor"}) {
		t.Fatal(got)
	}
	if !slices.Equal(w.calls("apply:"), []string{"tool.git", "tool.editor", "config.editor"}) || !slices.Equal(saved(t, e).Selected, []string{"editor"}) {
		t.Fatal(w.calls("apply:"))
	}
}

func TestActionMenuKeepsItsChoicesAndCheckReturnsToIt(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	ui := &scriptedUI{t: t, answers: []string{"check", "exit"}}
	e.UI = ui
	result, err := e.Run(context.Background())
	if err != nil || result.Status != "cancelled" {
		t.Fatal(result, err)
	}
	want := menuCall{"What would you like to do?", []string{"Choose tools", "Update tools", "Check installation", "Exit"}, []string{"apply"}}
	if len(ui.menus) != 2 || ui.menus[0].title != want.title || !slices.Equal(ui.menus[0].labels, want.labels) || !slices.Equal(ui.menus[0].initial, want.initial) || !slices.Equal(ui.menus[1].initial, want.initial) {
		t.Fatal(ui.menus)
	}
	if len(ui.reports) != 1 || ui.reports[0].Status != "ready" || ui.reports[0].Plan.Mode != "check" || ui.begun != 0 {
		t.Fatal(ui.reports)
	}
	w.resource("tool.editor").unhealthy = true
	ui = &scriptedUI{t: t, answers: []string{"check", "exit"}}
	e.UI = ui
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ui.reports[0].Status != "needs-action" || !slices.Equal(ui.menus[1].initial, []string{"update"}) {
		t.Fatal("a failed check did not select Update", ui.reports, ui.menus)
	}
	w.resource("tool.editor").unhealthy = false
	e.Source = "source-2"
	ui = &scriptedUI{t: t, answers: []string{"update"}}
	e.UI = ui
	result, err = e.Run(context.Background())
	if err != nil || result.Status != "ready" || !slices.Equal(ui.menus[0].initial, []string{"update"}) || ui.reviews[0].Plan.Mode != "update" || len(ui.reviews[0].Choices) != 0 {
		t.Fatal("a changed checkout did not select Update", result, err, ui.menus)
	}
	if saved(t, e).Source != "source-2" {
		t.Fatal("update did not save the source")
	}
}

func TestToolMenuShowsTheStatusOfTheSavedTools(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor", "other", "shell"))
	w.resource("tool.editor").unhealthy = true
	w.resource("tool.other").outdated = true
	w.resource("tool.zsh").present = false
	ui := &scriptedUI{t: t, answers: []string{"apply"}}
	ui.tools = func(menu engine.ToolMenu) ([]string, error) {
		if !slices.Equal(menu.Initial, []string{"editor", "other", "shell"}) || !slices.Equal(menu.Installed, menu.Initial) {
			t.Fatal(menu.Initial, menu.Installed)
		}
		status := waitReady(t, menu)
		if status.Summary != "3 of 3 installed tools need attention." || status.Tools["editor"] != "needs repair" || status.Tools["other"] != "update available" || status.Tools["shell"] != "needs repair" {
			t.Fatal(status)
		}
		return nil, engine.ErrCancelled
	}
	e.UI = ui
	if result, err := e.Run(context.Background()); err != nil || result.Status != "cancelled" {
		t.Fatal(result, err)
	}
	w.resource("tool.editor").unhealthy, w.resource("tool.other").outdated, w.resource("tool.zsh").present = false, false, true
	ui = &scriptedUI{t: t, answers: []string{"apply"}}
	ui.tools = func(menu engine.ToolMenu) ([]string, error) {
		if status := waitReady(t, menu); status.Summary != "All 3 installed tools are healthy and current." || len(status.Tools) != 0 {
			t.Fatal(status)
		}
		return nil, engine.ErrCancelled
	}
	e.UI = ui
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReviewOffersSharedRemovalByName(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	ui := &scriptedUI{t: t, answers: []string{"apply"}, tools: choose()}
	ui.review = func(ctx context.Context, proposal engine.Proposal, revise engine.Revise) (bool, error) {
		if len(proposal.Choices) != 1 || proposal.Choices[0].Group != engine.ReviewShared || proposal.Choices[0].ID != "tool.git" || proposal.Choices[0].Label != "Git" || proposal.Choices[0].Checked ||
			!strings.HasPrefix(proposal.Choices[0].Detail, "Created by this installer; another application may use it. ") {
			t.Fatal(proposal.Choices)
		}
		if operation(t, proposal.Plan, "tool.git").Action != "retain" {
			t.Fatal(actions(proposal.Plan))
		}
		if _, err := revise(ctx, []engine.ReviewChoice{{Group: engine.ReviewShared, ID: "tool.other", Checked: true}}); err == nil {
			t.Fatal("accepted a choice that was not offered")
		}
		choices := slices.Clone(proposal.Choices)
		choices[0].Checked = true
		revised, err := revise(ctx, choices)
		if err != nil || !revised.Choices[0].Checked || operation(t, revised.Plan, "tool.git").Action != "remove" {
			t.Fatal(revised, err)
		}
		return true, nil
	}
	e.UI = ui
	result, err := e.Run(context.Background())
	if err != nil || result.Status != "ready" {
		t.Fatal(result, err)
	}
	if got := w.calls("remove:"); !slices.Equal(got, []string{"config.editor", "tool.editor", "tool.git"}) {
		t.Fatal(got)
	}
}

func TestReviewOffersToReleaseKeptResources(t *testing.T) {
	e, w := fixture(t)
	request := selection("editor")
	request.Keep = []string{"tool.other"}
	w.resource("tool.other").present = true
	mustApply(t, e, request)
	ui := &scriptedUI{t: t, answers: []string{"apply"}, tools: choose("editor")}
	ui.review = func(ctx context.Context, proposal engine.Proposal, revise engine.Revise) (bool, error) {
		if len(proposal.Choices) != 1 || proposal.Choices[0].Group != engine.ReviewKeep || proposal.Choices[0].ID != "tool.other" || !proposal.Choices[0].Checked || proposal.Choices[0].Detail != "Keep this resource even without a selected tool." {
			t.Fatal(proposal.Choices)
		}
		choices := slices.Clone(proposal.Choices)
		choices[0].Checked = false
		revised, err := revise(ctx, choices)
		if err != nil || len(revised.Plan.Keep) != 0 {
			t.Fatal(revised, err)
		}
		return true, nil
	}
	e.UI = ui
	if result, err := e.Run(context.Background()); err != nil || result.Status != "ready" {
		t.Fatal(result, err)
	}
	if len(saved(t, e).Keep) != 0 {
		t.Fatal("the released kept resource stayed in the state")
	}
}

func TestBackReturnsToTheMenuWithTheDraftSelection(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	ui := &scriptedUI{t: t, answers: []string{"apply", "apply", "back", "exit"}}
	opened := 0
	ui.tools = func(menu engine.ToolMenu) ([]string, error) {
		opened++
		if opened == 1 {
			return []string{"editor", "other"}, nil
		}
		if !slices.Equal(menu.Initial, []string{"editor", "other"}) || !slices.Equal(menu.Installed, []string{"editor"}) {
			t.Fatal("the draft selection was lost", menu.Initial, menu.Installed)
		}
		return nil, engine.ErrBack
	}
	ui.review = func(context.Context, engine.Proposal, engine.Revise) (bool, error) { return false, engine.ErrBack }
	e.UI = ui
	result, err := e.Run(context.Background())
	if err != nil || result.Status != "cancelled" || opened != 2 || len(ui.menus) != 4 {
		t.Fatal(result, err, opened, ui.menus)
	}
	if slices.Contains(w.calls("apply:"), "tool.other") || ui.begun != 0 {
		t.Fatal("going back changed the system")
	}
}

func TestDeclinedReviewChangesNothing(t *testing.T) {
	e, w := fixture(t)
	ui := &scriptedUI{t: t, tools: choose("editor")}
	ui.review = func(context.Context, engine.Proposal, engine.Revise) (bool, error) { return false, nil }
	e.UI = ui
	result, err := e.Run(context.Background())
	if err != nil || result.Status != "cancelled" || len(w.calls("apply:")) != 0 || saved(t, e).Schema != 0 || ui.begun != 0 || len(ui.reports) != 0 {
		t.Fatal(result, err)
	}
}

func TestChangedSystemBringsBackTheReview(t *testing.T) {
	e, w := fixture(t)
	ui := &scriptedUI{t: t, tools: choose("other")}
	ui.review = func(context.Context, engine.Proposal, engine.Revise) (bool, error) {
		if len(ui.reviews) == 1 {
			w.resource("tool.other").generation++
		}
		return true, nil
	}
	e.UI = ui
	result, err := e.Run(context.Background())
	if err != nil || result.Status != "ready" {
		t.Fatal(result, err)
	}
	if len(ui.reviews) != 2 || ui.reviews[0].Notice != "" || ui.reviews[1].Notice != "Your system changed after the review. This is the updated plan." || ui.begun != 2 || len(ui.reports) != 1 {
		t.Fatal(ui.reviews, ui.begun, ui.reports)
	}
	if got := w.calls("apply:"); !slices.Equal(got, []string{"tool.other"}) {
		t.Fatal(got)
	}
}

func TestNewProviderBringsBackTheReviewOfTheRemainingWork(t *testing.T) {
	e, w := fixture(t)
	ui := &scriptedUI{t: t, tools: choose("packages")}
	e.UI = ui
	result, err := e.Run(context.Background())
	if err != nil || result.Status != "ready" {
		t.Fatal(result, err)
	}
	if len(ui.reviews) != 2 || !strings.Contains(ui.reviews[1].Notice, "Package manager is ready") || !slices.Equal(actions(ui.reviews[1].Plan), []string{"keep:infra.boot", "install:tool.afterboot"}) {
		t.Fatal(ui.reviews)
	}
	if got := w.calls("apply:"); !slices.Equal(got, []string{"infra.boot", "tool.afterboot"}) {
		t.Fatal(got)
	}
}

func TestPendingMarkAsksBeforeAMutatingRun(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("other"))
	store, err := state.Open(e.StateDir, e.Target)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(store.Update(func(s *state.State) error { s.Mark("tool.zsh:install"); return nil }), store.Close()); err != nil {
		t.Fatal(err)
	}
	ui := &scriptedUI{t: t, answers: []string{"update", "back", "apply", "confirm"}, tools: choose("other", "shell")}
	e.UI = ui
	result, err := e.Run(context.Background())
	if err != nil || result.Status != "ready" {
		t.Fatal(result, err)
	}
	question := ui.menus[1]
	if question.title != "An earlier step did not report its end: tool.zsh:install. Did its command exit?" || !slices.Equal(question.labels, []string{"Yes, the command exited", "No, go back"}) || !slices.Equal(question.initial, []string{"back"}) {
		t.Fatal(question)
	}
	if len(ui.menus) != 4 || ui.menus[2].title != "What would you like to do?" || ui.menus[3].title != question.title {
		t.Fatal("declining did not return to the action menu", ui.menus)
	}
	if st := saved(t, e); len(st.Pending) != 0 || !st.HasSystem("apt", "zsh") {
		t.Fatal("the confirmation did not clear the mark, or the step did not run", st)
	}
	if got := w.calls("apply:"); !slices.Equal(got, []string{"tool.other", "tool.zsh"}) {
		t.Fatal(got)
	}
}

func TestFailureIsReportedOneTime(t *testing.T) {
	e, w := fixture(t)
	w.resource("tool.other").applyErr = errors.New("download failed")
	ui := &scriptedUI{t: t, tools: choose("other")}
	e.UI = ui
	result, err := e.Run(context.Background())
	var reported *engine.ReportedError
	if !errors.As(err, &reported) || result.Status != "failed" || len(ui.reports) != 1 || ui.reports[0].Status != "failed" {
		t.Fatal(result, err, ui.reports)
	}
	if !strings.HasPrefix(result.Message, "1 of 1 steps failed\nOther tool (tool.other): download failed") {
		t.Fatal(result.Message)
	}
	e.UI = nil
	if _, err := e.Run(context.Background()); err == nil {
		t.Fatal("the menu ran without an interface")
	}
}

func TestReviewOffersToReplaceChangedContent(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	w.resource("config.editor").changed = true
	ui := &scriptedUI{t: t, answers: []string{"update"}}
	e.UI = ui
	result, err := e.Run(context.Background())
	if err != nil || result.Status != "needs-action" || len(w.replaced) != 0 || !w.resource("config.editor").changed {
		t.Fatal("an unchecked choice replaced changed content", result, err)
	}
	choice := ui.reviews[0].Choices
	if len(choice) != 1 || choice[0].Group != engine.ReviewReplace || choice[0].ID != "config.editor" || choice[0].Label != "Editor configuration" || choice[0].Checked || !strings.Contains(choice[0].Detail, "Save the current content separately") {
		t.Fatal(choice)
	}
	ui = &scriptedUI{t: t, answers: []string{"apply"}, tools: choose("editor")}
	ui.review = func(ctx context.Context, proposal engine.Proposal, revise engine.Revise) (bool, error) {
		choices := slices.Clone(proposal.Choices)
		choices[0].Checked = true
		revised, err := revise(ctx, choices)
		if err != nil || len(revised.Choices) != 1 || !revised.Choices[0].Checked {
			t.Fatal(revised, err)
		}
		if op := operation(t, revised.Plan, "config.editor"); op.Action != "repair" || !op.Finding.Replace {
			t.Fatal(op)
		}
		return true, nil
	}
	e.UI = ui
	result, err = e.Run(context.Background())
	if err != nil || result.Status != "ready" || !slices.Equal(w.replaced, []string{"config.editor"}) {
		t.Fatal(result, err, w.replaced)
	}
}
