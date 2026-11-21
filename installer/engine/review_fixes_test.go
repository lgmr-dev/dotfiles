package engine_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func TestActionMenuOffersTheRestoreOnlyWhileTheFallbackIsOpen(t *testing.T) {
	e, _ := fixture(t)
	mustApply(t, e, selection("editor"))
	called := 0
	e.RestoreV2 = func(_ context.Context, ui engine.Interaction) (engine.Result, error) {
		called++
		if ui == nil {
			t.Error("the restore received no interface")
		}
		return engine.Result{Status: "ready", Message: "restored"}, nil
	}
	ui := &scriptedUI{t: t, answers: []string{"exit"}}
	e.UI = ui
	if _, err := e.Run(context.Background()); err != nil || slices.Contains(ui.menus[0].labels, "Restore release v2") {
		t.Fatal("the restore was offered without an open fallback", ui.menus, err)
	}
	setFallback(t, e, "open")
	ui = &scriptedUI{t: t, answers: []string{"restore-v2"}}
	e.UI = ui
	result, err := e.Run(context.Background())
	if err != nil || result.Status != "ready" || result.Message != "restored" || called != 1 {
		t.Fatal("the restore choice did not call the restore", result, err, called)
	}
	if want := []string{"Choose tools", "Update tools", "Check installation", "Restore release v2", "Exit"}; !slices.Equal(ui.menus[0].labels, want) {
		t.Fatal(ui.menus[0].labels)
	}
	e.RestoreV2 = func(context.Context, engine.Interaction) (engine.Result, error) {
		return engine.Result{}, engine.ErrBack
	}
	ui = &scriptedUI{t: t, answers: []string{"restore-v2", "exit"}}
	e.UI = ui
	if result, err := e.Run(context.Background()); err != nil || result.Status != "cancelled" || len(ui.menus) != 2 {
		t.Fatal("going back from the restore did not return to the menu", result, err)
	}
	e.RestoreV2 = nil
	ui = &scriptedUI{t: t, answers: []string{"exit"}}
	e.UI = ui
	if _, err := e.Run(context.Background()); err != nil || slices.Contains(ui.menus[0].labels, "Restore release v2") {
		t.Fatal("the restore was offered without a restore function", err)
	}
}

func TestCheckKeepsAV2FolderThatARecordedTargetStillNames(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	packages, recovery, restore := v2Folder(t, e, "packages"), v2Folder(t, e, "recovery"), v2Folder(t, e, "v2-restore")
	settings := filepath.Join(w.home, "settings.json")
	content, err := json.Marshal(map[string]string{"commandline": filepath.Join(packages, "tool.powershell", "current", "pwsh")})
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(os.MkdirAll(w.home, 0700), os.WriteFile(settings, content, 0600)); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(w.home, "profile")
	if err := os.WriteFile(profile, []byte("plain"), 0600); err != nil {
		t.Fatal(err)
	}
	baseline := base64.StdEncoding.EncodeToString([]byte("source " + filepath.Join(recovery, "old", "init")))
	edit(t, e, func(s *state.State) {
		s.Targets[settings+"#windows-terminal"] = state.Record{Resource: "config.editor", Kind: "field", Before: "fp:abc"}
		s.Targets[profile+"#block"] = state.Record{Resource: "config.editor", Kind: "block", Applied: "fp:abc", Before: "value:" + baseline}
	})
	check, err := e.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Note: " + packages + " is a folder of release v2 still in use by " + settings + "#windows-terminal; keep it",
		"Note: " + recovery + " is a folder of release v2 still in use by " + profile + "#block; keep it",
		"Owner cleanup: " + restore + " is a folder of release v2 that setup no longer uses; inspect it, then delete it yourself",
	} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("check lacks %q:\n%s", want, check.Message)
		}
	}
	if strings.Contains(check.Message, "Owner cleanup: "+packages) || strings.Contains(check.Message, "Owner cleanup: "+recovery) {
		t.Fatal("check told the owner to delete a folder that a recorded target still names", check.Message)
	}
	if err := errors.Join(os.Remove(profile), os.Mkdir(profile, 0700)); err != nil {
		t.Fatal(err)
	}
	if check, err = e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(check.Message, "Owner cleanup") || !strings.Contains(check.Message, "Note: setup cannot say if "+restore+" is still in use, because it could not read "+profile+"#block") ||
		!strings.Contains(check.Message, "Note: "+packages+" is a folder of release v2 still in use by ") {
		t.Fatal("check gave a cleanup instruction although a recorded target is unreadable", check.Message)
	}
}
