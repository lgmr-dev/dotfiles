package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/state"
)

func TestInstallUsesDependencyOrderAndRemovalTheReverse(t *testing.T) {
	e, w := fixture(t)
	plan, err := e.Preview(context.Background(), selection("editor"))
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(plan); !slices.Equal(got, []string{"install:tool.git", "install:tool.editor", "install:config.editor"}) {
		t.Fatal(got)
	}
	if op := operation(t, plan, "tool.git"); !slices.Equal(op.RequiredBy, []string{"editor"}) || op.Reason != "required by selection" {
		t.Fatal(op)
	}
	if _, err := os.Lstat(e.StateDir); !os.IsNotExist(err) {
		t.Fatal("a preview created the state folder", err)
	}
	mustApply(t, e, selection("editor"))
	if got := w.calls("apply:"); !slices.Equal(got, []string{"tool.git", "tool.editor", "config.editor"}) {
		t.Fatal(got)
	}
	plan, err = e.Preview(context.Background(), selection("editor"))
	if err != nil || !slices.Equal(actions(plan), []string{"keep:tool.git", "keep:tool.editor", "keep:config.editor"}) {
		t.Fatal(actions(plan), err)
	}
	request := selection()
	request.RemoveShared = []string{"tool.git"}
	mustApply(t, e, request)
	if got := w.calls("remove:"); !slices.Equal(got, []string{"config.editor", "tool.editor", "tool.git"}) {
		t.Fatal(got)
	}
	if st := saved(t, e); len(st.Targets) != 0 || len(st.Selected) != 0 {
		t.Fatal("removal left records", st)
	}
}

func TestSharedResourceStaysUntilRemovalByName(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	plan, err := e.Preview(context.Background(), selection())
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(plan); !slices.Equal(got, []string{"remove:config.editor", "remove:tool.editor", "retain:tool.git"}) {
		t.Fatal(got)
	}
	if op := operation(t, plan, "tool.git"); !strings.Contains(op.Reason, "explicit per-item removal required") {
		t.Fatal(op.Reason)
	}
	mustApply(t, e, selection())
	if slices.Contains(w.calls("remove:"), "tool.git") || !w.resource("tool.git").present {
		t.Fatal("removed a shared resource without a removal by name")
	}
	plan, err = e.Preview(context.Background(), selection())
	if err != nil || !slices.Equal(actions(plan), []string{"retain:tool.git"}) {
		t.Fatal("retention is not derived on each run", actions(plan), err)
	}
	request := selection()
	request.RemoveShared = []string{"tool.other"}
	if _, err := e.Preview(context.Background(), request); err == nil {
		t.Fatal("accepted the removal of an unrecorded resource")
	}
}

func TestChangedTargetHoldsItsResourceAndRequirements(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	w.resource("config.editor").changed = true
	request := selection()
	request.RemoveShared = []string{"tool.git"}
	plan, err := e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(plan); !slices.Equal(got, []string{"retain:config.editor", "retain:tool.editor", "retain:tool.git"}) {
		t.Fatal(got)
	}
	if reason := operation(t, plan, "tool.editor").Reason; reason != "required by another retained resource" {
		t.Fatal(reason)
	}
	if reason := operation(t, plan, "config.editor").Reason; !strings.Contains(reason, "changed content stays") {
		t.Fatal(reason)
	}
	if err := os.MkdirAll(w.key("config.editor"), 0700); err != nil {
		t.Fatal(err)
	}
	result := mustApply(t, e, request)
	if len(w.calls("remove:")) != 0 || len(saved(t, e).Targets) != 3 {
		t.Fatal("released a retained resource")
	}
	if !strings.Contains(result.Message, "Preserved files for inspection: "+w.key("config.editor")) {
		t.Fatal("the preserved content was not reported", result.Message)
	}
}

func TestRecordedSystemPackageStaysWithItsRemovalCommand(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("shell"))
	if st := saved(t, e); !st.HasSystem("apt", "zsh") {
		t.Fatal("fixture did not record the system package", st)
	}
	plan, err := e.Preview(context.Background(), selection("shell"))
	if err != nil || len(operation(t, plan, "tool.zsh").Commands) != 1 {
		t.Fatal("a wanted resource lost its command block", err)
	}
	plan, err = e.Preview(context.Background(), selection())
	if err != nil {
		t.Fatal(err)
	}
	op := operation(t, plan, "tool.zsh")
	if op.Action != "retain" || !strings.Contains(op.Reason, "never removes a system package") || len(op.Commands) != 1 || op.Commands[0].By != "you" {
		t.Fatal(op)
	}
	mustApply(t, e, selection())
	if len(w.calls("remove:")) != 0 || !saved(t, e).HasSystem("apt", "zsh") {
		t.Fatal("setup ran a removal of a system package")
	}
	w.resource("tool.zsh").present = false
	plan, err = e.Preview(context.Background(), selection())
	if err != nil || !slices.Equal(actions(plan), []string{"forget:tool.zsh"}) {
		t.Fatal(actions(plan), err)
	}
	mustApply(t, e, selection())
	if saved(t, e).HasSystem("apt", "zsh") {
		t.Fatal("the record of an absent package stayed")
	}
	plan, err = e.Preview(context.Background(), selection())
	if err != nil || len(plan.Operations) != 0 {
		t.Fatal(actions(plan), err)
	}
}

func TestRetainMarkAndUnrecordedResources(t *testing.T) {
	e, w := fixture(t)
	w.resource("tool.other").present = true
	if _, err := apply(t, e, selection("packages", "other")); !errors.Is(err, engine.ErrReplan) {
		t.Fatal(err)
	}
	mustApply(t, e, selection("packages", "other"))
	plan, err := e.Preview(context.Background(), selection())
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(plan); !slices.Equal(got, []string{"remove:tool.afterboot", "retain:infra.boot"}) {
		t.Fatal("an unrecorded resource got an operation, or the retain mark was ignored", got)
	}
	if reason := operation(t, plan, "infra.boot").Reason; reason != "retained infrastructure or personal data" {
		t.Fatal(reason)
	}
}

func TestKeptResourceStaysInTheClosure(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	request := selection()
	request.Keep = []string{"tool.editor"}
	plan, err := e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(plan); !slices.Equal(got, []string{"keep:tool.git", "keep:tool.editor", "remove:config.editor"}) {
		t.Fatal(got)
	}
	if _, err := e.Apply(context.Background(), request, plan); err != nil {
		t.Fatal(err)
	}
	if st := saved(t, e); !slices.Equal(st.Keep, []string{"tool.editor"}) {
		t.Fatal("kept resources were not saved", st.Keep)
	}
	plan, err = e.Preview(context.Background(), engine.Request{Mode: "apply"})
	if err != nil || !slices.Equal(actions(plan), []string{"keep:tool.git", "keep:tool.editor"}) || !slices.Equal(plan.Keep, []string{"tool.editor"}) {
		t.Fatal("a run without a selection does not use the saved one", actions(plan), err)
	}
	if len(w.calls("remove:")) != 1 {
		t.Fatal(w.calls("remove:"))
	}
	bad := selection()
	bad.Keep, bad.RemoveShared = []string{"tool.git"}, []string{"tool.git"}
	if _, err := e.Preview(context.Background(), bad); err == nil {
		t.Fatal("a resource was both kept and removed")
	}
}

func TestApplyRepairsAndKeepsOutdatedWhileUpdateUpdates(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor", "other"))
	w.resource("tool.editor").outdated = true
	w.resource("tool.other").unhealthy = true
	plan, err := e.Preview(context.Background(), engine.Request{Mode: "apply"})
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(plan); !slices.Equal(got, []string{"keep:tool.git", "keep:tool.editor", "keep:config.editor", "repair:tool.other"}) {
		t.Fatal(got)
	}
	if reason := operation(t, plan, "tool.editor").Reason; !strings.Contains(reason, "choose Update tools") {
		t.Fatal(reason)
	}
	if reason := operation(t, plan, "tool.other").Reason; reason != "selected resource failed its check: its command fails" {
		t.Fatal(reason)
	}
	plan, err = e.Preview(context.Background(), engine.Request{Mode: "update"})
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(plan); !slices.Equal(got, []string{"keep:tool.git", "update:tool.editor", "keep:config.editor", "repair:tool.other"}) {
		t.Fatal(got)
	}
	before := len(w.calls("apply:"))
	result, err := e.Apply(context.Background(), engine.Request{Mode: "update"}, plan)
	if err != nil || result.Status != "ready" || !strings.Contains(result.Message, "Note: Editor program 2.0 is available") {
		t.Fatal(result, err)
	}
	if got := w.calls("apply:")[before:]; !slices.Equal(got, []string{"tool.editor", "tool.other"}) {
		t.Fatal(got)
	}
	for _, request := range []engine.Request{{Mode: "update", Selected: []string{"other"}, SelectSet: true}, {Mode: "update", RemoveShared: []string{"tool.git"}}, {Mode: "check", Keep: []string{}}, {Mode: "plan"}} {
		if _, err := e.Preview(context.Background(), request); err == nil {
			t.Error("accepted", request)
		}
	}
	if _, err := e.Preview(context.Background(), selection("tool.git")); err == nil || !strings.Contains(err.Error(), "unknown capability") {
		t.Fatal("a hidden resource was selected", err)
	}
}

func TestUpdateDoesNotRemoveADeselectedResource(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor", "other"))
	store, err := state.Open(e.StateDir, e.Target)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(store.Update(func(s *state.State) error { s.Selected = []string{"editor"}; return nil }), store.Close()); err != nil {
		t.Fatal(err)
	}
	plan, err := e.Preview(context.Background(), engine.Request{Mode: "update"})
	if err != nil {
		t.Fatal(err)
	}
	if op := operation(t, plan, "tool.other"); op.Action != "retain" || op.Reason != "selection changes and removal require a separate apply request" {
		t.Fatal(op)
	}
	if _, err := e.Apply(context.Background(), engine.Request{Mode: "update"}, plan); err != nil || len(w.calls("remove:")) != 0 {
		t.Fatal("update removed a resource", err)
	}
}

func TestChangedTokenStopsTheStep(t *testing.T) {
	e, w := fixture(t)
	request := selection("editor")
	plan, err := e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	w.resource("tool.editor").generation++
	result, err := e.Apply(context.Background(), request, plan)
	if !errors.Is(err, engine.ErrPlanChanged) || result.Status != "failed" {
		t.Fatal(result, err)
	}
	if got := w.calls("apply:"); !slices.Equal(got, []string{"tool.git"}) {
		t.Fatal("a step ran for a different observation", got)
	}
	if w.swept != 0 || saved(t, e).Source != "" {
		t.Fatal("a stopped pass ran the sweep or saved the source")
	}
	other := selection("other")
	if _, err := e.Apply(context.Background(), other, plan); !errors.Is(err, engine.ErrPlanChanged) {
		t.Fatal("a plan was applied for another selection", err)
	}
	if _, err := e.Apply(context.Background(), engine.Request{Mode: "check"}, plan); err == nil {
		t.Fatal("a check request applied changes")
	}
}

func TestFailedStepBlocksOnlyItsDependents(t *testing.T) {
	e, w := fixture(t)
	w.resource("tool.git").applyErr = errors.New("download failed")
	result, err := apply(t, e, selection("editor", "other"))
	if err == nil || result.Status != "failed" || !strings.Contains(err.Error(), "1 of 4 steps failed") {
		t.Fatal(result, err)
	}
	if got := w.calls("apply:"); !slices.Equal(got, []string{"tool.git", "tool.other"}) {
		t.Fatal(got)
	}
	for _, want := range []string{"Git (tool.git): download failed", "Editor program (tool.editor): waiting for tool.git: its install step failed", "Editor configuration (config.editor): waiting for tool.git"} {
		if !strings.Contains(result.Message, want) {
			t.Fatalf("message lacks %q:\n%s", want, result.Message)
		}
	}
	st := saved(t, e)
	if w.swept != 0 || st.Source != "" || !slices.Equal(st.Selected, []string{"editor", "other"}) {
		t.Fatal("a failed pass ran the sweep, saved the source or lost the selection", st)
	}
	w.resource("tool.git").applyErr = nil
	mustApply(t, e, engine.Request{Mode: "apply"})
	if got := w.calls("apply:"); !slices.Equal(got, []string{"tool.git", "tool.other", "tool.git", "tool.editor", "config.editor"}) {
		t.Fatal("a later run did not converge", got)
	}
	if w.swept != 1 || saved(t, e).Source != "source-1" {
		t.Fatal("a pass without a failure did not run the sweep or save the source")
	}
}

func TestFailedVerificationFailsTheStep(t *testing.T) {
	e, w := fixture(t)
	w.resource("tool.other").afterApply = func() { w.resource("tool.other").unhealthy = true }
	result, err := apply(t, e, selection("other"))
	if err == nil || !strings.Contains(result.Message, "did not pass verification after its install step: unhealthy: its command fails") {
		t.Fatal(result, err)
	}
}

func TestFailedRemovalKeepsItsRequirements(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	w.resource("config.editor").removeErr = errors.New("target is busy")
	request := selection()
	request.RemoveShared = []string{"tool.git"}
	result, err := apply(t, e, request)
	if err == nil || result.Status != "failed" {
		t.Fatal(result, err)
	}
	if got := w.calls("remove:"); !slices.Equal(got, []string{"config.editor"}) {
		t.Fatal("a requirement of a failed removal was removed", got)
	}
	if !strings.Contains(result.Message, "config.editor still needs it: its removal failed") {
		t.Fatal(result.Message)
	}
}

func TestUnknownInspectionStaysLocal(t *testing.T) {
	e, w := fixture(t)
	w.resource("tool.git").unknown = true
	w.resource("tool.other").inspectErr = errors.New("permission denied")
	plan, err := e.Preview(context.Background(), selection("editor", "other", "shell"))
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(plan); !slices.Equal(got, []string{"pending:tool.git", "pending:tool.editor", "pending:config.editor", "pending:tool.other", "install:tool.zsh"}) {
		t.Fatal(got)
	}
	if reason := operation(t, plan, "tool.editor").Reason; !strings.HasPrefix(reason, "waiting for tool.git: ") {
		t.Fatal(reason)
	}
	if reason := operation(t, plan, "tool.other").Reason; !strings.Contains(reason, "inspection failed: permission denied") {
		t.Fatal(reason)
	}
	result, err := e.Apply(context.Background(), selection("editor", "other", "shell"), plan)
	if err != nil || result.Status != "needs-action" || !slices.Equal(w.calls("apply:"), []string{"tool.zsh"}) {
		t.Fatal(result, err, w.calls("apply:"))
	}
}

func TestReplanEndsThePassAfterSuccess(t *testing.T) {
	e, w := fixture(t)
	request := selection("packages", "other")
	plan, err := e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if reason := operation(t, plan, "infra.boot").Reason; !strings.Contains(reason, "review newly available provider inventory") {
		t.Fatal(reason)
	}
	result, err := e.Apply(context.Background(), request, plan)
	if !errors.Is(err, engine.ErrReplan) || result.Status != "needs-action" || !strings.Contains(result.Message, "Package manager is ready") {
		t.Fatal(result, err)
	}
	if got := w.calls("apply:"); !slices.Equal(got, []string{"tool.other", "infra.boot"}) || w.swept != 0 {
		t.Fatal("the pass continued after a new provider", got)
	}
	mustApply(t, e, request)
	if got := w.calls("apply:"); !slices.Equal(got, []string{"tool.other", "infra.boot", "tool.afterboot"}) || w.swept != 1 {
		t.Fatal(got, w.swept)
	}
	e, w = fixture(t)
	w.resource("tool.afterboot").present = true
	if result := mustApply(t, e, selection("packages")); result.Status != "ready" || w.swept != 1 {
		t.Fatal("a replan step without later work stopped the pass", result)
	}
}

func TestPendingMarkStopsARunWithoutTheMenuAndCheckReportsIt(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("other"))
	store, err := state.Open(e.StateDir, e.Target)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(store.Update(func(s *state.State) error { s.Mark("tool.zsh:install"); return nil }), store.Close()); err != nil {
		t.Fatal(err)
	}
	before := len(w.log)
	plan, err := e.Preview(context.Background(), selection("shell"))
	if err != nil || !slices.Equal(plan.Pending, []string{"tool.zsh:install"}) {
		t.Fatal(plan.Pending, err)
	}
	inspected := len(w.log)
	result, err := e.Apply(context.Background(), selection("shell"), plan)
	if !errors.Is(err, engine.ErrPending) || result.Status != "needs-action" || !strings.Contains(result.Message, "tool.zsh:install") {
		t.Fatal(result, err)
	}
	if len(w.log) != inspected || inspected == before {
		t.Fatal("a run with a pending mark touched a provider", w.log[inspected:])
	}
	if st := saved(t, e); !slices.Equal(st.Selected, []string{"other"}) {
		t.Fatal("a stopped run saved its selection", st.Selected)
	}
	check, err := e.Check(context.Background())
	if err != nil || check.Status != "needs-action" || !strings.Contains(check.Message, "An earlier step did not report its end: tool.zsh:install") {
		t.Fatal(check, err)
	}
}

func TestCheckOnlyReads(t *testing.T) {
	e, w := fixture(t)
	check, err := e.Check(context.Background())
	if err != nil || check.Status != "ready" || check.Plan.Mode != "check" {
		t.Fatal(check, err)
	}
	if _, err := os.Lstat(e.StateDir); !os.IsNotExist(err) {
		t.Fatal("a check created the state folder", err)
	}
	mustApply(t, e, selection("editor", "other"))
	if check, err = e.Check(context.Background()); err != nil || check.Status != "ready" || check.Message != "All selected resources passed verification." {
		t.Fatal(check, err)
	}
	w.resource("tool.other").unhealthy = true
	w.resource("tool.editor").present = false
	w.resource("config.editor").changed = true
	w.resource("tool.git").outdated = true
	path := filepath.Join(e.StateDir, state.File)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(e.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	calls := len(w.calls("apply:")) + len(w.calls("remove:"))
	check, err = e.Check(context.Background())
	if err != nil || check.Status != "needs-action" {
		t.Fatal(check, err)
	}
	for _, want := range []string{"Other tool (tool.other): selected resource failed its check", "Editor program (tool.editor): required by selection", "Editor configuration (config.editor): changed content stays as it is", "Note: Git 2.0 is available"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("check lacks %q:\n%s", want, check.Message)
		}
	}
	if strings.Contains(check.Message, "Git (tool.git)") {
		t.Fatal("an available update was reported as a difference", check.Message)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("a check changed the state", err)
	}
	now, err := os.ReadDir(e.StateDir)
	if err != nil || len(now) != len(entries) {
		t.Fatal("a check created a file", now, err)
	}
	if len(w.calls("apply:"))+len(w.calls("remove:")) != calls || w.swept != 1 || w.elevated != 0 {
		t.Fatal("a check changed the system")
	}
}

func TestSelectionIsSavedAfterApproval(t *testing.T) {
	e, _ := fixture(t)
	if _, err := e.Preview(context.Background(), selection("editor")); err != nil {
		t.Fatal(err)
	}
	if st := saved(t, e); st.Schema != 0 || len(st.Selected) != 0 {
		t.Fatal("a preview saved the selection", st)
	}
	mustApply(t, e, selection("editor"))
	st := saved(t, e)
	if st.Schema != 2 || !slices.Equal(st.Selected, []string{"editor"}) || st.Source != "source-1" || st.Target != target {
		t.Fatal(st)
	}
	e.Source = "source-2"
	mustApply(t, e, engine.Request{Mode: "update"})
	if st := saved(t, e); st.Source != "source-2" || !slices.Equal(st.Selected, []string{"editor"}) {
		t.Fatal("update changed the selection or did not save the source", st)
	}
}

func TestPrivilegedStepStartsOneAdministratorSession(t *testing.T) {
	e, w := fixture(t)
	w.resource("tool.zsh").privileged = true
	mustApply(t, e, selection("shell", "other"))
	if w.elevated != 1 || w.released != 1 {
		t.Fatal(w.elevated, w.released)
	}
	mustApply(t, e, selection("shell", "other"))
	if w.elevated != 1 {
		t.Fatal("a run without a privileged step asked for administrator rights")
	}
}

func TestSecondRunIsExcludedWhileTheFirstHoldsTheLock(t *testing.T) {
	e, w := fixture(t)
	plan, err := e.Preview(context.Background(), selection("other"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(e.StateDir, e.Target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(context.Background(), selection("other"), plan); err == nil || !strings.Contains(err.Error(), "another installer holds the mutation lock") {
		t.Fatal("a second run was admitted", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if len(w.calls("apply:")) != 0 {
		t.Fatal("an excluded run changed the system")
	}
}

type recordedProgress struct {
	steps  []string
	output strings.Builder
}

func (p *recordedProgress) context() context.Context {
	return engine.WithProgress(context.Background(), engine.Progress{Step: func(text string) { p.steps = append(p.steps, text) }, Output: &p.output})
}

func TestApplyNumbersEachStepAndWritesTheLog(t *testing.T) {
	e, w := fixture(t)
	w.resource("tool.git").present = true
	request := selection("editor")
	plan, err := e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	progress := &recordedProgress{}
	result, err := e.Apply(progress.context(), request, plan)
	if err != nil || result.Status != "ready" || filepath.Dir(result.Log) != filepath.Join(e.StateDir, "logs") {
		t.Fatal(result, err)
	}
	if len(progress.steps) != 5 || progress.steps[0] != "[1/2] Installing Editor program" || !strings.HasPrefix(progress.steps[1], "[1/2] Editor program done in ") ||
		progress.steps[2] != "[2/2] Installing Editor configuration" || progress.steps[4] != "Removing unused private builds" {
		t.Fatal(progress.steps)
	}
	data, err := os.ReadFile(result.Log)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range progress.steps {
		if !strings.Contains(string(data), " "+step+"\n") {
			t.Fatalf("log lacks displayed step %q:\n%s", step, data)
		}
	}
	if info, err := os.Stat(result.Log); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("log is not private", info, err)
	}
	engine.ReportDetail(progress.context(), "detail %d", 1)
	if progress.output.String() != "detail 1\n" {
		t.Fatal(progress.output.String())
	}
}

func TestApplyLogKeepsOnlyTheNewestLogs(t *testing.T) {
	e, _ := fixture(t)
	directory := filepath.Join(e.StateDir, "logs")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	old := []string{}
	for i := range 25 {
		name := "apply-2026010" + string(rune('0'+i/10)) + "T0000" + string(rune('0'+i%100/10)) + string(rune('0'+i%10)) + "Z-1.log"
		old = append(old, name)
		if err := os.WriteFile(filepath.Join(directory, name), []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "personal notes.log"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	result := mustApply(t, e, selection("other"))
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(old)
	want := append(append([]string{}, old[len(old)-19:]...), filepath.Base(result.Log), "personal notes.log")
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatal("log retention removed the wrong files", names)
	}
}

func TestUnavailableApplyLogIsANoteNotAFailure(t *testing.T) {
	e, _ := fixture(t)
	if err := os.MkdirAll(e.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.StateDir, "logs"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := apply(t, e, selection("other"))
	if err != nil || result.Status != "ready" || result.Log != "" || !strings.Contains(result.Message, "Apply log issue: ") {
		t.Fatal(result, err)
	}
}

func TestSystemRecordNames(t *testing.T) {
	c := fixtureCatalog()
	zsh, _ := c.Resource("tool.zsh")
	provider, name, ok := engine.SystemRecord(zsh, linux)
	if !ok || provider != "apt" || name != "zsh" {
		t.Fatal(provider, name, ok)
	}
	git, _ := c.Resource("tool.git")
	if _, _, ok := engine.SystemRecord(git, linux); ok {
		t.Fatal("a resource without a package binding has a system name")
	}
	st := state.State{System: map[string][]string{"windows": {"tool.git"}}, Targets: map[string]state.Record{"/x": {Resource: "tool.other"}}}
	other, _ := c.Resource("tool.other")
	if !engine.SystemRecorded(st, git, linux) || engine.SystemRecorded(st, zsh, linux) || !engine.Recorded(st, other, linux) || engine.Recorded(st, zsh, linux) {
		t.Fatal("record lookup is wrong")
	}
	if engine.Changed.String() != "changed" || engine.Status(99).String() != "invalid" {
		t.Fatal("status names changed")
	}
}

func TestChangedContentStaysUntilAnApprovedReplacement(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	w.resource("config.editor").changed = true
	before := len(w.calls("apply:"))
	request := engine.Request{Mode: "apply"}
	plan, err := e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if op := operation(t, plan, "config.editor"); op.Action != "pending" || op.Finding.Replace {
		t.Fatal(op)
	}
	result, err := e.Apply(context.Background(), request, plan)
	if err != nil || result.Status != "needs-action" || len(w.calls("apply:")) != before || len(w.replaced) != 0 || !w.resource("config.editor").changed {
		t.Fatal("changed content was not preserved by default", result, err)
	}
	for _, bad := range []engine.Request{{Mode: "apply", Replace: []string{"tool.editor"}}, {Mode: "apply", Replace: []string{"tool.other"}}, {Mode: "check", Replace: []string{"config.editor"}}} {
		if _, err := e.Preview(context.Background(), bad); err == nil {
			t.Error("accepted", bad)
		}
	}
	request.Replace = []string{"config.editor"}
	if _, err := e.Apply(context.Background(), request, plan); !errors.Is(err, engine.ErrPlanChanged) || len(w.replaced) != 0 {
		t.Fatal("a plan without the replacement approved one", err)
	}
	plan, err = e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	op := operation(t, plan, "config.editor")
	if op.Action != "repair" || !op.Finding.Replace || !strings.Contains(op.Reason, "save the current content, then replace it") || !slices.Equal(plan.Replace, []string{"config.editor"}) {
		t.Fatal(op)
	}
	w.resource("config.editor").generation++
	if _, err := e.Apply(context.Background(), request, plan); !errors.Is(err, engine.ErrPlanChanged) || len(w.replaced) != 0 {
		t.Fatal("the token check did not apply to a replacement", err)
	}
	result = mustApply(t, e, request)
	if !slices.Equal(w.replaced, []string{"config.editor"}) || w.resource("config.editor").changed {
		t.Fatal("the approved replacement did not reach the provider", w.replaced, result)
	}
	w.resource("config.editor").changed = true
	w.replaced = nil
	update := engine.Request{Mode: "update", Replace: []string{"config.editor"}}
	mustApply(t, e, update)
	if !slices.Equal(w.replaced, []string{"config.editor"}) {
		t.Fatal("update did not accept the approved replacement")
	}
}
