package v2import_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
	"github.com/luisgui1757/dotfiles/installer/v2import"
)

func TestAGeneratedShellFileWithPersonalLinesStaysUntilADecision(t *testing.T) {
	f := linuxFixture(t)
	initFile, env := filepath.Join(f.state, "shells", "init.sh"), filepath.Join(f.state, "shells", "env.sh")
	write(t, initFile, read(t, initFile)+"alias mine='personal line after the block'\n", 0644)
	personal := read(t, initFile)
	before := capture(t, f, f.save())
	mapping := mustMap(t, f)
	if _, has := mapping.State.Targets[initFile]; has {
		t.Fatalf("a generated file with lines outside the v2 block must get no ownership: %+v", mapping.State.Targets[initFile])
	}
	owned(t, mapping.State, env, shells, "file", "absent", filepath.Dir(env))
	if !strings.Contains(strings.Join(mapping.Notes, "\n"), initFile+" holds lines outside the block that release v2 wrote") {
		t.Fatalf("the mapping must name the file: %v", mapping.Notes)
	}
	proposal, err := f.preview()
	if err != nil {
		t.Fatal(err)
	}
	if got := action(proposal.Plan, shells); got != "pending" {
		t.Fatalf("the shell profiles must wait for a replacement decision: %q", got)
	}
	if result, err := proposal.Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, result.Message)
	}
	if read(t, initFile) != personal || f.current().V2Restore != "open" {
		t.Fatalf("the switch replaced personal lines:\n%s", read(t, initFile))
	}
	mustRestore(t, f)
	before.restored(t, f)
}

func TestALostV2BackupGivesNoOwnership(t *testing.T) {
	f := linuxFixture(t)
	f.save()
	tmux := filepath.Join(f.home, ".tmux.conf")
	backup := filepath.Join(f.home, ".dotfiles-config-"+operation("config.tmux")+"-0", "previous")
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	mapping := mustMap(t, f)
	record := mapping.State.Targets[tmux]
	if record.Applied != "" || !strings.HasPrefix(record.Before, "fp:file:") {
		t.Fatalf("a unit without its v2 backup must be baseline-only: %+v", record)
	}
	if notes := strings.Join(mapping.Notes, "\n"); !strings.Contains(notes, tmux) || !strings.Contains(notes, backup) {
		t.Fatalf("the note must name the unit and the lost backup: %v", mapping.Notes)
	}
	owned(t, mapping.State, filepath.Join(f.home, ".config", "nvim"), "config.nvim", "link", "absent", filepath.Join(f.home, ".config"))
}

type interruptedKind struct{ targets.Kind }

func aside(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(filepath.Dir(key), ".dotfiles-"+hex.EncodeToString(sum[:8])+".previous")
}

func (k interruptedKind) Restore(_ context.Context, d targets.Desired, _ state.Record, _ string) error {
	if err := os.Rename(d.Path, aside(d.Key)); err != nil {
		return err
	}
	return errors.New("interrupted after the move of the v3 file")
}

func TestARestoreInterruptedAfterTheMoveCompletesOnTheNextRun(t *testing.T) {
	f := linuxFixture(t)
	before := capture(t, f, f.save())
	f.mustImport()
	f.extraKinds = map[string]targets.Kind{"file": interruptedKind{targets.FileKind("file")}}
	restoration, err := v2import.PreviewRestore(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(f.state, "shells", "env.sh")
	if _, err := restoration.Run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), env) || !strings.Contains(err.Error(), "run restore-v2 again") {
		t.Fatalf("the interrupted restore must name the slot and stop: %v", err)
	}
	if _, err := os.Lstat(env); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the fixture must stop after the v3 file moved aside")
	}
	if st := f.current(); st.V2Restore != "open" {
		t.Fatalf("schema 2 must stay active: %+v", st)
	}
	f.extraKinds = nil
	mustRestore(t, f)
	before.restored(t, f)
}

func editManifest(t *testing.T, f *fx, edit func(slots map[string]any)) {
	t.Helper()
	path := filepath.Join(f.state, "v2-restore", "manifest.json")
	var manifest map[string]any
	if err := json.Unmarshal([]byte(read(t, path)), &manifest); err != nil {
		t.Fatal(err)
	}
	edit(manifest["slots"].(map[string]any))
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(data)+"\n", 0600)
}

func TestRestoreDoesNotPublishSchema1OverASlotItCannotCompare(t *testing.T) {
	f := linuxFixture(t)
	before := capture(t, f, f.save())
	f.mustImport()
	env := filepath.Join(f.state, "shells", "env.sh")
	saved := ""
	editManifest(t, f, func(slots map[string]any) {
		saved = strings.TrimPrefix(slots[env].(map[string]any)["saved"].(string), "backup:")
	})
	hidden := saved + ".hidden"
	if err := os.Rename(saved, hidden); err != nil {
		t.Fatal(err)
	}
	restoration, err := v2import.PreviewRestore(context.Background(), f.options())
	if err != nil || !strings.Contains(restoration.Text(), "blocked  "+env) {
		t.Fatalf("the preview must show the blocked slot: %v\n%s", err, restoration.Text())
	}
	if _, err := restoration.Run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), env) || !strings.Contains(err.Error(), "run restore-v2 again") {
		t.Fatalf("a slot without a readable copy must stop the restore: %v", err)
	}
	if st := f.current(); st.V2Restore != "open" {
		t.Fatalf("schema 2 must stay active: %+v", st)
	}
	if err := os.Rename(hidden, saved); err != nil {
		t.Fatal(err)
	}
	mustRestore(t, f)
	before.restored(t, f)
}

func TestASlotWithOnlyAFingerprintIsDisclosedAndDoesNotStopTheRestore(t *testing.T) {
	f := linuxFixture(t)
	before := capture(t, f, f.save())
	f.mustImport()
	editManifest(t, f, func(slots map[string]any) {
		slots[f.fontPath()].(map[string]any)["saved"] = "fp:link:no-copy-by-design"
	})
	_, message := mustRestore(t, f)
	if !strings.Contains(message, "Not restored: "+f.fontPath()) || !strings.Contains(message, "Release v2 is active again") {
		t.Fatalf("a slot without a copy by design must be named, and the restore must end: %q", message)
	}
	before.schema1(t, f)
	if read(t, f.fontPath()) != "v3 font bytes\n" {
		t.Fatal("the fixture must leave the slot without a copy as it is")
	}
}

type uncopiedKind struct{ targets.Kind }

func (uncopiedKind) Observe(d targets.Desired) (string, error) {
	return "", errors.New("the copy for restore-v2 failed")
}

func TestTheSwitchStopsBeforeASlotWhoseRestoreCopyFailed(t *testing.T) {
	f := linuxFixture(t)
	before := capture(t, f, f.save())
	f.captureKinds = map[string]targets.Kind{"file": uncopiedKind{targets.FileKind("file")}}
	proposal, err := f.preview()
	if err != nil {
		t.Fatal(err)
	}
	result, err := proposal.Run(context.Background())
	if err == nil || !strings.Contains(result.Message, "cannot save "+filepath.Join(f.state, "shells", "env.sh")+" for restore-v2") {
		t.Fatalf("a failed restore copy must stop the step: %v\n%s", err, result.Message)
	}
	if got := describe(t, filepath.Join(f.state, "shells")); got != before.shells {
		t.Fatalf("the switch changed a slot without its restore copy:\n%s", got)
	}
	if read(t, f.fontPath()) != "v2 font bytes\n" {
		t.Fatal("the switch changed the font without its restore copy")
	}
	if st := f.current(); st.V2Restore != "switching" {
		t.Fatalf("the switch must stay unfinished: %+v", st)
	}
	f.captureKinds = nil
	mustRestore(t, f)
	before.restored(t, f)
}

func editedFingerprint(t *testing.T, path string) string {
	t.Helper()
	snapshot, err := platform.SnapshotTree(path)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.Kind + ":" + snapshot.Hash
}

func setRestoring(t *testing.T, f *fx, restoring map[string]any) {
	t.Helper()
	path := filepath.Join(f.state, "v2-restore", "manifest.json")
	var manifest map[string]any
	if err := json.Unmarshal([]byte(read(t, path)), &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["restoring"] = restoring
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(data)+"\n", 0600)
}

func editedCopies(t *testing.T, f *fx) string {
	t.Helper()
	copies := ""
	entries, err := os.ReadDir(filepath.Join(f.state, "v2-restore", "edited"))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		copies += read(t, filepath.Join(f.state, "v2-restore", "edited", entry.Name()))
	}
	return copies
}

func TestARestoreRetryNeverReplacesAnEditWithoutItsVerifiedCopy(t *testing.T) {
	t.Cleanup(func() { v2import.SetFault(nil) })
	const personal = "# v3 env\n# my edit of a generated file\n"
	t.Run("progress without a copy", func(t *testing.T) {
		f := linuxFixture(t)
		f.save()
		f.mustImport()
		env := filepath.Join(f.state, "shells", "env.sh")
		write(t, env, personal, 0644)
		setRestoring(t, f, map[string]any{env: editedFingerprint(t, env)})
		_, message := mustRestore(t, f)
		if read(t, env) != personal || !strings.Contains(message, "Kept your changed content: "+env) {
			t.Fatalf("a retry replaced an edit that has no saved copy: %q\n%s", read(t, env), message)
		}
	})
	t.Run("stop after the copy", func(t *testing.T) {
		f := linuxFixture(t)
		before := capture(t, f, f.save())
		f.mustImport()
		env := filepath.Join(f.state, "shells", "env.sh")
		write(t, env, personal, 0644)
		restoration, err := v2import.PreviewRestore(context.Background(), f.options())
		if err != nil {
			t.Fatal(err)
		}
		v2import.SetFault(func(at string) error {
			if at == "restore-slot:"+env {
				return errors.New("interrupted before the replacement")
			}
			return nil
		})
		if _, err := restoration.Run(context.Background(), []string{env}); err == nil {
			t.Fatal("the interruption did not stop the restore")
		}
		v2import.SetFault(nil)
		if read(t, env) != personal || editedCopies(t, f) != personal {
			t.Fatalf("at the stop the edit must be in place and in its verified copy: %q %q", read(t, env), editedCopies(t, f))
		}
		mustRestore(t, f)
		if editedCopies(t, f) != personal {
			t.Fatalf("the retry lost the copy of the edit: %q", editedCopies(t, f))
		}
		before.restored(t, f)
	})
}

func TestARestoreRetryCompletesAfterAStopBetweenTheStageAndTheMove(t *testing.T) {
	f := linuxFixture(t)
	before := capture(t, f, f.save())
	f.mustImport()
	env := filepath.Join(f.state, "shells", "env.sh")
	saved := ""
	editManifest(t, f, func(slots map[string]any) {
		saved = strings.TrimPrefix(slots[env].(map[string]any)["saved"].(string), "backup:")
	})
	setRestoring(t, f, map[string]any{env: editedFingerprint(t, env)})
	staged := strings.TrimSuffix(aside(env), ".previous") + ".next"
	if err := os.Link(saved, staged); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(env, aside(env)); err != nil {
		t.Fatal(err)
	}
	mustRestore(t, f)
	before.restored(t, f)
}

func TestAResumedSwitchNeverChangesASystemItemOrRunsSentinel(t *testing.T) {
	t.Cleanup(func() { v2import.SetFault(nil) })
	for _, id := range []string{"tool.zsh", "integration.sentinel-installer"} {
		t.Run(id, func(t *testing.T) {
			f := linuxFixture(t)
			before := capture(t, f, f.save())
			proposal, err := f.preview()
			if err != nil {
				t.Fatal(err)
			}
			v2import.SetFault(func(at string) error {
				if at == "published" {
					return errors.New("interrupted after the publication")
				}
				return nil
			})
			if _, err := proposal.Run(context.Background()); err == nil {
				t.Fatal("the interruption did not stop the import")
			}
			v2import.SetFault(nil)
			f.present[id] = false
			next, err := f.preview()
			if err != nil || !next.Switching {
				t.Fatalf("%+v %v", next, err)
			}
			if !strings.Contains(next.Details, "Items that the switch does not change while the v2 fallback is open") || !strings.Contains(next.Details, "("+id+")") {
				t.Fatalf("the review of the resumed switch must name the blocker:\n%s", next.Details)
			}
			result, err := next.Run(context.Background())
			if err == nil || len(f.systemChanges) != 0 {
				t.Fatalf("the resumed switch changed %v: %v", f.systemChanges, err)
			}
			if !strings.Contains(result.Message, id+" needs a system change or the Sentinel installer") || !strings.Contains(result.Message, "Run restore-v2 and finish this work with the v2.3.0 checkout") {
				t.Fatalf("the report must name the blocker and the way back:\n%s", result.Message)
			}
			if st := f.current(); st.V2Restore != "switching" {
				t.Fatalf("the switch must stay unfinished: %+v", st)
			}
			mustRestore(t, f)
			before.restored(t, f)
		})
	}
}

func TestRestoreStopsWhenAFileChangedAfterTheReview(t *testing.T) {
	f := linuxFixture(t)
	f.save()
	f.mustImport()
	restoration, err := v2import.PreviewRestore(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(f.state, "shells", "env.sh")
	write(t, env, "# v3 env\n# edited after the review\n", 0644)
	if _, err := restoration.Run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "the files changed after the review; run restore-v2 again") {
		t.Fatalf("a plan that differs from the review must stop: %v", err)
	}
	if st := f.current(); st.V2Restore != "open" || read(t, env) != "# v3 env\n# edited after the review\n" {
		t.Fatalf("the stop must change nothing: %+v", st)
	}
	if read(t, f.fontPath()) != "v3 font bytes\n" {
		t.Fatal("the stop must come before the first write")
	}
}

func editState(t *testing.T, f *fx, edit func(document map[string]any)) {
	t.Helper()
	path := filepath.Join(f.state, "state.json")
	var document map[string]any
	if err := json.Unmarshal([]byte(read(t, path)), &document); err != nil {
		t.Fatal(err)
	}
	edit(document)
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(data)+"\n", 0600)
}

func TestRestoreRecoversAnInterruptedSwitchWriteBeforeItRollsBack(t *testing.T) {
	f := linuxFixture(t)
	before := capture(t, f, f.save())
	f.mustImport()
	env := filepath.Join(f.state, "shells", "env.sh")
	saved := ""
	editManifest(t, f, func(slots map[string]any) {
		saved = strings.TrimPrefix(slots[env].(map[string]any)["saved"].(string), "backup:")
	})
	v2, v3 := editedFingerprint(t, saved), editedFingerprint(t, env)
	editState(t, f, func(document map[string]any) {
		record := document["targets"].(map[string]any)[env].(map[string]any)
		record["applied"], record["next"] = v2, v3
	})
	if err := os.Rename(env, strings.TrimSuffix(aside(env), ".previous")+".next"); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(saved, aside(env)); err != nil {
		t.Fatal(err)
	}
	mustRestore(t, f)
	before.restored(t, f)
}

func TestASwitchRunDoesNotReopenAClosedFallback(t *testing.T) {
	f := linuxFixture(t)
	f.save()
	f.verify = func(context.Context) error {
		store, err := state.Open(f.state, f.engine().Target)
		if err != nil {
			return err
		}
		err = store.Update(func(s *state.State) error { s.V2Restore = ""; return nil })
		return errors.Join(err, store.Close())
	}
	result := f.mustImport()
	if st := f.current(); st.V2Restore != "" {
		t.Fatalf("a switch run reopened a fallback that another run closed: %+v", st)
	}
	if !strings.Contains(result.Message, "The switch from release v2 is already complete") || strings.Contains(result.Message, "restore-v2 can give release v2 back") {
		t.Fatalf("the report must say that the switch is already complete:\n%s", result.Message)
	}
}

func TestRestoreStopsWhenASavedCopyChangedAfterTheReview(t *testing.T) {
	f := linuxFixture(t)
	f.save()
	f.mustImport()
	restoration, err := v2import.PreviewRestore(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(f.state, "shells", "env.sh")
	saved := ""
	editManifest(t, f, func(slots map[string]any) {
		saved = strings.TrimPrefix(slots[env].(map[string]any)["saved"].(string), "backup:")
	})
	write(t, saved, "tampered copy\n", 0644)
	if _, err := restoration.Run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "the files changed after the review; run restore-v2 again") {
		t.Fatalf("a saved copy that changed after the review must stop the restore: %v", err)
	}
	if st := f.current(); st.V2Restore != "open" || read(t, env) != "# v3 env\n" {
		t.Fatalf("the stop must change nothing: %+v %q", st, read(t, env))
	}
}

func TestASwitchNeverRemovesASystemResource(t *testing.T) {
	t.Cleanup(func() { v2import.SetFault(nil) })
	f := linuxFixture(t)
	f.save()
	proposal, err := f.preview()
	if err != nil {
		t.Fatal(err)
	}
	v2import.SetFault(func(at string) error {
		if at == "published" {
			return errors.New("interrupted after the publication")
		}
		return nil
	})
	if _, err := proposal.Run(context.Background()); err == nil {
		t.Fatal("the interruption did not stop the import")
	}
	v2import.SetFault(nil)
	editState(t, f, func(document map[string]any) {
		selected := []any{}
		for _, id := range document["selected"].([]any) {
			if id != "keyboard" {
				selected = append(selected, id)
			}
		}
		document["selected"] = selected
	})
	next, err := f.preview()
	if err != nil || action(next.Plan, "system.keyboard-access") != "remove" {
		t.Fatalf("the fixture must plan the removal of the keyboard rule: %q %v", action(next.Plan, "system.keyboard-access"), err)
	}
	result, err := next.Run(context.Background())
	if err == nil || len(f.systemChanges) != 0 {
		t.Fatalf("the switch removed a system resource: %v %v", f.systemChanges, err)
	}
	if !strings.Contains(result.Message, "system.keyboard-access needs a system change or the Sentinel installer") {
		t.Fatalf("the report must name the blocker:\n%s", result.Message)
	}
	if _, recorded := f.current().Targets[f.keyboard]; !recorded || read(t, f.keyboard) != keyboardRule(operation("keyboard")) {
		t.Fatal("the keyboard rule and its record must stay")
	}
}
