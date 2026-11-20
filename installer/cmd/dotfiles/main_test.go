package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

func TestParseArguments(t *testing.T) {
	for _, scenario := range []struct {
		name string
		args []string
		want command
	}{
		{"menu", nil, command{}},
		{"plan", []string{"plan"}, command{name: "plan"}},
		{"plan with a selection", []string{"plan", "--select=neovim,zsh"}, command{name: "plan", selected: []string{"neovim", "zsh"}, selectSet: true}},
		{"plan with each list", []string{"plan", "--replace=config.nvim", "--remove-shared=tool.git", "--select=zsh"},
			command{name: "plan", selected: []string{"zsh"}, selectSet: true, removeShared: []string{"tool.git"}, replace: []string{"config.nvim"}}},
		{"apply preview", []string{"apply"}, command{name: "apply"}},
		{"apply", []string{"apply", "--yes"}, command{name: "apply", yes: true}},
		{"apply with an empty selection", []string{"apply", "--yes", "--select="}, command{name: "apply", yes: true, selected: []string{}, selectSet: true}},
		{"apply with each flag", []string{"apply", "--select=zsh", "--remove-shared=tool.git,tool.node", "--replace=config.zsh", "--yes"},
			command{name: "apply", yes: true, selected: []string{"zsh"}, selectSet: true, removeShared: []string{"tool.git", "tool.node"}, replace: []string{"config.zsh"}}},
		{"update preview", []string{"update"}, command{name: "update"}},
		{"update", []string{"update", "--yes", "--replace=config.zsh"}, command{name: "update", yes: true, replace: []string{"config.zsh"}}},
		{"check", []string{"check"}, command{name: "check"}},
		{"restore preview", []string{"restore-v2"}, command{name: "restore-v2"}},
		{"restore", []string{"restore-v2", "--yes"}, command{name: "restore-v2", yes: true}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			got, err := parseArguments(scenario.args)
			if err != nil || !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("got %+v, %v; want %+v", got, err, scenario.want)
			}
		})
	}
	for _, args := range [][]string{
		{"machine"}, {"internal-native-worker", "state"}, {"--yes"}, {"--select=zsh"}, {"install"}, {""},
		{"plan", "--yes"}, {"plan", "apply"}, {"plan", "--select"}, {"plan", "--select", "zsh"},
		{"apply", "--yes=true"}, {"apply", "--yes", "--yes"}, {"apply", "--select=zsh", "--select=git"}, {"apply", "--all"},
		{"apply", "--select=zsh,,git"}, {"apply", "--select=zsh, git"}, {"apply", "--select=,"}, {"apply", "--remove-shared="}, {"apply", "--replace="},
		{"apply", "--remove-shared"}, {"apply", "zsh"},
		{"update", "--select=zsh"}, {"update", "--select="}, {"update", "--remove-shared=tool.git"},
		{"check", "--yes"}, {"check", "--select=zsh"}, {"check", "--replace=config.zsh"},
		{"restore-v2", "--select=zsh"}, {"restore-v2", "--replace=config.zsh"},
	} {
		if got, err := parseArguments(args); err == nil {
			t.Errorf("arguments %q were accepted as %+v", args, got)
		}
	}
}

func TestRequestOfACommand(t *testing.T) {
	plan, _ := parseArguments([]string{"plan", "--select="})
	if got := plan.request(); got.Mode != "apply" || !got.SelectSet || len(got.Selected) != 0 || got.Keep != nil {
		t.Fatalf("plan must preview an apply with an explicit empty selection and the saved kept resources: %+v", got)
	}
	update, _ := parseArguments([]string{"update", "--yes", "--replace=config.zsh"})
	if got := update.request(); got.Mode != "update" || got.SelectSet || !reflect.DeepEqual(got.Replace, []string{"config.zsh"}) {
		t.Fatalf("%+v", got)
	}
	if got := (command{name: "apply"}).request(); got.SelectSet || got.Selected != nil {
		t.Fatalf("apply without --select must use the saved selection: %+v", got)
	}
}

type fixtureFiles struct {
	directory string
	unstable  bool
	inspected int
}

func (d *fixtureFiles) Inspect(_ context.Context, r catalog.Resource, want engine.Want, _ engine.Records) (engine.Finding, error) {
	d.inspected++
	token := ""
	if d.unstable {
		token = fmt.Sprint(d.inspected)
	}
	_, err := os.Stat(filepath.Join(d.directory, r.ID))
	switch {
	case errors.Is(err, os.ErrNotExist) && !want.Present:
		return engine.Finding{Status: engine.Absent, Token: "absent" + token}, nil
	case errors.Is(err, os.ErrNotExist):
		return engine.Finding{Status: engine.Missing, Token: "absent" + token, Commands: []engine.CommandBlock{
			{Title: "Install " + r.Name, Lines: []string{"fixture-install " + r.ID}, By: "setup"}}}, nil
	case err != nil:
		return engine.Finding{}, err
	}
	return engine.Finding{Status: engine.Satisfied, Token: "installed" + token}, nil
}

func (d *fixtureFiles) Apply(_ context.Context, r catalog.Resource, _ engine.Finding, rec engine.Records) error {
	path := filepath.Join(d.directory, r.ID)
	if err := os.WriteFile(path, []byte("installed"), 0600); err != nil {
		return err
	}
	return rec.Update(func(s *state.State) error {
		if err := s.BeginTarget(path, state.Record{Resource: r.ID, Kind: "file", Next: "fp:1", Before: "absent"}); err != nil {
			return err
		}
		return s.Promote(path)
	})
}

func (d *fixtureFiles) Remove(_ context.Context, r catalog.Resource, _ engine.Finding, rec engine.Records) error {
	path := filepath.Join(d.directory, r.ID)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return rec.Update(func(s *state.State) error {
		s.Drop(path)
		return nil
	})
}

type fixture struct {
	engine *engine.Engine
	files  *fixtureFiles
	output *os.File
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	output, err := os.Create(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { output.Close() })
	files := &fixtureFiles{directory: home}
	return &fixture{files: files, output: output, engine: &engine.Engine{
		Catalog: &catalog.Catalog{Resources: []catalog.Resource{
			{ID: "agent", Name: "Coding agent", Category: "AI coding", Capability: true, Requires: []string{"runtime"}},
			{ID: "runtime", Name: "Shared runtime", Action: "runtime"},
		}},
		Router:   func(catalog.Resource) (engine.Provider, error) { return files, nil },
		Context:  platform.Context{OS: "linux", Arch: "amd64"},
		StateDir: filepath.Join(home, "ledger"),
		Target:   "linux/amd64:" + home,
		Source:   "command-fixture",
	}}
}

func (f *fixture) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c, err := parseArguments(args)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.output.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.output.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	runErr := execute(context.Background(), f.engine, c, f.output)
	data, err := os.ReadFile(f.output.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data), runErr
}

func (f *fixture) untouched(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(f.engine.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a preview created the state folder", err)
	}
	if _, err := os.Lstat(filepath.Join(f.files.directory, "runtime")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a preview installed a resource", err)
	}
}

func TestPreviewWithoutYesChangesNothing(t *testing.T) {
	f := newFixture(t)
	for _, args := range [][]string{{"plan", "--select=agent"}, {"apply", "--select=agent"}} {
		output, err := f.run(t, args...)
		if err != nil {
			t.Fatal(args, err)
		}
		f.untouched(t)
		for _, want := range []string{"Add: 1  Change: 0", "INSTALL: Shared runtime (runtime)", "Required by: agent", "Install Shared runtime\nSetup runs:\n    fixture-install runtime\n"} {
			if !strings.Contains(output, want) {
				t.Fatalf("%q preview lacks %q: %q", args, want, output)
			}
		}
		if strings.Contains(output, "==>") || strings.Contains(output, "\x1b") {
			t.Fatalf("%q preview shows a run or a control code: %q", args, output)
		}
		if asked := strings.Contains(output, "Nothing was changed. To apply this plan, run the same command with --yes."); asked != (args[0] == "apply") {
			t.Fatalf("%q preview and the --yes instruction: %q", args, output)
		}
	}
	if output, err := f.run(t, "update"); err != nil || !strings.Contains(output, "No resource changes.") || !strings.Contains(output, "Nothing was changed.") {
		t.Fatal(output, err)
	}
	f.untouched(t)
}

func TestApplyWithYesRunsThePlanAndCheckReportsDrift(t *testing.T) {
	f := newFixture(t)
	output, err := f.run(t, "apply", "--yes", "--select=agent")
	if err != nil {
		t.Fatal(output, err)
	}
	for _, want := range []string{"INSTALL: Shared runtime (runtime)", "==> [1/1] Installing Shared runtime\n", "==> Done: everything selected is ready.", "Full log: "} {
		if !strings.Contains(output, want) {
			t.Fatalf("apply output lacks %q: %q", want, output)
		}
	}
	if strings.Contains(output, "\x1b") {
		t.Fatalf("redirected output has a control code: %q", output)
	}
	st, err := state.Peek(f.engine.StateDir, f.engine.Target)
	if err != nil || !reflect.DeepEqual(st.Selected, []string{"agent"}) || st.Source != "command-fixture" {
		t.Fatal(st, err)
	}
	if output, err := f.run(t, "check"); err != nil || !strings.Contains(output, "==> Done: everything selected is ready.") {
		t.Fatal(output, err)
	}
	if err := os.Remove(filepath.Join(f.files.directory, "runtime")); err != nil {
		t.Fatal(err)
	}
	output, err = f.run(t, "check")
	if code := exitCode(context.Background(), err); code != exitFailed || !strings.Contains(output, "Shared runtime (runtime): required by selection") {
		t.Fatal("check did not fail for drift", code, output, err)
	}
	if _, err := os.Lstat(filepath.Join(f.files.directory, "runtime")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("check repaired the drift", err)
	}
	if output, err = f.run(t, "apply", "--yes", "--select="); err != nil || !strings.Contains(output, "==> [1/1] Releasing the records of Shared runtime") {
		t.Fatal(output, err)
	}
	if st, err := state.Peek(f.engine.StateDir, f.engine.Target); err != nil || len(st.Selected) != 0 || len(st.Targets) != 0 {
		t.Fatal(st, err)
	}
}

func TestExitCodes(t *testing.T) {
	live := context.Background()
	interrupted, cancel := context.WithCancel(live)
	cancel()
	for _, scenario := range []struct {
		name string
		ctx  context.Context
		err  error
		want int
	}{
		{"done", live, finish(engine.Result{Status: "ready"}, nil), exitDone},
		{"cancelled in the menu", live, finish(engine.Result{Status: "cancelled"}, nil), exitDone},
		{"needs action", live, finish(engine.Result{Status: "needs-action"}, nil), exitFailed},
		{"failed", live, finish(engine.Result{Status: "failed"}, nil), exitFailed},
		{"error", live, finish(engine.Result{}, errors.New("inspection failed")), exitFailed},
		{"reported failure", live, finish(engine.Result{Status: "failed"}, &engine.ReportedError{Err: errors.New("1 of 2 steps failed")}), exitFailed},
		{"pending mark", live, finish(engine.Result{Status: "needs-action"}, &engine.ReportedError{Err: fmt.Errorf("%w: tool.zsh:install", engine.ErrPending)}), exitFailed},
		{"provider became available", live, finish(engine.Result{Status: "needs-action"}, &engine.ReportedError{Err: engine.ErrReplan}), exitRunAgain},
		{"machine changed", live, finish(engine.Result{Status: "failed"}, &engine.ReportedError{Err: engine.ErrPlanChanged}), exitRunAgain},
		{"interrupt", interrupted, finish(engine.Result{Status: "failed"}, &engine.ReportedError{Err: context.Canceled}), exitInterrupt},
		{"cancellation without an interrupt", live, context.Canceled, exitFailed},
	} {
		if got := exitCode(scenario.ctx, scenario.err); got != scenario.want {
			t.Errorf("%s: exit %d, want %d (%v)", scenario.name, got, scenario.want, scenario.err)
		}
	}
	for _, err := range []error{engine.ErrReplan, engine.ErrPlanChanged} {
		if message := finish(engine.Result{}, err).Error(); !strings.Contains(message, "run the same command again") {
			t.Errorf("exit 3 does not tell the operator to run the command again: %q", message)
		}
	}
}

func TestAChangeAfterThePreviewStopsWithRunAgain(t *testing.T) {
	f := newFixture(t)
	f.files.unstable = true
	output, err := f.run(t, "apply", "--yes", "--select=agent")
	if code := exitCode(context.Background(), err); code != exitRunAgain || !errors.Is(err, engine.ErrPlanChanged) {
		t.Fatal(code, err, output)
	}
	if !strings.Contains(output, "Shared runtime changed after approval") {
		t.Fatalf("the report does not name the change: %q", output)
	}
	if _, err := os.Lstat(filepath.Join(f.files.directory, "runtime")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("setup applied a plan that the machine no longer matches", err)
	}
}

func TestEndOfInputNeverApproves(t *testing.T) {
	f := newFixture(t)
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := menu(context.Background(), f.engine, input, f.output); err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatal("the menu accepted input that is not a terminal", err)
	}
	f.untouched(t)
	for _, args := range [][]string{{"apply", "--select=agent"}, {"update"}} {
		if _, err := f.run(t, args...); err != nil {
			t.Fatal(err)
		}
		f.untouched(t)
	}
}

func TestEntrypointChecksComeBeforeDiscovery(t *testing.T) {
	t.Setenv("DOTFILES_ENTRYPOINT", "migrate")
	t.Setenv("DOTFILES_CHECKOUT", t.TempDir())
	if err := run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "unknown installer entrypoint") {
		t.Fatal(err)
	}
	t.Setenv("DOTFILES_ENTRYPOINT", "setup")
	t.Setenv("DOTFILES_CHECKOUT", "")
	if err := run(context.Background(), []string{"check"}); err == nil || !strings.Contains(err.Error(), "DOTFILES_CHECKOUT is missing") {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"machine"}); err == nil || !strings.Contains(err.Error(), `unknown command "machine"`) {
		t.Fatal(err)
	}
}

func TestUnconnectedWiringFailsWithItsName(t *testing.T) {
	if _, err := newRouter(setup{})(catalog.Resource{ID: "tool.rg"}); err == nil || !strings.Contains(err.Error(), "provider wiring is not connected") {
		t.Fatal(err)
	}
	if err := importV2(context.Background(), setup{}); err == nil || !strings.Contains(err.Error(), "import") {
		t.Fatal(err)
	}
	if err := restoreV2(context.Background(), setup{}); err == nil {
		t.Fatal("restore-v2 reported a result without its implementation")
	}
}
