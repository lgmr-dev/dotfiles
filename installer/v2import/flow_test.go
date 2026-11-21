package v2import_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
	"github.com/luisgui1757/dotfiles/installer/v2import"
)

type machine struct {
	original []byte
	home     string
	shells   string
	v2       map[string]string
	keyboard string
}

var v2Folders = []string{"packages", "nvim-sync", "recovery", "sentinel"}

func capture(t *testing.T, f *fx, original []byte) machine {
	t.Helper()
	m := machine{original: original, home: describe(t, f.home), shells: describe(t, filepath.Join(f.state, "shells")), v2: map[string]string{}, keyboard: describe(t, filepath.Dir(f.keyboard))}
	for _, name := range v2Folders {
		m.v2[name] = describe(t, filepath.Join(f.state, name))
	}
	return m
}

func (m machine) folders(t *testing.T, f *fx) {
	t.Helper()
	for _, name := range v2Folders {
		if got := describe(t, filepath.Join(f.state, name)); got != m.v2[name] {
			t.Fatalf("setup wrote under the folder %s of release v2:\n%s\nwas:\n%s", name, got, m.v2[name])
		}
	}
	if got := describe(t, filepath.Dir(f.keyboard)); got != m.keyboard {
		t.Fatal("setup changed the keyboard rule")
	}
}

func (m machine) schema1(t *testing.T, f *fx) {
	t.Helper()
	if got := read(t, filepath.Join(f.state, "state.json")); got != string(m.original) {
		t.Fatalf("the state is not the exact schema-1 state of release v2:\n%s", got)
	}
}

func (m machine) restored(t *testing.T, f *fx) {
	t.Helper()
	m.schema1(t, f)
	m.folders(t, f)
	if got := describe(t, f.home); got != m.home {
		t.Fatalf("the home folder differs from release v2:\n%s\nwas:\n%s", got, m.home)
	}
	if got := describe(t, filepath.Join(f.state, "shells")); got != m.shells {
		t.Fatalf("the shell init files differ from release v2:\n%s\nwas:\n%s", got, m.shells)
	}
}

func switched(t *testing.T, f *fx) {
	t.Helper()
	st := f.current()
	if st.Schema != 2 || st.V2Restore != "open" {
		t.Fatalf("after the import the state must be schema 2 with an open fallback: %+v", st)
	}
	if got := read(t, filepath.Join(f.state, "shells", "env.sh")); got != "# v3 env\n" {
		t.Fatalf("env.sh: %q", got)
	}
	zshrc := read(t, filepath.Join(f.home, ".zshrc"))
	for _, part := range []string{"# personal zshrc\nexport EDITOR=vi\n", "alias personal='after the block'\n", "# >>> dotfiles:integration.shells >>>\n. v3-init\n# <<< dotfiles:integration.shells <<<\n"} {
		if !strings.Contains(zshrc, part) {
			t.Fatalf(".zshrc lost %q:\n%s", part, zshrc)
		}
	}
	if strings.Contains(zshrc, "v2-init") {
		t.Fatalf(".zshrc keeps the block of release v2:\n%s", zshrc)
	}
	if got := read(t, filepath.Join(f.home, ".zprofile")); !strings.Contains(got, ". v3-env") {
		t.Fatalf(".zprofile: %q", got)
	}
	if info, err := os.Lstat(f.fontPath()); err != nil || !info.Mode().IsRegular() || read(t, f.fontPath()) != "v3 font bytes\n" {
		t.Fatalf("the font must be the copy of release v3: %v %v", info, err)
	}
	runtime, err := os.Readlink(f.publicRuntime())
	if err != nil || !strings.HasPrefix(runtime, filepath.Join(f.state, "tools", hooks.NvimResource)) {
		t.Fatalf("the public runtime link must point at the new runtime: %q %v", runtime, err)
	}
	if got := read(t, filepath.Join(f.home, ".tmux.plugins.conf")); !strings.Contains(got, "run-shell mine") {
		t.Fatalf("the changed tmux block must stay: %q", got)
	}
	for _, id := range []string{"tool.nvim", "tool.lazygit", hooks.FontArchive} {
		if read(t, filepath.Join(f.state, "tools", id, "current", "bin", "tool")) != "v3 "+id+"\n" {
			t.Fatalf("%s has no build under tools", id)
		}
	}
}

func mustRestore(t *testing.T, f *fx, replace ...string) (*v2import.Restoration, string) {
	t.Helper()
	restoration, err := v2import.PreviewRestore(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	message, err := restoration.Run(context.Background(), replace)
	if err != nil {
		t.Fatal(err)
	}
	return restoration, message
}

func action(plan engine.Plan, id string) string {
	for _, op := range plan.Operations {
		if op.Resource == id {
			return op.Action
		}
	}
	return ""
}

func TestImportSwitchesThroughTheEngineAndRestoreGivesReleaseV2Back(t *testing.T) {
	f := linuxFixture(t)
	before := capture(t, f, f.save())
	everything := describe(t, f.root)
	proposal, err := f.preview()
	if err != nil {
		t.Fatal(err)
	}
	if describe(t, f.root) != everything {
		t.Fatal("the preview wrote to the machine")
	}
	want := map[string]string{"tool.nvim": "install", "tool.lazygit": "install", hooks.FontArchive: "install", hooks.NvimResource: "install", shells: "repair",
		hooks.FontResource: "repair", plugins: "pending", "config.nvim": "keep", "config.tmux": "keep", "tool.zsh": "keep", "system.keyboard-access": "keep", vscode: "keep"}
	for id, expected := range want {
		if got := action(proposal.Plan, id); got != expected {
			t.Fatalf("import plan: %s is %q, want %q", id, got, expected)
		}
	}
	for _, op := range proposal.Plan.Operations {
		if op.Action == "remove" || op.Action == "forget" {
			t.Fatalf("the import must not remove %s", op.Resource)
		}
	}
	for _, part := range []string{"fb15ce5", "Neovim program (tool.nvim)", "Shell profiles (" + shells + "): repair", filepath.Join(f.state, "packages"), filepath.Join(f.state, "recovery"),
		"Records of release v2:", "retired Sentinel record: dropped", filepath.Join(f.state, "v2-restore")} {
		if !strings.Contains(proposal.Details, part) {
			t.Fatalf("the review lacks %q:\n%s", part, proposal.Details)
		}
	}
	result, err := proposal.Run(context.Background())
	if err != nil {
		t.Fatalf("%v\n%s", err, result.Message)
	}
	if !strings.Contains(result.Message, "The import from release v2 is complete") {
		t.Fatalf("result: %s", result.Message)
	}
	switched(t, f)
	before.folders(t, f)
	if read(t, filepath.Join(f.state, "v2-restore", "state.json")) != string(before.original) {
		t.Fatal("the restore set must hold the exact schema-1 state")
	}
	st := f.current()
	if !slices.Equal(st.Selected, []string{"fonts", "keyboard", "neovim", "sentinel", "shell", "tmux", "vscode"}) || !slices.Equal(st.Keep, []string{"tool.lazygit"}) {
		t.Fatalf("selection after the import: %v %v", st.Selected, st.Keep)
	}
	if read(t, filepath.Join(f.state, "sentinel", "record.json")) != "{\"schema\":1,\"release\":\"v0.4.0\"}\n" {
		t.Fatal("the Sentinel record changed")
	}

	restoration, err := v2import.PreviewRestore(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, item := range restoration.Items {
		actions[item.Key] = item.Action
	}
	expected := map[string]string{
		filepath.Join(f.state, "shells", "env.sh"):                    "restore",
		filepath.Join(f.state, "shells", "init.sh"):                   "restore",
		targets.Key(filepath.Join(f.home, ".zshrc"), shells):          "restore",
		targets.Key(filepath.Join(f.home, ".bashrc"), shells):         "restore",
		targets.Key(filepath.Join(f.home, ".zprofile"), shells):       "remove",
		filepath.Join(f.home, ".config", "v3-added", "settings.conf"): "remove",
		f.fontPath():      "restore",
		f.publicRuntime(): "restore",
	}
	for key, want := range expected {
		if actions[key] != want {
			t.Fatalf("restore plan: %s is %q, want %q (%v)", key, actions[key], want, actions)
		}
	}
	if len(actions) != len(expected) || len(restoration.Changed()) != 0 {
		t.Fatalf("restore plan has other items: %v", actions)
	}
	if !strings.Contains(restoration.Text(), "Then setup publishes the saved schema-1 state of release v2.") {
		t.Fatal(restoration.Text())
	}
	message, err := restoration.Run(context.Background(), nil)
	if err != nil || !strings.Contains(message, "Check out fb15ce5 (v2.3.0) and run its setup.") {
		t.Fatalf("%q %v", message, err)
	}
	before.restored(t, f)
	if _, again := mustRestore(t, f); !strings.Contains(again, "Release v2 is active again") {
		t.Fatalf("a second restore must report that nothing is left: %q", again)
	}
	before.restored(t, f)

	f.mustImport()
	switched(t, f)
	mustRestore(t, f)
	before.restored(t, f)
}

func TestAPersonalEditAfterTheSwitchSurvivesRestore(t *testing.T) {
	for _, replace := range []bool{false, true} {
		f := linuxFixture(t)
		before := capture(t, f, f.save())
		f.mustImport()
		zshrc, env := filepath.Join(f.home, ".zshrc"), filepath.Join(f.state, "shells", "env.sh")
		write(t, zshrc, strings.Replace(read(t, zshrc), ". v3-init", ". v3-init\n# my edit in the block", 1)+"export ADDED_AFTER_THE_SWITCH=1\n", 0644)
		write(t, env, "# v3 env\n# my edit of a generated file\n", 0644)
		restoration, err := v2import.PreviewRestore(context.Background(), f.options())
		if err != nil {
			t.Fatal(err)
		}
		changed := []string{}
		for _, item := range restoration.Changed() {
			changed = append(changed, item.Key)
		}
		slices.Sort(changed)
		if !slices.Equal(changed, []string{targets.Key(zshrc, shells), env}) {
			t.Fatalf("changed targets: %v", changed)
		}
		chosen := []string{}
		if replace {
			chosen = changed
		}
		message, err := restoration.Run(context.Background(), chosen)
		if err != nil {
			t.Fatal(err)
		}
		before.schema1(t, f)
		before.folders(t, f)
		text := read(t, zshrc)
		if !strings.Contains(text, "export ADDED_AFTER_THE_SWITCH=1\n") || !strings.Contains(text, "# personal zshrc\nexport EDITOR=vi\n") {
			t.Fatalf("restore lost personal content outside the block:\n%s", text)
		}
		if _, err := os.Lstat(filepath.Join(f.home, ".zprofile")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("restore must remove the target that the switch created")
		}
		if info, err := os.Lstat(f.fontPath()); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("restore must give back the unchanged targets")
		}
		if !replace {
			if !strings.Contains(text, "# my edit in the block") || read(t, env) != "# v3 env\n# my edit of a generated file\n" || strings.Count(message, "Kept your changed content") != 2 {
				t.Fatalf("without a decision restore must keep and report the edits: %q\n%s", message, text)
			}
			continue
		}
		if strings.Contains(text, "# my edit in the block") || !strings.Contains(text, ". v2-init") || !strings.Contains(read(t, env), "export PATH=v2") {
			t.Fatalf("an approved replacement must give back the value of release v2:\n%s", text)
		}
		saved := ""
		entries, err := os.ReadDir(filepath.Join(f.state, "v2-restore", "edited"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			saved += read(t, filepath.Join(f.state, "v2-restore", "edited", entry.Name()))
		}
		if !strings.Contains(saved, "# my edit in the block") || !strings.Contains(saved, "# my edit of a generated file") {
			t.Fatalf("restore must save the edited content before it replaces it: %q", saved)
		}
	}
}

func TestImportStopsBeforeTheSwitchAndLeavesSchema1(t *testing.T) {
	t.Run("build failure", func(t *testing.T) {
		f := linuxFixture(t)
		before := capture(t, f, f.save())
		f.failBuild[hooks.NvimResource] = true
		proposal, err := f.preview()
		if err != nil {
			t.Fatal(err)
		}
		_, err = proposal.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "release v2 stays active") || !strings.Contains(err.Error(), "Neovim runtime ("+hooks.NvimResource+") did not build") {
			t.Fatalf("want a stop before the switch, got %v", err)
		}
		before.restored(t, f)
		if read(t, filepath.Join(f.state, "tools", "tool.nvim", "current", "bin", "tool")) != "v3 tool.nvim\n" {
			t.Fatal("the earlier builds must stay for the next run")
		}
		f.failBuild[hooks.NvimResource] = false
		f.mustImport()
		switched(t, f)
		mustRestore(t, f)
		before.restored(t, f)
	})
	t.Run("system item needs work", func(t *testing.T) {
		f := linuxFixture(t)
		before := capture(t, f, f.save())
		everything := describe(t, f.root)
		f.present["tool.zsh"] = false
		proposal, err := f.preview()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(proposal.Details, "Items that need work first. The import stops before the switch") || !strings.Contains(proposal.Details, "Zsh (tool.zsh): required by selection: the system item is not installed") {
			t.Fatalf("the review must name the item that stops the import:\n%s", proposal.Details)
		}
		_, err = proposal.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "Zsh (tool.zsh)") || !strings.Contains(err.Error(), "fb15ce5") {
			t.Fatalf("want the named system item, got %v", err)
		}
		before.restored(t, f)
		if describe(t, f.root) != everything {
			t.Fatal("a stop for a system item must change nothing")
		}
	})
	t.Run("blocked target", func(t *testing.T) {
		f := linuxFixture(t)
		before := capture(t, f, f.save())
		everything := describe(t, f.root)
		if err := os.Chmod(filepath.Join(f.home, ".zshrc"), 0); err != nil {
			t.Fatal(err)
		}
		if _, err := os.ReadFile(filepath.Join(f.home, ".zshrc")); err == nil {
			t.Skip("this account reads a file without permissions")
		}
		proposal, err := f.preview()
		if err != nil {
			t.Fatal(err)
		}
		_, err = proposal.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "Shell profiles ("+shells+")") || !strings.Contains(err.Error(), "release v2 stays active") {
			t.Fatalf("a target that setup cannot inspect must stop the import before the switch: %v", err)
		}
		if err := os.Chmod(filepath.Join(f.home, ".zshrc"), 0644); err != nil {
			t.Fatal(err)
		}
		before.restored(t, f)
		if describe(t, f.root) != everything {
			t.Fatal("a stop for a blocked target must change nothing")
		}
	})
	t.Run("state changed after the review", func(t *testing.T) {
		f := linuxFixture(t)
		f.save()
		proposal, err := f.preview()
		if err != nil {
			t.Fatal(err)
		}
		f.v1.Generation++
		changed := f.save()
		if _, err := proposal.Run(context.Background()); !errors.Is(err, engine.ErrPlanChanged) {
			t.Fatalf("want a new review, got %v", err)
		}
		if read(t, filepath.Join(f.state, "state.json")) != string(changed) {
			t.Fatal("the state changed")
		}
	})
}

func deselectNeovim(t *testing.T, f *fx) {
	t.Helper()
	f.v1.Selected = slices.DeleteFunc(f.v1.Selected, func(id string) bool { return id == "neovim" })
	for _, id := range []string{"config.nvim", "tool.nvim", hooks.NvimResource} {
		delete(f.v1.Receipts, id)
	}
	for _, path := range []string{filepath.Join(f.home, ".config", "nvim"), f.publicRuntime()} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	f.save()
}

func TestAStagedBuildOfADeselectedToolDoesNotStopTheNextImport(t *testing.T) {
	for _, earlier := range []string{"restored import", "failed build"} {
		t.Run(earlier, func(t *testing.T) {
			f := linuxFixture(t)
			f.save()
			if earlier == "restored import" {
				f.mustImport()
				mustRestore(t, f)
			} else {
				f.failBuild[hooks.NvimResource] = true
				proposal, err := f.preview()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := proposal.Run(context.Background()); err == nil {
					t.Fatal("the fixture must stop at the failed build")
				}
				f.failBuild[hooks.NvimResource] = false
			}
			deselectNeovim(t, f)
			proposal, err := f.preview()
			if err != nil {
				t.Fatalf("a staged build of a deselected tool must not stop the import: %v", err)
			}
			for _, op := range proposal.Plan.Operations {
				if op.Action == "remove" || op.Action == "forget" {
					t.Fatalf("the import must not remove %s", op.Resource)
				}
			}
			if result, err := proposal.Run(context.Background()); err != nil {
				t.Fatalf("%v\n%s", err, result.Message)
			}
			st := f.current()
			if st.Schema != 2 || st.V2Restore != "open" || slices.Contains(st.Selected, "neovim") {
				t.Fatalf("after the import the state must be schema 2 without Neovim: %+v", st)
			}
			current := filepath.Join(f.state, "tools", "tool.nvim", "current")
			if _, has := st.Targets[current]; has {
				t.Fatalf("the state records the staged build of a deselected tool: %+v", st.Targets[current])
			}
			if read(t, filepath.Join(current, "bin", "tool")) != "v3 tool.nvim\n" {
				t.Fatal("the import must leave the staged build for the sweep")
			}
		})
	}
}

func TestAnInterruptedSwitchCompletesOrRestores(t *testing.T) {
	t.Cleanup(func() { v2import.SetFault(nil) })
	for _, point := range []string{"restore-set", "publish", "published", "apply", "open"} {
		for _, continuation := range []string{"complete", "restore"} {
			t.Run(point+"/"+continuation, func(t *testing.T) {
				f := linuxFixture(t)
				before := capture(t, f, f.save())
				proposal, err := f.preview()
				if err != nil {
					t.Fatal(err)
				}
				if point == "apply" {
					f.failUnit[shells] = 3
				} else {
					v2import.SetFault(func(at string) error {
						if at == point {
							return errors.New("interrupted at " + at)
						}
						return nil
					})
				}
				result, err := proposal.Run(context.Background())
				v2import.SetFault(nil)
				f.failUnit[shells] = 0
				if err == nil {
					t.Fatalf("the interruption did not stop the import: %+v", result)
				}
				published := point != "restore-set" && point != "publish"
				if !published {
					before.restored(t, f)
				} else {
					st := f.current()
					if st.V2Restore != "switching" {
						t.Fatalf("an unfinished switch must stay marked: %+v", st)
					}
					before.folders(t, f)
					e := f.engine()
					if _, err := e.Apply(context.Background(), engine.Request{Mode: "update"}, engine.Plan{Mode: "update", Selected: st.Selected, Keep: st.Keep, V2Restore: "switching"}); !errors.Is(err, engine.ErrSwitching) {
						t.Fatalf("an ordinary run must not start during the switch: %v", err)
					}
					check, err := e.Check(context.Background())
					if err != nil || check.Status != "needs-action" || !strings.Contains(check.Message, "The import from release v2 is not complete") {
						t.Fatalf("check must report the unfinished switch: %+v %v", check, err)
					}
				}
				if point == "apply" && read(t, filepath.Join(f.state, "shells", "env.sh")) != "# v3 env\n" {
					t.Fatal("the fixture must stop in the middle of the apply")
				}
				if continuation == "complete" {
					next, err := f.preview()
					if err != nil || next.Switching != published {
						t.Fatalf("the next run must continue from the published state: %+v %v", next, err)
					}
					if result, err := next.Run(context.Background()); err != nil {
						t.Fatalf("%v\n%s", err, result.Message)
					}
					switched(t, f)
					before.folders(t, f)
					mustRestore(t, f)
					before.restored(t, f)
					return
				}
				restoration, err := v2import.PreviewRestore(context.Background(), f.options())
				switch {
				case point == "restore-set":
					if err == nil || !strings.Contains(err.Error(), "nothing to restore") {
						t.Fatalf("without a restore set there is nothing to restore: %v", err)
					}
				case err != nil:
					t.Fatal(err)
				default:
					if _, err := restoration.Run(context.Background(), nil); err != nil {
						t.Fatal(err)
					}
				}
				before.restored(t, f)
				f.mustImport()
				switched(t, f)
			})
		}
	}
}

func TestAnInterruptedRestoreRunsAgain(t *testing.T) {
	t.Cleanup(func() { v2import.SetFault(nil) })
	f := linuxFixture(t)
	before := capture(t, f, f.save())
	f.mustImport()
	v2import.SetFault(func(at string) error {
		if at == "restore-publish" {
			return errors.New("interrupted before the publication")
		}
		return nil
	})
	restoration, err := v2import.PreviewRestore(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restoration.Run(context.Background(), nil); err == nil {
		t.Fatal("the interruption did not stop the restore")
	}
	if st := f.current(); st.V2Restore != "open" {
		t.Fatalf("the schema-2 state must stay until the publication: %+v", st)
	}
	v2import.SetFault(nil)
	again, _ := mustRestore(t, f)
	for _, item := range again.Items {
		if item.Action != "none" {
			t.Fatalf("the second run must find each target restored: %+v", item)
		}
	}
	before.restored(t, f)
}

func TestTheNextConfirmedRunClosesTheFallback(t *testing.T) {
	f := linuxFixture(t)
	f.save()
	f.mustImport()
	e := f.engine()
	check, err := e.Check(context.Background())
	if err != nil || !strings.Contains(check.Message, "the v2 fallback is open. Keep these folders") || !strings.Contains(check.Message, filepath.Join(f.state, "packages")) || strings.Contains(check.Message, "Owner cleanup") {
		t.Fatalf("check during the open fallback: %+v %v", check, err)
	}
	request := engine.Request{Mode: "update"}
	plan, err := e.Preview(context.Background(), request)
	if err != nil || engine.FallbackNotice(plan) != "" {
		t.Fatalf("a run that changes nothing must not name the end of the fallback: %q %v", engine.FallbackNotice(plan), err)
	}
	if _, err := e.Apply(context.Background(), request, plan); err != nil || f.current().V2Restore != "open" {
		t.Fatalf("a run that changes nothing must keep the fallback open: %v", err)
	}
	if err := os.Remove(f.fontPath()); err != nil {
		t.Fatal(err)
	}
	plan, err = e.Preview(context.Background(), request)
	if err != nil || !strings.Contains(engine.FallbackNotice(plan), "This closes the v2 fallback") {
		t.Fatalf("the preview must name the end of the fallback: %q %v", engine.FallbackNotice(plan), err)
	}
	if st := f.current(); st.V2Restore != "open" {
		t.Fatal("a preview must not close the fallback")
	}
	if _, err := e.Apply(context.Background(), request, plan); err != nil {
		t.Fatal(err)
	}
	if st := f.current(); st.V2Restore != "" {
		t.Fatalf("the confirmed run must close the fallback: %+v", st)
	}
	if _, err := v2import.PreviewRestore(context.Background(), f.options()); err == nil || !strings.Contains(err.Error(), "restore-v2 is not available") {
		t.Fatalf("restore-v2 must stop after the fallback closed: %v", err)
	}
	if _, err := f.preview(); err == nil {
		t.Fatal("no import is necessary after the fallback closed")
	}
	check, err = e.Check(context.Background())
	if err != nil || strings.Contains(check.Message, "fallback is open") {
		t.Fatalf("%+v %v", check, err)
	}
	for _, name := range []string{"packages", "nvim-sync", "recovery", "v2-restore"} {
		if !strings.Contains(check.Message, "Owner cleanup: "+filepath.Join(f.state, name)+" is a folder of release v2") {
			t.Fatalf("check must list %s for the owner:\n%s", name, check.Message)
		}
	}
	if !strings.Contains(check.Message, "the reference check of the v2 folders covers") {
		t.Fatalf("check must state the scope of its reference check:\n%s", check.Message)
	}
}

func TestTheMacImportKeepsTheDeferredApplicationsOnReleaseV2(t *testing.T) {
	f := darwinFixture(t)
	before := capture(t, f, f.save())
	application := filepath.Join(f.home, "Applications", "Visual Studio Code.app")
	legacy := filepath.Join(f.state, "packages", "tool.vscode", "current", "Visual Studio Code.app", "Contents", "Resources", "app", "bin")
	proposal, err := f.preview()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.legacy, []string{legacy}) {
		t.Fatalf("the shell profile must get the legacy command folder before its inspection: %v", f.legacy)
	}
	if !strings.Contains(proposal.Details, "A later Update tools run offers each cask install, and Choose tools keeps the v2 payload. The first confirmed run that changes something closes the v2 fallback:") {
		t.Fatalf("the review must name Update tools for the move and the end of the fallback:\n%s", proposal.Details)
	}
	if !strings.Contains(proposal.Details, "Applications that stay on release v2 in this run") || !strings.Contains(proposal.Details, "VS Code program (tool.vscode)") {
		t.Fatalf("the review must name the deferred applications:\n%s", proposal.Details)
	}
	result, err := proposal.Run(context.Background())
	if err != nil {
		t.Fatalf("%v\n%s", err, result.Message)
	}
	for _, id := range []string{"tool.vscode", "desktop.vscode", "infra.homebrew"} {
		if action(proposal.Plan, id) != "" || slices.Contains(f.inspected, id) {
			t.Fatalf("the import run must leave %s out of its closure", id)
		}
	}
	st := f.current()
	if st.V2Restore != "open" || st.Targets[application].Applied == "" || !slices.Contains(st.Selected, "vscode") {
		t.Fatalf("the legacy entry point must stay recorded: %+v", st.Targets[application])
	}
	if target, err := os.Readlink(application); err != nil || target != filepath.Join(f.state, "packages", "tool.vscode", "current", "Visual Studio Code.app") {
		t.Fatalf("the legacy application link changed: %q %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(f.home, ".local", "bin", "code")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the import must not create the command link of the cask")
	}
	before.folders(t, f)
	if !slices.Equal(v2import.LegacyCommandDirectories(f.state, st, f.ctx), []string{legacy}) {
		t.Fatal("the legacy command folder must stay known after the import")
	}
	later, err := f.engine().Preview(context.Background(), engine.Request{Mode: "apply"})
	if err != nil || action(later, "tool.vscode") != "install" || !strings.Contains(engine.FallbackNotice(later), "closes the v2 fallback") {
		t.Fatalf("a later run must propose the cask install as an ordinary action: %q %v", action(later, "tool.vscode"), err)
	}
	mustRestore(t, f)
	before.restored(t, f)
}

func TestTheMacImportStopsWhenADeferredApplicationCannotStart(t *testing.T) {
	f := darwinFixture(t)
	if err := os.Remove(filepath.Join(f.state, "packages", "tool.vscode", "current", "Visual Studio Code.app", "Contents", "MacOS", "Code")); err != nil {
		t.Fatal(err)
	}
	before := capture(t, f, f.save())
	proposal, err := f.preview()
	if err != nil {
		t.Fatal(err)
	}
	result, err := proposal.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "do not start: VS Code program (tool.vscode). Repair them with the setup of "+v2import.SourceCommit) {
		t.Fatalf("the import must stop and name the application that does not start: %v\n%s", err, result.Message)
	}
	before.restored(t, f)
	for _, name := range []string{"v2-restore", "tools"} {
		if _, err := os.Lstat(filepath.Join(f.state, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the stop must come before the builds and the restore set: %s %v", name, err)
		}
	}
}

func TestTheMacImportStopsWhenALauncherOfADeferredApplicationIsMissing(t *testing.T) {
	for name, c := range map[string]struct {
		prepare func(f *fx) string
		named   string
	}{
		"OpenWhispr.app": {named: "OpenWhispr program (tool.openwhispr)", prepare: func(f *fx) string {
			f.catalog.Resources = append(f.catalog.Resources,
				catalog.Resource{ID: "openwhispr", Name: "OpenWhispr", Capability: true, Requires: []string{"tool.openwhispr", "desktop.openwhispr"}},
				catalog.Resource{ID: "tool.openwhispr", Name: "OpenWhispr program", Action: "package",
					Bindings:         map[string]catalog.Binding{"darwin": {Provider: "homebrew-cask", Package: "openwhispr"}},
					PlatformRequires: map[string][]string{"darwin": {"infra.homebrew"}}},
				catalog.Resource{ID: "desktop.openwhispr", Name: "OpenWhispr launcher", Action: "desktop", Requires: []string{"tool.openwhispr"}})
			payload := filepath.Join(f.state, "packages", "tool.openwhispr", "versions", "dddd", "payload")
			write(t, filepath.Join(payload, "OpenWhispr.app", "Contents", "MacOS", "OpenWhispr"), "v2 openwhispr\n", 0755)
			link(t, payload, filepath.Join(f.state, "packages", "tool.openwhispr", "current"))
			f.receipt("tool.openwhispr", "archive", "created", true)
			launcher := filepath.Join(f.home, "Applications", "OpenWhispr.app")
			f.configuration("desktop.openwhispr", "configuration", map[string]func(string){
				launcher: func(path string) {
					link(t, filepath.Join(f.state, "packages", "tool.openwhispr", "current", "OpenWhispr.app"), path)
				}})
			f.v1.Selected = append(f.v1.Selected, "openwhispr")
			return launcher
		}},
		"code": {named: "VS Code program (tool.vscode)", prepare: func(f *fx) string {
			launcher := filepath.Join(f.home, "Applications", "Visual Studio Code.app")
			command := filepath.Join(f.home, ".local", "bin", "code")
			if err := os.Remove(launcher); err != nil {
				t.Fatal(err)
			}
			bundle := filepath.Join(f.state, "packages", "tool.vscode", "current", "Visual Studio Code.app")
			f.configuration("desktop.vscode", "configuration", map[string]func(string){
				launcher: func(path string) { link(t, bundle, path) },
				command:  func(path string) { link(t, filepath.Join(bundle, "Contents", "Resources", "app", "bin", "code"), path) }})
			return command
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := darwinFixture(t)
			if err := os.Remove(c.prepare(f)); err != nil {
				t.Fatal(err)
			}
			before := capture(t, f, f.save())
			proposal, err := f.preview()
			if err != nil {
				t.Fatal(err)
			}
			result, err := proposal.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "do not start: "+c.named+". Repair them with the setup of "+v2import.SourceCommit) {
				t.Fatalf("the import must stop and name the application without its launcher: %v\n%s", err, result.Message)
			}
			before.restored(t, f)
			if _, err := os.Lstat(filepath.Join(f.state, "v2-restore")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the stop must come before the restore set: %v", err)
			}
		})
	}
}

func TestStateOfTheImportedMachineIsValidSchema2(t *testing.T) {
	f := linuxFixture(t)
	f.save()
	f.mustImport()
	st, err := state.Peek(f.state, f.engine().Target)
	if err != nil || st.Validate() != nil {
		t.Fatal(st, err)
	}
	for key, record := range st.Targets {
		if record.Next != "" || record.Release {
			t.Fatalf("%s has an unfinished write: %+v", key, record)
		}
	}
}

func TestTheMacImportConvergesARecordedRequirementOfADeferredApplication(t *testing.T) {
	f := darwinFixture(t)
	f.v1.Selected = slices.DeleteFunc(f.v1.Selected, func(id string) bool { return id == "shell" })
	before := capture(t, f, f.save())
	proposal, err := f.preview()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(proposal.Mapping.Deferred, []string{"desktop.vscode", "infra.homebrew", "tool.vscode"}) {
		t.Fatalf("the shell profiles have records of release v2, so they must stay in the closure: %v", proposal.Mapping.Deferred)
	}
	if got := action(proposal.Plan, shells); got != "repair" {
		t.Fatalf("the shell profiles are %q in the import plan", got)
	}
	for _, op := range proposal.Plan.Operations {
		if op.Action == "remove" || op.Action == "forget" {
			t.Fatalf("the import must not remove %s", op.Resource)
		}
	}
	if result, err := proposal.Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, result.Message)
	}
	if got := read(t, filepath.Join(f.state, "shells", "env.sh")); got != "# v3 env\n" || f.current().V2Restore != "open" {
		t.Fatalf("the switch must converge the shell profiles: %q", got)
	}
	mustRestore(t, f)
	before.restored(t, f)
}
