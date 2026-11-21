package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/state"
)

func importFixture(t *testing.T) (setup, []byte) {
	t.Helper()
	s := wiredSetup(t, "linux", "amd64")
	if err := os.MkdirAll(s.stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	home, err := json.Marshal(s.folders.Home)
	if err != nil {
		t.Fatal(err)
	}
	original := []byte("{\n  \"schema\": 1,\n  \"context\": {\n    \"os\": \"linux\",\n    \"arch\": \"amd64\"\n  },\n  \"target\": " + string(home) +
		",\n  \"source\": \"v2\",\n  \"generation\": 3,\n  \"selected\": [],\n  \"keep\": [],\n  \"receipts\": {}\n}\n")
	if err := os.WriteFile(filepath.Join(s.stateDir, state.File), original, 0600); err != nil {
		t.Fatal(err)
	}
	return s, original
}

func runV2(t *testing.T, s setup, run func(context.Context, setup) error, args ...string) (string, error) {
	t.Helper()
	c, err := parseArguments(args)
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	s.command, s.output = c, output
	runErr := run(context.Background(), s)
	data, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data), runErr
}

func TestImportAndRestoreCommandsOfASchema1State(t *testing.T) {
	s, original := importFixture(t)
	file := filepath.Join(s.stateDir, state.File)
	unchanged := func(step string) {
		t.Helper()
		if data, err := os.ReadFile(file); err != nil || string(data) != string(original) {
			t.Fatalf("%s changed the state of release v2: %v", step, err)
		}
	}
	for _, args := range [][]string{{"plan"}, {"apply"}, {"update"}} {
		text, err := runV2(t, s, importV2, args...)
		if err != nil || !strings.Contains(text, "Import from release v2 (fb15ce5, v2.3.0) into release v3.") {
			t.Fatalf("%v: %q %v", args, text, err)
		}
		if (args[0] != "plan") != strings.Contains(text, "Nothing was changed. To import this installation, run the same command with --yes.") {
			t.Fatalf("%v: %q", args, text)
		}
		unchanged(args[0])
	}
	if text, err := runV2(t, s, importV2, "check"); err == nil || !strings.Contains(err.Error(), "runs release v2") || !strings.Contains(text, "Import from release v2") {
		t.Fatalf("check of a schema-1 state: %q %v", text, err)
	}
	if _, err := runV2(t, s, importV2, "apply", "--yes", "--select=zsh"); err == nil || !strings.Contains(err.Error(), "keeps the selection of release v2") {
		t.Fatal(err)
	}
	if _, err := runV2(t, s, restoreV2, "restore-v2", "--yes"); err == nil || !strings.Contains(err.Error(), "nothing to restore") {
		t.Fatal(err)
	}
	unchanged("a refused command")
	text, err := runV2(t, s, importV2, "apply", "--yes")
	if err != nil || !strings.Contains(text, "The import from release v2 is complete.") {
		t.Fatalf("%q %v", text, err)
	}
	st, err := state.Peek(s.stateDir, s.target)
	if err != nil || st.Schema != 2 || st.V2Restore != "open" {
		t.Fatalf("%+v %v", st, err)
	}
	text, err = runV2(t, s, restoreV2, "restore-v2")
	if err != nil || !strings.Contains(text, "Nothing was changed. To restore release v2, run restore-v2 --yes.") {
		t.Fatalf("%q %v", text, err)
	}
	if st, err := state.Peek(s.stateDir, s.target); err != nil || st.V2Restore != "open" {
		t.Fatal("a restore preview changed the state")
	}
	text, err = runV2(t, s, restoreV2, "restore-v2", "--yes")
	if err != nil || !strings.Contains(text, "Check out fb15ce5 (v2.3.0) and run its setup.") {
		t.Fatalf("%q %v", text, err)
	}
	unchanged("restore-v2 must give back the exact state, and it")
	if _, err := state.Peek(s.stateDir, s.target); !errors.Is(err, state.ErrSchema1) {
		t.Fatal(err)
	}
}

func TestWiringKeepsTheLegacyCommandFoldersOfDeferredMacApplications(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses a symbolic link")
	}
	s := wiredSetup(t, "darwin", "arm64")
	if got := newWiring(s).profiles.LegacyCommandDirectories; len(got) != 0 {
		t.Fatalf("a fresh installation has no legacy folder: %v", got)
	}
	payload := filepath.Join(s.stateDir, "packages", "tool.ghostty", "current", "Ghostty.app")
	application := filepath.Join(s.folders.Home, "Applications", "Ghostty.app")
	for _, folder := range []string{payload, filepath.Dir(application)} {
		if err := os.MkdirAll(folder, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(payload, application); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(s.stateDir, s.target)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Update(func(st *state.State) error {
		st.Targets[application] = state.Record{Resource: "integration.ghostty", Kind: "link", Applied: "link:abc", Before: "absent"}
		return nil
	})
	if err = errors.Join(err, store.Close()); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(s.stateDir, "packages", "tool.ghostty", "current", "Ghostty.app", "Contents", "MacOS")}
	if got := newWiring(s).profiles.LegacyCommandDirectories; !slices.Equal(got, want) {
		t.Fatalf("legacy folders: %v, want %v", got, want)
	}
	if err := os.Remove(application); err != nil {
		t.Fatal(err)
	}
	if got := newWiring(s).profiles.LegacyCommandDirectories; len(got) != 0 {
		t.Fatalf("after the release of the legacy link: %v", got)
	}
}

type restoreScreen struct {
	proposals []engine.Proposal
	check     []string
	decline   bool
	back      error
	closed    bool
	answers   [][]string
	asked     []string
}

func (r *restoreScreen) Choose(_ context.Context, title string, _ []engine.Choice, _ []string, _ bool) ([]string, error) {
	r.asked = append(r.asked, title)
	if len(r.answers) == 0 {
		return nil, engine.ErrCancelled
	}
	answer := r.answers[0]
	r.answers = r.answers[1:]
	return answer, nil
}

func (r *restoreScreen) BeginApply(ctx context.Context) (context.Context, error) { return ctx, nil }

func (r *restoreScreen) Report(engine.Result) error { return nil }

func (r *restoreScreen) Review(ctx context.Context, _ engine.Catalog, proposal engine.Proposal, revise engine.Revise) (bool, error) {
	r.proposals = append(r.proposals, proposal)
	choices := slices.Clone(proposal.Choices)
	for i := range choices {
		choices[i].Checked = slices.Contains(r.check, choices[i].ID)
	}
	if r.decline || r.back != nil {
		return false, r.back
	}
	if revise == nil {
		return true, nil
	}
	revised, err := revise(ctx, choices)
	r.proposals = append(r.proposals, revised)
	return true, err
}

func (r *restoreScreen) Close() error { r.closed = true; return nil }

func TestTheRestoreMenuUsesTheReviewScreenWithTheFullListAndTheReplacementChoices(t *testing.T) {
	s, original := importFixture(t)
	if _, err := runV2(t, s, importV2, "apply", "--yes"); err != nil {
		t.Fatal(err)
	}
	edited, kept := filepath.Join(s.folders.Home, "edited.conf"), filepath.Join(s.folders.Home, "kept.conf")
	manifestPath := filepath.Join(s.stateDir, "v2-restore", "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	slots := map[string]any{}
	for _, path := range []string{edited, kept} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("my content of "+filepath.Base(path)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		slots[path] = map[string]any{"resource": "config.fixture", "kind": "file", "saved": "absent", "fingerprint": "absent"}
	}
	manifest["slots"] = slots
	if data, err = json.Marshal(manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	options, _, err := importOptions(s)
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	s.output = output
	screen := &restoreScreen{check: []string{edited}}
	if err := restoreMenu(context.Background(), screen, s, options); err != nil {
		t.Fatal(err)
	}
	if len(screen.proposals) != 2 || !screen.closed {
		t.Fatalf("the restore approval must use the review screen: %+v", screen)
	}
	first, revised := screen.proposals[0], screen.proposals[1]
	for _, path := range []string{edited, kept} {
		if !strings.Contains(first.Details, "changed  "+path) {
			t.Fatalf("the details of the review must hold each line of the preview:\n%s", first.Details)
		}
	}
	if len(first.Choices) != 2 || first.Choices[0].Group != restoreReplaceGroup || first.Choices[0].Checked || first.Choices[1].Checked || !strings.Contains(first.Notice, "2 saved targets") {
		t.Fatalf("the review must offer each changed target, unchecked: %+v", first)
	}
	count := func(proposal engine.Proposal) map[string]int {
		counts := map[string]int{}
		for _, op := range proposal.Plan.Operations {
			counts[op.Action]++
		}
		return counts
	}
	if got := count(first); len(first.Plan.Operations) != 2 || got["pending"] != 2 {
		t.Fatalf("the review must count each saved target; two kept targets need action: %v", got)
	}
	if got := count(revised); len(revised.Plan.Operations) != 2 || got["remove"] != 1 || got["pending"] != 1 {
		t.Fatalf("an approved replacement must count as the removal that it does: %v", got)
	}
	if !strings.Contains(revised.Plan.Operations[0].Reason, edited) {
		t.Fatalf("each operation must name its target: %+v", revised.Plan.Operations[0])
	}
	if !revised.Choices[0].Checked || revised.Choices[1].Checked {
		t.Fatalf("a toggle must give the revised choices: %+v", revised.Choices)
	}
	if _, err := os.Lstat(edited); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the chosen target must get its value of release v2, which is absence")
	}
	if data, err := os.ReadFile(kept); err != nil || string(data) != "my content of kept.conf\n" {
		t.Fatalf("a target without a choice must stay: %q %v", data, err)
	}
	saved := ""
	entries, err := os.ReadDir(filepath.Join(s.stateDir, "v2-restore", "edited"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(s.stateDir, "v2-restore", "edited", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		saved += string(content)
	}
	if saved != "my content of edited.conf\n" {
		t.Fatalf("setup must save the replaced content first: %q", saved)
	}
	if data, err := os.ReadFile(filepath.Join(s.stateDir, state.File)); err != nil || string(data) != string(original) {
		t.Fatalf("the restore must publish the state of release v2: %v", err)
	}
}

func TestADeclinedRestoreReviewGoesBackToTheMenuAndChangesNothing(t *testing.T) {
	s, _ := importFixture(t)
	if _, err := runV2(t, s, importV2, "apply", "--yes"); err != nil {
		t.Fatal(err)
	}
	options, _, err := importOptions(s)
	if err != nil {
		t.Fatal(err)
	}
	for name, screen := range map[string]*restoreScreen{"declined": {decline: true}, "back": {back: engine.ErrBack}} {
		if err := restoreMenu(context.Background(), screen, s, options); !errors.Is(err, engine.ErrBack) {
			t.Fatalf("%s: a restore that did not run must go back to the menu: %v", name, err)
		}
		if st, err := state.Peek(s.stateDir, s.target); err != nil || st.V2Restore != "open" || screen.closed {
			t.Fatalf("%s: a declined restore must change nothing: %+v %v", name, st, err)
		}
	}
	if err := restoreMenu(context.Background(), &restoreScreen{back: engine.ErrCancelled}, s, options); !errors.Is(err, engine.ErrCancelled) || errors.Is(err, engine.ErrBack) {
		t.Fatalf("a quit must stay a cancellation: %v", err)
	}
	output, err := os.Create(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	s.output = output
	if err := restoreMenu(context.Background(), &restoreScreen{}, s, options); err != nil {
		t.Fatalf("a completed restore returns no error: %v", err)
	}
	if _, err := state.Peek(s.stateDir, s.target); !errors.Is(err, state.ErrSchema1) {
		t.Fatal(err)
	}
}

func editV2State(t *testing.T, s setup, edit func(*state.State)) []byte {
	t.Helper()
	store, err := state.Open(s.stateDir, s.target)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Update(func(st *state.State) error { edit(st); return nil })
	if err = errors.Join(err, store.Close()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.stateDir, state.File))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func reportPart(t *testing.T, text string) string {
	t.Helper()
	at := strings.Index(text, "\n==> ")
	if at < 0 {
		t.Fatalf("the output has no report:\n%s", text)
	}
	return text[at:]
}

var fontMark = hooks.FontResource + ":activate"

func TestRestoreAndTheImportCommandsStopAtAPendingMarkLikeApply(t *testing.T) {
	s, _ := importFixture(t)
	if _, err := runV2(t, s, importV2, "apply", "--yes"); err != nil {
		t.Fatal(err)
	}
	marked := editV2State(t, s, func(st *state.State) { st.Mark(fontMark) })
	unchanged := func(step string, want []byte) {
		t.Helper()
		if data, err := os.ReadFile(filepath.Join(s.stateDir, state.File)); err != nil || string(data) != string(want) {
			t.Fatalf("%s changed the state at a pending mark:\n%s", step, data)
		}
	}
	normal, normalErr := runV2(t, s, func(ctx context.Context, s setup) error { return execute(ctx, newEngine(s), s.command, s.output) }, "apply", "--yes")
	if normalErr == nil || !strings.Contains(normal, "An earlier step did not report its end: "+fontMark+". Open setup, confirm that its command exited, then choose the action again.") {
		t.Fatalf("the fixture must stop a normal apply at the mark: %q %v", normal, normalErr)
	}
	unchanged("apply", marked)
	text, err := runV2(t, s, restoreV2, "restore-v2", "--yes")
	if err == nil || err.Error() != normalErr.Error() || exitCode(context.Background(), err) != exitCode(context.Background(), normalErr) || reportPart(t, text) != reportPart(t, normal) {
		t.Fatalf("restore-v2 --yes must stop like apply at a mark:\n%s\n%v\nwant:\n%s\n%v", text, err, normal, normalErr)
	}
	unchanged("restore-v2 --yes", marked)
	switching := editV2State(t, s, func(st *state.State) { st.V2Restore = "switching" })
	text, err = runV2(t, s, importV2, "apply", "--yes")
	if err == nil || err.Error() != normalErr.Error() || exitCode(context.Background(), err) != exitCode(context.Background(), normalErr) || reportPart(t, text) != reportPart(t, normal) {
		t.Fatalf("apply --yes during an import must stop like apply at a mark:\n%s\n%v\nwant:\n%s\n%v", text, err, normal, normalErr)
	}
	unchanged("apply --yes during an import", switching)
}

func pendingQuestion(mark string) string {
	return "An earlier step did not report its end: " + mark + ". Did its command exit?"
}

func TestTheRestoreMenuAsksForEachPendingMarkBeforeTheRestore(t *testing.T) {
	s, original := importFixture(t)
	if _, err := runV2(t, s, importV2, "apply", "--yes"); err != nil {
		t.Fatal(err)
	}
	marked := editV2State(t, s, func(st *state.State) { st.Mark(fontMark); st.Mark("tool.zsh:install") })
	options, _, err := importOptions(s)
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	s.output = output
	screen := &restoreScreen{answers: [][]string{{"confirm"}, {"back"}}}
	if err := restoreMenu(context.Background(), screen, s, options); !errors.Is(err, engine.ErrBack) {
		t.Fatalf("a mark without a confirmation must go back: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(s.stateDir, state.File)); err != nil || string(data) != string(marked) || len(screen.proposals) != 0 {
		t.Fatalf("a restore without the confirmation of each mark must change nothing and show no review: %v", err)
	}
	screen = &restoreScreen{answers: [][]string{{"confirm"}, {"confirm"}}}
	if err := restoreMenu(context.Background(), screen, s, options); err != nil {
		t.Fatal(err)
	}
	if want := []string{pendingQuestion(fontMark), pendingQuestion("tool.zsh:install")}; !slices.Equal(screen.asked, want) {
		t.Fatalf("the menu must ask once for each mark with the words of the normal menu: %q", screen.asked)
	}
	if data, err := os.ReadFile(filepath.Join(s.stateDir, state.File)); err != nil || string(data) != string(original) {
		t.Fatalf("after the confirmations the restore must publish the state of release v2: %v", err)
	}
}

func TestTheImportMenuAsksForEachPendingMarkBeforeTheSwitch(t *testing.T) {
	s, _ := importFixture(t)
	if _, err := runV2(t, s, importV2, "apply", "--yes"); err != nil {
		t.Fatal(err)
	}
	marked := editV2State(t, s, func(st *state.State) { st.Mark(fontMark); st.V2Restore = "switching" })
	options, setLegacy, err := importOptions(s)
	if err != nil {
		t.Fatal(err)
	}
	const choice = "The import from release v2 is not complete. What would you like to do?"
	screen := &restoreScreen{answers: [][]string{{"complete"}, {"back"}, {"exit"}}}
	if open, err := reviewImport(context.Background(), screen, s, options, setLegacy); err != nil || open {
		t.Fatalf("an exit must end the import menu: %v %v", open, err)
	}
	if want := []string{choice, pendingQuestion(fontMark), choice}; !slices.Equal(screen.asked, want) || len(screen.proposals) != 0 {
		t.Fatalf("the menu must ask for the mark before the review, and go back without a confirmation: %q", screen.asked)
	}
	if data, err := os.ReadFile(filepath.Join(s.stateDir, state.File)); err != nil || string(data) != string(marked) {
		t.Fatalf("a mark without a confirmation must change nothing: %v", err)
	}
	screen = &restoreScreen{answers: [][]string{{"complete"}, {"confirm"}}}
	if open, err := reviewImport(context.Background(), screen, s, options, setLegacy); err != nil || !open {
		t.Fatalf("a confirmed mark must let the switch complete: %v %v", open, err)
	}
	if st, err := state.Peek(s.stateDir, s.target); err != nil || len(st.Pending) != 0 || st.V2Restore != "open" {
		t.Fatalf("the approved switch must clear the confirmed mark and complete: %+v %v", st, err)
	}
}

func TestTheImportCommandsDuringTheSwitchNameEachUnfinishedTarget(t *testing.T) {
	s, _ := importFixture(t)
	if _, err := runV2(t, s, importV2, "apply", "--yes"); err != nil {
		t.Fatal(err)
	}
	first, second := filepath.Join(s.folders.Home, ".zshrc"), filepath.Join(s.folders.Home, ".zshenv")
	editV2State(t, s, func(st *state.State) {
		st.V2Restore = "switching"
		st.Targets[first] = state.Record{Resource: "config.zsh", Kind: "file", Next: "fp:1", Before: "absent"}
		st.Targets[second] = state.Record{Resource: "config.zsh", Kind: "file", Next: "fp:2", Before: "absent"}
	})
	lines := []string{"unfinished change: " + first + " (config.zsh). Run setup again to finish it.", "unfinished change: " + second + " (config.zsh). Run setup again to finish it."}
	for _, args := range [][]string{{"plan"}, {"apply"}, {"check"}} {
		text, err := runV2(t, s, importV2, args...)
		for _, line := range lines {
			if !strings.Contains(text, line) {
				t.Fatalf("%v must name each unfinished target:\n%s\n%v", args, text, err)
			}
		}
		if args[0] == "check" && (err == nil || !strings.Contains(reportPart(t, text), "these items need your action")) {
			t.Fatalf("check must report the unfinished targets as an action:\n%s\n%v", text, err)
		}
	}
}

type actionScreen struct {
	restoreScreen
	offered []string
}

func (a *actionScreen) Choose(_ context.Context, _ string, choices []engine.Choice, _ []string, _ bool) ([]string, error) {
	for _, choice := range choices {
		a.offered = append(a.offered, choice.ID)
	}
	return []string{"exit"}, nil
}

func (a *actionScreen) ChooseTools(context.Context, engine.ToolMenu) ([]string, error) {
	return nil, engine.ErrCancelled
}

func TestTheMenuAfterAnImportOffersTheRestoreOfReleaseV2(t *testing.T) {
	s, _ := importFixture(t)
	if _, err := runV2(t, s, importV2, "apply", "--yes"); err != nil {
		t.Fatal(err)
	}
	e := menuEngine(s)
	screen := &actionScreen{}
	e.UI = screen
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(screen.offered, "restore-v2") {
		t.Fatalf("the menu after an import must offer the restore of release v2: %q", screen.offered)
	}
}
