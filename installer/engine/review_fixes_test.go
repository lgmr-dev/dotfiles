package engine_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

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
	w.resource("tool.zsh").afterApply = func() { w.resource("tool.zsh").unhealthy = true }
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

func TestAKeptResourceThatChangesAfterTheReviewStopsTheRun(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	request := selection("editor", "other")
	plan, err := e.Preview(context.Background(), request)
	if err != nil || operation(t, plan, "config.editor").Action != "keep" {
		t.Fatal(actions(plan), err)
	}
	w.resource("config.editor").changed = true
	result, err := e.Apply(context.Background(), request, plan)
	if !errors.Is(err, engine.ErrPlanChanged) || result.Status != "failed" || strings.Contains(result.Message, "All selected resources passed verification.") {
		t.Fatal("a kept resource changed after the review and the run continued", result, err)
	}
	if slices.Contains(w.calls("apply:"), "tool.other") || !slices.Equal(saved(t, e).Selected, []string{"editor"}) {
		t.Fatal("the run changed the system or the selection before it stopped")
	}
	w.resource("config.editor").changed = false
	ui := &scriptedUI{t: t, answers: []string{"apply"}, tools: choose("editor")}
	ui.review = func(context.Context, engine.Proposal, engine.Revise) (bool, error) {
		if len(ui.reviews) == 1 {
			w.resource("config.editor").changed = true
		}
		return len(ui.reviews) == 1, nil
	}
	e.UI = ui
	if result, err := e.Run(context.Background()); err != nil || result.Status != "cancelled" {
		t.Fatal(result, err)
	}
	if len(ui.reviews) != 2 || !strings.Contains(ui.reviews[1].Notice, "Your system changed after the review. This is the updated plan.") || operation(t, ui.reviews[1].Plan, "config.editor").Action != "pending" || len(ui.reports) != 0 {
		t.Fatal("the menu did not show the updated plan", len(ui.reviews), ui.reports)
	}
}

func TestAFailedFirstStepWithoutAChangeLeavesTheFallbackOpen(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	setFallback(t, e, "open")
	request := selection("editor", "other")
	plan, err := e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	w.resource("tool.other").applyErr = errors.New("permission denied")
	result, err := e.Apply(context.Background(), request, plan)
	if err == nil || !strings.Contains(result.Message, "permission denied") {
		t.Fatal(result, err)
	}
	if saved(t, e).V2Restore != "open" || !strings.Contains(result.Message, "Nothing changed, so the v2 fallback stays open.") {
		t.Fatal("a failed first step that wrote nothing closed the fallback", saved(t, e).V2Restore, result.Message)
	}
	w.resource("tool.other").applyErr = nil
	w.resource("tool.other").afterApply = func() { w.resource("tool.other").unhealthy = true }
	if plan, err = e.Preview(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result, err = e.Apply(context.Background(), request, plan)
	if err == nil || !strings.Contains(result.Message, "did not pass verification") {
		t.Fatal(result, err)
	}
	if saved(t, e).V2Restore != "" || strings.Contains(result.Message, "fallback stays open") {
		t.Fatal("a failed step that changed the state left the fallback open", saved(t, e).V2Restore, result.Message)
	}
}

func TestOnlyAStepThatRunsTheProviderShowsTheCommandsOfSetup(t *testing.T) {
	st := state.State{Schema: 2, Target: target, Selected: []string{"other"}, Keep: []string{}, System: map[string][]string{}, Targets: map[string]state.Record{}}
	blocks := []engine.CommandBlock{
		{Title: "Update Other tool", Lines: []string{"sudo apt-get install other"}, By: "setup"},
		{Title: "Repair Other tool", Lines: []string{"sudo apt-get install --reinstall other"}, By: "you"},
	}
	found := engine.Findings{Wanted: map[string]engine.Finding{
		"tool.other": {Status: engine.Outdated, Notes: []string{"Other tool 2.0 is available"}, Commands: blocks, Token: "1.0"},
	}}
	for mode, want := range map[string][]engine.CommandBlock{"apply": blocks[1:], "update": blocks} {
		plan, err := engine.PlanChanges(fixtureCatalog(), linux, st, engine.Request{Mode: mode}, found, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		op := operation(t, plan, "tool.other")
		if !slices.EqualFunc(op.Commands, want, func(a, b engine.CommandBlock) bool { return a.Title == b.Title && a.By == b.By }) {
			t.Fatalf("%s %s shows %+v", mode, op.Action, op.Commands)
		}
		if !slices.Equal(op.Finding.Notes, []string{"Other tool 2.0 is available"}) {
			t.Fatalf("%s lost the note of the update: %+v", mode, op.Finding.Notes)
		}
	}
}

func TestCheckKeepsAV2FolderThatOnlyASavedBaselineNames(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	packages, recovery := v2Folder(t, e, "packages"), v2Folder(t, e, "recovery")
	baselines := filepath.Join(w.home, "baselines")
	if err := os.MkdirAll(filepath.Join(baselines, "folder", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(baselines, "file")
	nested := filepath.Join(baselines, "folder", "nested", "init")
	err := errors.Join(
		os.WriteFile(file, []byte("source "+filepath.Join(packages, "tool.zsh", "current", "init.zsh")), 0600),
		os.WriteFile(nested, []byte("dofile('"+filepath.Join(recovery, "old", "init.lua")+"')"), 0600),
		os.WriteFile(filepath.Join(w.home, "profile"), []byte("plain"), 0600),
	)
	if err != nil {
		t.Fatal(err)
	}
	edit(t, e, func(s *state.State) {
		s.Targets[filepath.Join(w.home, "profile")] = state.Record{Resource: "config.editor", Kind: "file", Applied: "fp:abc", Before: "backup:" + file}
		s.Targets[filepath.Join(w.home, "nvim")] = state.Record{Resource: "config.editor", Kind: "tree", Applied: "fp:abc", Before: "backup:" + filepath.Join(baselines, "folder")}
	})
	check, err := e.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Note: " + packages + " is a folder of release v2 still in use by " + filepath.Join(w.home, "profile") + "; keep it",
		"Note: " + recovery + " is a folder of release v2 still in use by " + filepath.Join(w.home, "nvim") + "; keep it",
	} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("check lacks %q:\n%s", want, check.Message)
		}
	}
	if err := os.Chmod(nested, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(nested, 0600) })
	if _, err := os.ReadFile(nested); err == nil {
		t.Skip("this account reads a file without read permission")
	}
	if check, err = e.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(check.Message, "Owner cleanup") || !strings.Contains(check.Message, "Note: setup cannot say if "+recovery+" is still in use, because it could not read "+filepath.Join(w.home, "nvim")) {
		t.Fatal("check gave a cleanup instruction although a saved baseline is unreadable", check.Message)
	}
}

func TestCheckReportsAnUnfinishedRemovalOfADeselectedResource(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor", "other"))
	edit(t, e, func(s *state.State) {
		s.Selected = []string{"editor"}
		if err := s.BeginRelease(w.key("tool.other")); err != nil {
			t.Fatal(err)
		}
	})
	check, err := e.Check(context.Background())
	if err != nil || check.Status != "needs-action" || !strings.Contains(check.Message, "unfinished change: "+w.key("tool.other")+" (tool.other)") {
		t.Fatal("check hid the unfinished removal of a deselected resource", check, err)
	}
}

func TestCheckReportsAnUnfinishedWriteOfAWantedResource(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("other"))
	edit(t, e, func(s *state.State) {
		if err := s.BeginTarget(w.key("tool.other"), state.Record{Resource: "tool.other", Kind: "file", Next: "fp:2"}); err != nil {
			t.Fatal(err)
		}
	})
	check, err := e.Check(context.Background())
	if err != nil || check.Status != "needs-action" || !strings.Contains(check.Message, "unfinished change: "+w.key("tool.other")+" (tool.other)") {
		t.Fatal("check hid the unfinished write of a wanted resource", check, err)
	}
}

func TestAnOperationThatChangesAfterTheReviewStopsTheRunBeforeAnyChange(t *testing.T) {
	e, w := fixture(t)
	setFallback(t, e, "open")
	request := selection("other", "shell")
	plan, err := e.Preview(context.Background(), request)
	if err != nil || !slices.Equal(actions(plan), []string{"install:tool.other", "install:tool.zsh"}) {
		t.Fatal(actions(plan), err)
	}
	w.resource("tool.zsh").generation++
	result, err := e.Apply(context.Background(), request, plan)
	if !errors.Is(err, engine.ErrPlanChanged) || result.Status != "failed" {
		t.Fatal(result, err)
	}
	st := saved(t, e)
	if got := w.calls("apply:"); len(got) != 0 || len(st.Selected) != 0 || len(st.Targets) != 0 || st.V2Restore != "open" {
		t.Fatal("the run changed the system, the selection or the fallback before it showed the new plan", got, st.Selected, st.V2Restore)
	}
}

func TestTheRetryOfAnInterruptedReplacementAcceptsItsReplaceFlag(t *testing.T) {
	e, w := fixture(t)
	edit(t, e, func(s *state.State) {
		s.Targets = map[string]state.Record{w.key("config.editor"): {Resource: "config.editor", Kind: "file", Next: "fp:1", Before: "absent"}}
	})
	request := selection("editor")
	request.Replace = []string{"config.editor"}
	plan, err := e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal("the retry of an approved replacement was refused", err)
	}
	if op := operation(t, plan, "config.editor"); op.Action != "install" || !op.Finding.Replace {
		t.Fatal(op)
	}
	if result, err := e.Apply(context.Background(), request, plan); err != nil || result.Status != "ready" || !slices.Equal(w.replaced, []string{"config.editor"}) {
		t.Fatal("the retry did not complete the replacement", result, err, w.replaced)
	}
	request.Replace = []string{"tool.editor"}
	if _, err := e.Preview(context.Background(), request); err == nil {
		t.Fatal("a replacement without changed content or an unfinished record was accepted")
	}
}

func TestDeselectingAResourceKeepsItWhileItsWriteIsUnfinished(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	edit(t, e, func(s *state.State) {
		if err := s.BeginTarget(w.key("config.editor"), state.Record{Resource: "config.editor", Kind: "file", Next: "fp:2"}); err != nil {
			t.Fatal(err)
		}
	})
	request := selection()
	request.RemoveShared = []string{"tool.git"}
	plan, err := e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(plan); !slices.Equal(got, []string{"retain:config.editor", "retain:tool.editor", "retain:tool.git"}) {
		t.Fatal(got)
	}
	want := "an unfinished change of " + w.key("config.editor") + " stays until setup finishes it: select Editor configuration again and run setup until its step passes, then deselect it"
	if reason := operation(t, plan, "config.editor").Reason; reason != want {
		t.Fatal(reason)
	}
	if _, err := e.Apply(context.Background(), request, plan); err != nil || len(w.calls("remove:")) != 0 {
		t.Fatal("the run removed a resource with an unfinished write", w.calls("remove:"), err)
	}
	if record := saved(t, e).Targets[w.key("config.editor")]; record.Next != "fp:2" {
		t.Fatal("the unfinished write lost its record", record)
	}
}

func TestAnInterruptedRemovalOfADeselectedResourceCompletes(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor", "other"))
	edit(t, e, func(s *state.State) {
		s.Selected = []string{"editor"}
		if err := s.BeginRelease(w.key("tool.other")); err != nil {
			t.Fatal(err)
		}
	})
	request := selection("editor")
	plan, err := e.Preview(context.Background(), request)
	if err != nil || operation(t, plan, "tool.other").Action != "remove" {
		t.Fatal(actions(plan), err)
	}
	if _, err := e.Apply(context.Background(), request, plan); err != nil || !slices.Equal(w.calls("remove:"), []string{"tool.other"}) {
		t.Fatal("the interrupted removal did not complete", w.calls("remove:"), err)
	}
	if _, recorded := saved(t, e).Targets[w.key("tool.other")]; recorded {
		t.Fatal("the completed removal left its record")
	}
}

func utf16Profile(order binary.AppendByteOrder, text string) []byte {
	data := order.AppendUint16(nil, 0xFEFF)
	for _, unit := range utf16.Encode([]rune(text)) {
		data = order.AppendUint16(data, unit)
	}
	return data
}

func TestCheckKeepsAV2FolderThatAUTF16ProfileNames(t *testing.T) {
	for name, order := range map[string]binary.AppendByteOrder{"little endian": binary.LittleEndian, "big endian": binary.BigEndian} {
		t.Run(name, func(t *testing.T) {
			e, w := fixture(t)
			mustApply(t, e, selection("editor"))
			packages := v2Folder(t, e, "packages")
			profile := filepath.Join(w.home, "Microsoft.PowerShell_profile.ps1")
			content := utf16Profile(order, "$env:PATH = '"+filepath.Join(packages, "tool.powershell", "current")+";' + $env:PATH\r\n")
			if err := errors.Join(os.MkdirAll(w.home, 0700), os.WriteFile(profile, content, 0600)); err != nil {
				t.Fatal(err)
			}
			edit(t, e, func(s *state.State) {
				s.Targets[profile+"#block"] = state.Record{Resource: "config.editor", Kind: "block", Applied: "fp:abc"}
			})
			check, err := e.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(check.Message, "Note: "+packages+" is a folder of release v2 still in use by "+profile+"#block; keep it") || strings.Contains(check.Message, "Owner cleanup: "+packages) {
				t.Fatal("check missed the v2 folder in a UTF-16 profile", check.Message)
			}
			if err := os.WriteFile(profile, append(content, 0), 0600); err != nil {
				t.Fatal(err)
			}
			if check, err = e.Check(context.Background()); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(check.Message, "Owner cleanup") || !strings.Contains(check.Message, "Note: setup cannot say if "+packages+" is still in use, because it could not read "+profile+"#block") {
				t.Fatal("check gave a cleanup instruction for a UTF-16 profile that does not decode", check.Message)
			}
		})
	}
}

func TestARetainedResourceThatIsBlockedShowsItsRepairCommand(t *testing.T) {
	st := state.State{Schema: 2, Target: target, Selected: []string{}, Keep: []string{}, System: map[string][]string{},
		Targets: map[string]state.Record{"/home/tool.other": {Resource: "tool.other", Kind: "file", Applied: "fp:1"}}}
	repair := engine.CommandBlock{Title: "Repair the dependent", Lines: []string{"brew reinstall --formula consumer"}, By: "you"}
	found := engine.Findings{Wanted: map[string]engine.Finding{}, Released: map[string]engine.Finding{
		"tool.other": {Status: engine.Blocked, Reason: "consumer is broken", Commands: []engine.CommandBlock{repair}, Token: "1"},
	}}
	plan, err := engine.PlanChanges(fixtureCatalog(), linux, st, engine.Request{Mode: "apply", SelectSet: true, Selected: []string{}}, found, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	op := operation(t, plan, "tool.other")
	if !slices.ContainsFunc(op.Commands, func(b engine.CommandBlock) bool { return b.Title == repair.Title && b.By == "you" }) {
		t.Fatalf("the repair command of a blocked retained resource is hidden: %s %+v", op.Action, op.Commands)
	}
}
