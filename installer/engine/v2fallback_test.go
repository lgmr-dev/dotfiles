package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/state"
)

func setFallback(t *testing.T, e *engine.Engine, value string) {
	t.Helper()
	store, err := state.Open(e.StateDir, e.Target)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Update(func(s *state.State) error { s.V2Restore = value; return nil })
	if err = errors.Join(err, store.Close()); err != nil {
		t.Fatal(err)
	}
}

func v2Folder(t *testing.T, e *engine.Engine, name string) string {
	t.Helper()
	folder := filepath.Join(e.StateDir, name)
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	return folder
}

func TestTheFirstConfirmedRunClosesTheOpenFallbackBeforeItsFirstChange(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	setFallback(t, e, "open")
	request := selection("editor", "other")
	plan, err := e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if plan.V2Restore != "open" || !strings.Contains(engine.FallbackNotice(plan), "This closes the v2 fallback") {
		t.Fatalf("the preview must say that the run closes the fallback: %+v", plan)
	}
	if saved(t, e).V2Restore != "open" {
		t.Fatal("a preview must not close the fallback")
	}
	w.resource("tool.other").afterApply = func() {
		if saved(t, e).V2Restore != "" {
			t.Error("the fallback must be closed before the first change")
		}
	}
	if result, err := e.Apply(context.Background(), request, plan); err != nil || result.Status != "ready" {
		t.Fatalf("%+v %v", result, err)
	}
	if saved(t, e).V2Restore != "" {
		t.Fatal("the confirmed run must close the fallback")
	}
	after, err := e.Preview(context.Background(), request)
	if err != nil || engine.FallbackNotice(after) != "" {
		t.Fatalf("a closed fallback needs no notice: %q %v", engine.FallbackNotice(after), err)
	}
	check, err := e.Preview(context.Background(), engine.Request{Mode: "check"})
	if err != nil || engine.FallbackNotice(check) != "" {
		t.Fatal("a check closes nothing and needs no notice")
	}
}

func TestAPlanFromBeforeTheFallbackChangedNeedsANewReview(t *testing.T) {
	e, _ := fixture(t)
	mustApply(t, e, selection("editor"))
	request := selection("editor", "other")
	plan, err := e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	setFallback(t, e, "open")
	if _, err := e.Apply(context.Background(), request, plan); !errors.Is(err, engine.ErrPlanChanged) {
		t.Fatalf("a plan that did not show the end of the fallback must not close it: %v", err)
	}
	if saved(t, e).V2Restore != "open" {
		t.Fatal("the fallback must stay open")
	}
}

func TestOnlyTheSwitchRunsWhileTheImportIsNotComplete(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	setFallback(t, e, "switching")
	request := selection("editor", "other")
	plan, err := e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(engine.FallbackNotice(plan), "The import from release v2 is not complete") {
		t.Fatalf("notice: %q", engine.FallbackNotice(plan))
	}
	steps := len(w.log)
	result, err := e.Apply(context.Background(), request, plan)
	if !errors.Is(err, engine.ErrSwitching) || result.Status != "needs-action" || len(w.log) != steps {
		t.Fatalf("an ordinary run must stop before any step: %+v %v", result, err)
	}
	check, err := e.Check(context.Background())
	if err != nil || check.Status != "needs-action" || !strings.Contains(check.Message, "Run setup to complete it, or run restore-v2") {
		t.Fatalf("check must report the unfinished switch: %+v %v", check, err)
	}
	e.Switch = true
	if result, err := e.Apply(context.Background(), request, plan); err != nil || result.Status != "ready" {
		t.Fatalf("the switch run must apply the plan: %+v %v", result, err)
	}
	if saved(t, e).V2Restore != "switching" {
		t.Fatal("the switch run must leave the mark for the import")
	}
	setFallback(t, e, "open")
	plan, err = e.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply(context.Background(), request, plan); err != nil || saved(t, e).V2Restore != "open" {
		t.Fatalf("the switch run must not close the fallback: %v", err)
	}
}

func TestCheckListsTheV2FoldersOnlyAfterTheFallbackCloses(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	check, err := e.Check(context.Background())
	if err != nil || strings.Contains(check.Message, "release v2") {
		t.Fatalf("an installation without folders of release v2 needs no note: %+v %v", check, err)
	}
	packages, recovery, restore := v2Folder(t, e, "packages"), v2Folder(t, e, "recovery"), v2Folder(t, e, "v2-restore")
	payload := filepath.Join(packages, "tool.app", "current")
	if err := os.MkdirAll(payload, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(w.home, "Application.app")
	if err := os.MkdirAll(w.home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(payload, link); err != nil {
		t.Skip("links are not available:", err)
	}
	setFallback(t, e, "open")
	check, err = e.Check(context.Background())
	if err != nil || check.Status != "ready" || strings.Contains(check.Message, "Owner cleanup") ||
		!strings.Contains(check.Message, "Note: the v2 fallback is open. Keep these folders of release v2 while restore-v2 can use them: "+packages+", "+recovery+", "+restore) {
		t.Fatalf("while the fallback is open check must say to keep the folders: %+v %v", check, err)
	}
	store, err := state.Open(e.StateDir, e.Target)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Update(func(s *state.State) error {
		s.V2Restore = ""
		s.Targets[link] = state.Record{Resource: "config.editor", Kind: "link", Applied: "link:abc", Before: "absent"}
		return nil
	})
	if err = errors.Join(err, store.Close()); err != nil {
		t.Fatal(err)
	}
	check, err = e.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Note: " + packages + " is a folder of release v2 still in use by " + link + "; keep it",
		"Owner cleanup: " + recovery + " is a folder of release v2 that setup no longer uses; inspect it, then delete it yourself",
		"Owner cleanup: " + restore + " is a folder of release v2 that setup no longer uses; inspect it, then delete it yourself",
		"Note: the reference check of the v2 folders covers the recorded link targets, the content of the recorded file, block and field targets, and the baselines. Another file can still name a path in these folders.",
	} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("check lacks %q:\n%s", want, check.Message)
		}
	}
	for _, folder := range []string{packages, recovery, restore} {
		if _, err := os.Lstat(folder); err != nil {
			t.Fatalf("setup must never delete %s: %v", folder, err)
		}
	}
}

func TestPreviewStatePlansAStateThatIsNotSaved(t *testing.T) {
	e, _ := fixture(t)
	st := state.State{Schema: 2, Target: e.Target, Selected: []string{"editor"}, Keep: []string{}, System: map[string][]string{}, Targets: map[string]state.Record{}}
	plan, err := e.PreviewState(context.Background(), st, engine.Request{Mode: "apply"})
	if err != nil || operation(t, plan, "tool.editor").Action != "install" {
		t.Fatalf("%v %v", actions(plan), err)
	}
	if _, err := os.Lstat(e.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a preview of a state in memory must write nothing")
	}
}

func TestCheckKeepsAV2FolderThatALauncherInPlaceOfARecordedLinkNames(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	packages := v2Folder(t, e, "packages")
	launcher := filepath.Join(w.home, ".local", "bin", "app")
	if err := os.MkdirAll(filepath.Dir(launcher), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec "+filepath.Join(packages, "tool.app", "current", "bin", "app")+" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(e.StateDir, e.Target)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Update(func(s *state.State) error {
		s.Targets[launcher] = state.Record{Resource: "config.editor", Kind: "link", Applied: "link:abc", Before: "absent"}
		return nil
	})
	if err = errors.Join(err, store.Close()); err != nil {
		t.Fatal(err)
	}
	check, err := e.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := "Note: " + packages + " is a folder of release v2 still in use by " + launcher + "; keep it"; !strings.Contains(check.Message, want) || strings.Contains(check.Message, "Owner cleanup: "+packages) {
		t.Fatalf("check must keep the folder that the launcher names:\n%s", check.Message)
	}
}

func TestCheckKeepsAV2FolderThatARecordedRelativeLinkResolvesInto(t *testing.T) {
	e, w := fixture(t)
	mustApply(t, e, selection("editor"))
	packages := v2Folder(t, e, "packages")
	payload := filepath.Join(packages, "tool.x", "current", "bin", "x")
	link := filepath.Join(w.home, ".local", "bin", "x")
	if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(filepath.Dir(link), payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relative, link); err != nil {
		t.Skip("links are not available:", err)
	}
	store, err := state.Open(e.StateDir, e.Target)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Update(func(s *state.State) error {
		s.Targets[link] = state.Record{Resource: "config.editor", Kind: "link", Applied: "link:abc", Before: "absent"}
		return nil
	})
	if err = errors.Join(err, store.Close()); err != nil {
		t.Fatal(err)
	}
	check, err := e.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := "Note: " + packages + " is a folder of release v2 still in use by " + link + "; keep it"; !strings.Contains(check.Message, want) || strings.Contains(check.Message, "Owner cleanup: "+packages) {
		t.Fatalf("check must keep the folder that the relative link %s resolves into:\n%s", relative, check.Message)
	}
}
