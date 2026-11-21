package v2import_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/hooks"
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
		slot := slots[f.fontPath()].(map[string]any)
		slot["saved"], slot["fingerprint"] = "fp:link:no-copy-by-design", "link:no-copy-by-design"
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

func TestASecondStopInTheRollbackOfASwitchWriteCompletesOnTheNextRun(t *testing.T) {
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
	f.extraKinds = map[string]targets.Kind{"file": interruptedKind{targets.FileKind("file")}}
	restoration, err := v2import.PreviewRestore(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restoration.Run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), env) {
		t.Fatalf("the stop in the rollback must name the slot: %v", err)
	}
	f.extraKinds = nil
	if _, err := os.Lstat(env); !errors.Is(err, os.ErrNotExist) || read(t, aside(env)) != "# v3 env\n" {
		t.Fatal("the fixture must stop after the v3 file moved aside")
	}
	var manifest v2import.Manifest
	if err := json.Unmarshal([]byte(read(t, filepath.Join(f.state, "v2-restore", "manifest.json"))), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Restoring[env] != v3 {
		t.Errorf("the rollback must save its intent before its first move: %v", manifest.Restoring)
	}
	if record := f.current().Targets[env]; record.Next != v3 || record.Release {
		t.Fatalf("the state must keep the forward intent of the switch: %+v", record)
	}
	mustRestore(t, f)
	before.restored(t, f)
}

func TestRestoreStopsWhenTheSavedStateChangedAfterTheReview(t *testing.T) {
	f := linuxFixture(t)
	f.save()
	f.mustImport()
	restoration, err := v2import.PreviewRestore(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	stateFile, manifest := filepath.Join(f.state, "state.json"), filepath.Join(f.state, "v2-restore", "manifest.json")
	published, recorded := read(t, stateFile), read(t, manifest)
	write(t, filepath.Join(f.state, "v2-restore", "state.json"), `{"schema":1}`, 0600)
	if _, err := restoration.Run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "the saved v2 state changed after the review; run restore-v2 again") {
		t.Fatalf("a saved state that changed after the review must stop the restore: %v", err)
	}
	if read(t, stateFile) != published || read(t, manifest) != recorded || read(t, filepath.Join(f.state, "shells", "env.sh")) != "# v3 env\n" || read(t, f.fontPath()) != "v3 font bytes\n" {
		t.Fatal("the stop must come before the first write")
	}
}

func TestRestoreRefusesAnInvalidSavedStateAtTheReview(t *testing.T) {
	f := linuxFixture(t)
	f.save()
	f.mustImport()
	write(t, filepath.Join(f.state, "v2-restore", "state.json"), `{"schema":1}`, 0600)
	if _, err := v2import.PreviewRestore(context.Background(), f.options()); err == nil || !strings.Contains(err.Error(), "the restore set has no valid state of release v2") {
		t.Fatalf("the review must validate the saved state with the reader of release v2: %v", err)
	}
}

func TestRestoreRechecksThePendingMarksUnderTheLock(t *testing.T) {
	f := linuxFixture(t)
	before := capture(t, f, f.save())
	f.mustImport()
	restoration, err := v2import.PreviewRestore(context.Background(), f.options())
	if err != nil || len(restoration.Pending) != 0 {
		t.Fatalf("%v %v", restoration, err)
	}
	store, err := state.Open(f.state, f.engine().Target)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Update(func(s *state.State) error { s.Mark("tool.font:activate"); return nil })
	if err = errors.Join(err, store.Close()); err != nil {
		t.Fatal(err)
	}
	marked := read(t, filepath.Join(f.state, "state.json"))
	message, err := restoration.Run(context.Background(), nil)
	if !errors.Is(err, engine.ErrPending) || !strings.Contains(message, "An earlier step did not report its end: tool.font:activate.") {
		t.Fatalf("a mark that appeared after the review must stop the restore: %q %v", message, err)
	}
	if read(t, filepath.Join(f.state, "state.json")) != marked || read(t, f.fontPath()) != "v3 font bytes\n" {
		t.Fatal("the stop must come before the first write")
	}
	restoration.Confirmed = []string{"tool.font:activate"}
	if _, err := restoration.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	before.restored(t, f)
}

type held struct{ st state.State }

func (h *held) State() state.State { return h.st.Clone() }

func (h *held) Update(edit func(*state.State) error) error {
	next := h.st.Clone()
	if err := edit(&next); err != nil {
		return err
	}
	h.st = next
	return nil
}

func TestAnEditDuringTheMappingIsNeverOwned(t *testing.T) {
	t.Cleanup(func() { v2import.SetFault(nil) })
	type edit struct{ path, key string }
	cases := map[string]func(f *fx) edit{
		"generated file": func(f *fx) edit {
			env := filepath.Join(f.state, "shells", "env.sh")
			return edit{env, env}
		},
		"block": func(f *fx) edit {
			zshrc := filepath.Join(f.home, ".zshrc")
			return edit{zshrc, targets.Key(zshrc, shells)}
		},
		"field": func(f *fx) edit {
			settings := filepath.Join(f.home, ".config", "Code", "User", "settings.json")
			return edit{settings, targets.Key(settings, "update.mode")}
		},
		"runtime link": func(f *fx) edit { return edit{f.publicRuntime(), f.publicRuntime()} },
	}
	for name, unit := range cases {
		t.Run(name, func(t *testing.T) {
			f := linuxFixture(t)
			f.save()
			target := unit(f)
			change := func() {
				switch name {
				case "runtime link":
					if err := os.Remove(target.path); err != nil {
						t.Fatal(err)
					}
					link(t, filepath.Join(f.home, "my-runtime"), target.path)
				case "field":
					write(t, target.path, strings.Replace(read(t, target.path), `"none"`, `"default"`, 1), 0644)
				case "block":
					write(t, target.path, strings.Replace(read(t, target.path), ". v2-init", ". my edit", 1), 0644)
				default:
					write(t, target.path, read(t, target.path)+"# my edit\n", 0644)
				}
			}
			edited := false
			v2import.SetFault(func(at string) error {
				if at == "read:"+target.path && !edited {
					edited = true
					change()
				}
				return nil
			})
			mapping := mustMap(t, f)
			v2import.SetFault(nil)
			if !edited {
				t.Fatal("the fixture must edit the target after its first read")
			}
			live, err := f.writer().InspectRecord(target.key, &held{mapping.State})
			if err != nil {
				t.Fatal(err)
			}
			if live.State == targets.StateApplied {
				t.Fatalf("the import recorded an edit made during its inspection as owned: %+v %+v", mapping.State.Targets[target.key], live)
			}
		})
	}
}

func TestAnEditedV2ProfileBaselineGivesNoOwnership(t *testing.T) {
	f := linuxFixture(t)
	f.save()
	path := filepath.Join(f.state, f.recovery(vscode), "profile.json")
	var baseline map[string]any
	if err := json.Unmarshal([]byte(read(t, path)), &baseline); err != nil {
		t.Fatal(err)
	}
	entry := baseline["entries"].([]any)[0].(map[string]any)
	entry["block"] = []byte(`{"update.mode":"manual","workbench.colorTheme":"Personal <Dark>"}`)
	f.document(path, baseline)
	mapping := mustMap(t, f)
	settings := filepath.Join(f.home, ".config", "Code", "User", "settings.json")
	for _, field := range []string{"workbench.colorTheme", "update.mode"} {
		record := mapping.State.Targets[targets.Key(settings, field)]
		if record.Applied != "" || !strings.HasPrefix(record.Before, "fp:") {
			t.Fatalf("%s: an edited v2 baseline must give a baseline-only record without its values: %+v", field, record)
		}
	}
	if notes := strings.Join(mapping.Notes, "\n"); !strings.Contains(notes, vscode) || !strings.Contains(notes, path) {
		t.Fatalf("the note must name the resource and its baseline: %v", mapping.Notes)
	}
}

func savedCopy(t *testing.T, f *fx, key string) string {
	t.Helper()
	saved := ""
	editManifest(t, f, func(slots map[string]any) {
		saved = strings.TrimPrefix(slots[key].(map[string]any)["saved"].(string), "backup:")
	})
	return saved
}

func TestRestoreStopsWhenASavedCopyChangedBeforeTheReview(t *testing.T) {
	f := linuxFixture(t)
	f.save()
	f.mustImport()
	env := filepath.Join(f.state, "shells", "env.sh")
	write(t, savedCopy(t, f, env), "tampered copy\n", 0644)
	restoration, err := v2import.PreviewRestore(context.Background(), f.options())
	if err != nil || !strings.Contains(restoration.Text(), "blocked  "+env) {
		t.Fatalf("the review must block a slot whose saved copy changed: %v\n%s", err, restoration.Text())
	}
	if _, err := restoration.Run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), env) {
		t.Fatalf("a changed saved copy must stop the restore and name the slot: %v", err)
	}
	if st := f.current(); st.V2Restore != "open" || read(t, env) != "# v3 env\n" {
		t.Fatalf("the stop must keep release v3 and the target: %+v %q", st, read(t, env))
	}
}

func TestRestoreStopsWhenASavedCopyChangesAtTheWrite(t *testing.T) {
	t.Cleanup(func() { v2import.SetFault(nil) })
	f := linuxFixture(t)
	f.save()
	f.mustImport()
	env := filepath.Join(f.state, "shells", "env.sh")
	saved := savedCopy(t, f, env)
	restoration, err := v2import.PreviewRestore(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	v2import.SetFault(func(at string) error {
		if at == "restore-slot:"+env {
			write(t, saved, "tampered copy\n", 0644)
		}
		return nil
	})
	if _, err := restoration.Run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), env) {
		t.Fatalf("a saved copy that changes at the write must stop the restore and name the slot: %v", err)
	}
	if st := f.current(); st.V2Restore != "open" || read(t, env) == "tampered copy\n" {
		t.Fatalf("the restore must not publish release v2 or write the changed copy: %+v %q", st, read(t, env))
	}
}

func TestRestoreKeepsATargetThatYouDeletedAfterTheSwitch(t *testing.T) {
	for _, replace := range []bool{false, true} {
		f := linuxFixture(t)
		f.save()
		v2env := read(t, filepath.Join(f.state, "shells", "env.sh"))
		f.mustImport()
		env := filepath.Join(f.state, "shells", "env.sh")
		if err := os.Remove(env); err != nil {
			t.Fatal(err)
		}
		restoration, err := v2import.PreviewRestore(context.Background(), f.options())
		if err != nil {
			t.Fatal(err)
		}
		changed := restoration.Changed()
		if len(changed) != 1 || changed[0].Key != env || changed[0].Replacement != "restore" {
			t.Fatalf("a target that you deleted after the switch is changed content: %+v", restoration.Items)
		}
		chosen := []string{}
		if replace {
			chosen = []string{env}
		}
		message, err := restoration.Run(context.Background(), chosen)
		if err != nil {
			t.Fatal(err)
		}
		if replace {
			if read(t, env) != v2env {
				t.Fatalf("an approved replacement must give back the value of release v2: %q", read(t, env))
			}
			continue
		}
		if _, err := os.Lstat(env); !errors.Is(err, os.ErrNotExist) || !strings.Contains(message, "Kept your changed content: "+env) {
			t.Fatalf("without a decision the deletion must stay and be reported: %v %q", err, message)
		}
	}
}

func TestRestoreRecoversAnInterruptedSwitchReleaseBeforeItRollsBack(t *testing.T) {
	f := linuxFixture(t)
	before := capture(t, f, f.save())
	zshrc := filepath.Join(f.home, ".zshrc")
	key := targets.Key(zshrc, shells)
	v2zshrc := read(t, zshrc)
	f.mustImport()
	write(t, zshrc, v2zshrc, 0644)
	live, err := f.writer().InspectRecord(key, &held{f.current()})
	if err != nil {
		t.Fatal(err)
	}
	without, err := targets.Block{}.Replace([]byte(v2zshrc), targets.Desired{Path: zshrc, Slot: shells}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	editState(t, f, func(document map[string]any) {
		record := document["targets"].(map[string]any)[key].(map[string]any)
		record["applied"], record["next"], record["release"] = live.Fingerprint, targets.Absent, true
	})
	write(t, strings.TrimSuffix(aside(key), ".previous")+".next", string(without), 0644)
	if err := os.Rename(zshrc, aside(key)); err != nil {
		t.Fatal(err)
	}
	mustRestore(t, f)
	before.restored(t, f)
}

func TestRestoreGivesBackASlotThatTheSwitchReleasedTowardItsBaseline(t *testing.T) {
	f := linuxFixture(t)
	settings := filepath.Join(f.home, ".config", "Code", "User", "settings.json")
	f.units[vscode] = func() []targets.Desired {
		return []targets.Desired{
			{Kind: "field", Path: settings, Slot: "workbench.colorTheme", Content: []byte(`"Rosé Pine Moon"`)},
			{Kind: "field", Path: settings, Slot: "update.mode", Content: []byte(`"none"`)},
		}
	}
	before := capture(t, f, f.save())
	f.mustImport()
	key := targets.Key(settings, "workbench.colorTheme")
	if !strings.Contains(read(t, settings), "Rosé Pine Moon") {
		t.Fatalf("the switch must change the field: %s", read(t, settings))
	}
	released := &held{f.current()}
	if err := f.writer().Release(context.Background(), key, released, targets.Preserve); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, settings), "Personal") {
		t.Fatalf("the release must restore the baseline of the field: %s", read(t, settings))
	}
	editState(t, f, func(document map[string]any) {
		record := document["targets"].(map[string]any)[key].(map[string]any)
		record["release"], record["next"] = true, record["before"]
	})
	mustRestore(t, f)
	before.restored(t, f)
}

func TestRestoreNeverReplacesAnEditMadeAfterTheReview(t *testing.T) {
	t.Cleanup(func() { v2import.SetFault(nil) })
	f := linuxFixture(t)
	f.save()
	f.mustImport()
	env := filepath.Join(f.state, "shells", "env.sh")
	write(t, env, "# v3 env\n# reviewed edit\n", 0644)
	restoration, err := v2import.PreviewRestore(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	const later = "# v3 env\n# edit after the review\n"
	v2import.SetFault(func(at string) error {
		if at == "restore-plan" {
			write(t, env, later, 0644)
		}
		return nil
	})
	if _, err := restoration.Run(context.Background(), []string{env}); err == nil || !strings.Contains(err.Error(), "the files changed after the review; run restore-v2 again") {
		t.Fatalf("an edit that the review did not show must stop the restore: %v", err)
	}
	if st := f.current(); st.V2Restore != "open" || read(t, env) != later {
		t.Fatalf("the restore replaced an edit that the review did not show: %+v %q", st, read(t, env))
	}
}

func TestRestoreRefusesAnotherV2StateInTheRestoreSet(t *testing.T) {
	f := linuxFixture(t)
	f.save()
	f.mustImport()
	f.v1.Generation++
	data, err := json.MarshalIndent(f.v1, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.state, "v2-restore", "state.json"), string(data)+"\n", 0600)
	published := read(t, filepath.Join(f.state, "state.json"))
	if _, err := v2import.PreviewRestore(context.Background(), f.options()); err == nil || !strings.Contains(err.Error(), "differs from the state that the switch saved") {
		t.Fatalf("the review must refuse a saved state that the switch did not save: %v", err)
	}
	if read(t, filepath.Join(f.state, "state.json")) != published {
		t.Fatal("the refusal must change nothing")
	}
}

func TestRestoreAcceptsAnAbsenceAfterAnInterruptedSwitchOnlyWithItsEvidence(t *testing.T) {
	for _, evidence := range []string{"none", "staged", "aside"} {
		for _, replace := range []bool{false, true} {
			if evidence == "aside" && replace {
				continue
			}
			t.Run(fmt.Sprintf("%s/%v", evidence, replace), func(t *testing.T) {
				f := linuxFixture(t)
				before := capture(t, f, f.save())
				v2env := read(t, filepath.Join(f.state, "shells", "env.sh"))
				f.mustImport()
				env := filepath.Join(f.state, "shells", "env.sh")
				saved := ""
				editManifest(t, f, func(slots map[string]any) {
					saved = strings.TrimPrefix(slots[env].(map[string]any)["saved"].(string), "backup:")
				})
				editState(t, f, func(document map[string]any) {
					record := document["targets"].(map[string]any)[env].(map[string]any)
					record["applied"], record["next"] = editedFingerprint(t, saved), editedFingerprint(t, env)
				})
				staged := strings.TrimSuffix(aside(env), ".previous") + ".next"
				switch evidence {
				case "staged":
					err := os.Rename(env, staged)
					if err != nil {
						t.Fatal(err)
					}
				case "aside":
					if err := os.Link(saved, aside(env)); err != nil {
						t.Fatal(err)
					}
					fallthrough
				default:
					if err := os.Remove(env); err != nil {
						t.Fatal(err)
					}
				}
				restoration, err := v2import.PreviewRestore(context.Background(), f.options())
				if err != nil {
					t.Fatal(err)
				}
				changed := restoration.Changed()
				if evidence == "aside" {
					if len(changed) != 0 {
						t.Fatalf("a target that the switch moved aside is not changed content: %+v", restoration.Items)
					}
					if _, err := restoration.Run(context.Background(), nil); err != nil {
						t.Fatal(err)
					}
					before.restored(t, f)
					return
				}
				if len(changed) != 1 || changed[0].Key != env || changed[0].Replacement != "restore" {
					t.Fatalf("an absence without evidence of the switch is your deletion: %+v", restoration.Items)
				}
				chosen := []string{}
				if replace {
					chosen = []string{env}
				}
				message, err := restoration.Run(context.Background(), chosen)
				if err != nil {
					t.Fatal(err)
				}
				if replace {
					if read(t, env) != v2env {
						t.Fatalf("an approved replacement must give back the value of release v2: %q", read(t, env))
					}
					return
				}
				if _, err := os.Lstat(env); !errors.Is(err, os.ErrNotExist) || !strings.Contains(message, "Kept your changed content: "+env) {
					t.Fatalf("without a decision the deletion must stay and be reported: %v %q", err, message)
				}
			})
		}
	}
}

func TestAnAlteredV2ConfigurationBaselineGivesNoOwnership(t *testing.T) {
	f := linuxFixture(t)
	f.save()
	tmux := filepath.Join(f.home, ".tmux.conf")
	backup := filepath.Join(f.home, ".dotfiles-config-"+operation("config.tmux")+"-0", "previous")
	path := filepath.Join(f.state, f.recovery("config.tmux"), "configuration.json")
	var document map[string]any
	if err := json.Unmarshal([]byte(read(t, path)), &document); err != nil {
		t.Fatal(err)
	}
	document["targets"].([]any)[0].(map[string]any)["original"] = map[string]any{"kind": "absent"}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(data)+"\n", 0600)
	mapping := mustMap(t, f)
	record := mapping.State.Targets[tmux]
	if record.Applied != "" || !strings.HasPrefix(record.Before, "fp:") || len(record.Made) != 0 {
		t.Fatalf("a unit whose saved original differs from its v2 receipt must be baseline-only: %+v", record)
	}
	if notes := strings.Join(mapping.Notes, "\n"); !strings.Contains(notes, tmux) || !strings.Contains(notes, backup) {
		t.Fatalf("the note must name the unit and the place of its earlier content: %v", mapping.Notes)
	}
	if read(t, backup) != "# my own tmux configuration\n" {
		t.Fatal("the mapping must leave the earlier content in place")
	}
	owned(t, mapping.State, filepath.Join(f.home, ".config", "nvim"), "config.nvim", "link", "absent", filepath.Join(f.home, ".config"))
}

type stopAfterWrite struct {
	targets.Kind
	path string
}

func (k stopAfterWrite) ObserveContent(d targets.Desired, data []byte) (string, error) {
	return k.Kind.(targets.ContentObserver).ObserveContent(d, data)
}

func (k stopAfterWrite) Write(ctx context.Context, d targets.Desired, from string) error {
	if err := k.Kind.Write(ctx, d, from); err != nil || d.Path != k.path {
		return err
	}
	return errors.New("stopped after the write of " + d.Path)
}

func TestRestoreRemovesTheFileAndFolderOfAFieldThatTheSwitchStoppedAfterWriting(t *testing.T) {
	f := linuxFixture(t)
	settings := filepath.Join(f.home, ".config", "v3-fields", "settings.json")
	field := targets.Desired{Kind: "field", Path: settings, Slot: "theme", Content: []byte(`"rose-pine"`)}
	f.units[shells] = func() []targets.Desired { return append(f.shellUnits(), field) }
	before := capture(t, f, f.save())
	proposal, err := f.preview()
	if err != nil {
		t.Fatal(err)
	}
	f.extraKinds = map[string]targets.Kind{"field": stopAfterWrite{hooks.ProfileKinds()["field"], settings}}
	if result, err := proposal.Run(context.Background()); err == nil {
		t.Fatalf("the fixture must stop the switch after the write: %s", result.Message)
	}
	f.extraKinds = nil
	if record := f.current().Targets[targets.Key(settings, "theme")]; record.Next == "" || record.Applied != "" {
		t.Fatalf("the switch must stop after it published the field and before it promoted it: %+v", record)
	}
	if !strings.Contains(read(t, settings), "rose-pine") {
		t.Fatal("the fixture must stop after the field is in the new file")
	}
	mustRestore(t, f)
	before.restored(t, f)
}

type registrationKind struct {
	fontKind
	registry string
	changes  *[]string
}

func (registrationKind) CannotCopy() bool { return true }

func (k registrationKind) Observe(targets.Desired) (string, error) {
	data, err := os.ReadFile(k.registry)
	if errors.Is(err, os.ErrNotExist) {
		return targets.Absent, nil
	}
	return string(data), err
}

func (k registrationKind) Fingerprint(d targets.Desired) (string, error) {
	return "font:" + string(d.Content), nil
}

func (k registrationKind) Write(_ context.Context, d targets.Desired, _ string) error {
	*k.changes = append(*k.changes, d.Key)
	return os.WriteFile(k.registry, []byte("font:"+string(d.Content)), 0600)
}

func TestTheSwitchLeavesANativeRegistrationWithoutACopyToSetupAfterTheSwitch(t *testing.T) {
	f := linuxFixture(t)
	t.Cleanup(v2import.SetRegistryFonts(true))
	f.save()
	registry, changes := filepath.Join(f.root, "registry"), []string{}
	write(t, registry, "font:registered", 0600)
	f.extraKinds = map[string]targets.Kind{"font": registrationKind{fontKind{targets.FileKind("font")}, registry, &changes}}
	files := f.units[hooks.FontResource]
	key := targets.Key(f.fontPath(), hooks.FontSlot)
	f.units[hooks.FontResource] = func() []targets.Desired {
		return append([]targets.Desired{{Kind: "font", Path: f.fontPath(), Slot: hooks.FontSlot, Content: []byte("v3")}}, files()...)
	}
	result := f.mustImport()
	if len(changes) != 0 || read(t, registry) != "font:registered" {
		t.Fatalf("the switch changed a registration that restore-v2 cannot give back: %v %q", changes, read(t, registry))
	}
	if result.Status != "needs-action" || !strings.Contains(result.Message, "Hack Nerd Font (tool.font): ") ||
		!strings.Contains(result.Message, "Finish this with the v2.3.0 checkout, or run restore-v2; setup repairs it after the switch") {
		t.Fatalf("the report must name the refused step and the way forward:\n%+v", result)
	}
	if st := f.current(); st.V2Restore != "open" || st.Targets[key].Applied != "font:registered" {
		t.Fatalf("the switch must complete and keep the registration record: %+v", st)
	}
	e := f.engine()
	plan, err := e.Preview(context.Background(), engine.Request{Mode: "apply"})
	if err != nil {
		t.Fatal(err)
	}
	if after, err := e.Apply(context.Background(), engine.Request{Mode: "apply"}, plan); err != nil {
		t.Fatalf("a run after the switch must repair the font: %+v %v", after, err)
	}
	if len(changes) != 1 || changes[0] != key || read(t, registry) != "font:v3" || f.current().V2Restore != "" {
		t.Fatalf("the run after the switch did not repair the registration: %v %q %+v", changes, read(t, registry), f.current())
	}
}

func TestTheSwitchRepairsAFontRegistrationThatFollowsItsFiles(t *testing.T) {
	f := linuxFixture(t)
	f.save()
	registry, changes := filepath.Join(f.root, "registry"), []string{}
	write(t, registry, "font:registered", 0600)
	f.extraKinds = map[string]targets.Kind{"font": registrationKind{fontKind{targets.FileKind("font")}, registry, &changes}}
	files := f.units[hooks.FontResource]
	key := targets.Key(f.fontPath(), hooks.FontSlot)
	f.units[hooks.FontResource] = func() []targets.Desired {
		return append([]targets.Desired{{Kind: "font", Path: f.fontPath(), Slot: hooks.FontSlot, Content: []byte("v3")}}, files()...)
	}
	f.mustImport()
	if len(changes) != 1 || changes[0] != key || read(t, registry) != "font:v3" || f.current().V2Restore != "open" {
		t.Fatalf("on Linux and macOS the font files carry the registration, so the switch repairs it: %v %q %+v", changes, read(t, registry), f.current())
	}
}

func TestTheSwitchNeverCreatesAWindowsFontRegistration(t *testing.T) {
	f := linuxFixture(t)
	t.Cleanup(v2import.SetRegistryFonts(true))
	f.save()
	registry, changes := filepath.Join(f.root, "registry"), []string{}
	f.extraKinds = map[string]targets.Kind{"font": registrationKind{fontKind{targets.FileKind("font")}, registry, &changes}}
	files := f.units[hooks.FontResource]
	f.units[hooks.FontResource] = func() []targets.Desired {
		return append([]targets.Desired{{Kind: "font", Path: f.fontPath(), Slot: hooks.FontSlot, Content: []byte("v3")}}, files()...)
	}
	result := f.mustImport()
	if _, err := os.Stat(registry); len(changes) != 0 || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the switch created a registration that restore-v2 cannot take back: %v %v", changes, err)
	}
	if result.Status != "needs-action" || f.current().V2Restore != "open" {
		t.Fatalf("the switch must complete and leave the registration to the next run: %+v %+v", result, f.current())
	}
}
