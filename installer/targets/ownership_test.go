package targets_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

func ownerWrites(e *env, d targets.Desired, value string) {
	e.t.Helper()
	if d.Kind == "field" {
		write(e.t, d.Path, `{"model":"personal","theme":`+value+`}`, 0640)
		return
	}
	if err := os.RemoveAll(d.Path); err != nil {
		e.t.Fatal(err)
	}
	write(e.t, d.Path, value, 0644)
}

func ownerFixtures() map[string][3]string {
	return map[string][3]string{
		"file":  {"earlier content\n", "version one\n", "version two\n"},
		"field": {`"earlier"`, `"rose-pine"`, `"rose-pine-moon"`},
	}
}

func fixtureNamed(name string) fixture {
	for _, f := range fixtures() {
		if f.name == name {
			return f
		}
	}
	panic(name)
}

func (e *env) unowned(d targets.Desired) state.Record {
	e.t.Helper()
	record, has := e.State().Targets[targets.Key(d.Path, d.Slot)]
	if !has || record.Applied != "" || record.Next != "" || record.Release || record.Before == "" || record.Before == targets.Absent || len(record.Made) != 0 {
		e.t.Fatalf("content that setup did not write must have a record with a baseline only: %+v", record)
	}
	return record
}

func TestEqualContentWithoutARecordStaysUnowned(t *testing.T) {
	for name, values := range ownerFixtures() {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			f := fixtureNamed(name)
			d := f.first(e)
			ownerWrites(e, d, values[1])
			equal := describe(t, e.root)
			if err := e.converge(d, targets.Preserve); err != nil {
				t.Fatal(err)
			}
			e.unowned(d)
			if err := e.converge(d, targets.Preserve); err != nil {
				t.Fatal(err)
			}
			e.unowned(d)
			later := f.second(e)
			if live, err := e.writer.Inspect(later, e); err != nil || live.State != targets.StateChanged {
				t.Fatal("a later change against unowned content must be changed", live, err)
			}
			if err := e.converge(later, targets.Preserve); !errors.Is(err, targets.ErrChanged) || describe(t, e.root) != equal {
				t.Fatal("unowned content was replaced without approval", err)
			}
			e.mustRelease(d, targets.Preserve)
			if describe(t, e.root) != equal || len(e.baselines()) != 0 {
				t.Fatal("the release changed content that setup did not write")
			}
		})
	}
}

func TestEqualContentAgainstABaselineOnlyRecordStaysUnowned(t *testing.T) {
	for name, values := range ownerFixtures() {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			d := fixtureNamed(name).first(e)
			ownerWrites(e, d, values[0])
			interruptAt("baseline")
			if err := e.converge(d, targets.Replace); !errors.Is(err, errFault) {
				t.Fatal(err)
			}
			targets.SetFault(nil)
			earlier := e.unowned(d).Before
			ownerWrites(e, d, values[1])
			equal := describe(t, e.root)
			if err := e.converge(d, targets.Preserve); err != nil {
				t.Fatal(err)
			}
			if record := e.unowned(d); record.Before == earlier {
				t.Fatal("the record kept a baseline that is not the live content", record)
			}
			e.mustRelease(d, targets.Preserve)
			if describe(t, e.root) != equal {
				t.Fatal("the release restored an old baseline over content that setup did not write")
			}
		})
	}
}

func TestOwnerEditThatEqualsANewDesiredContentStaysUnowned(t *testing.T) {
	for name, values := range ownerFixtures() {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			f := fixtureNamed(name)
			d := f.first(e)
			ownerWrites(e, d, values[0])
			e.mustConverge(d, targets.Replace)
			ownerWrites(e, d, values[2])
			equal := describe(t, e.root)
			later := f.second(e)
			if err := e.converge(later, targets.Preserve); err != nil {
				t.Fatal(err)
			}
			e.unowned(later)
			e.mustRelease(later, targets.Preserve)
			if describe(t, e.root) != equal {
				t.Fatal("the release restored the first baseline over the edit of the owner")
			}
		})
	}
}

func TestStagingNeverDeletesAnEntryThatTheRecordCannotVerify(t *testing.T) {
	for _, name := range []string{"file", "tree", "link", "block", "field"} {
		for _, pending := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				e := newEnv(t)
				f := fixtureNamed(name)
				if f.outside != nil {
					f.outside(e)
				}
				d := f.first(e)
				if pending {
					interruptAt("next")
					if err := e.converge(d, targets.Preserve); !errors.Is(err, errFault) {
						t.Fatal(err)
					}
					targets.SetFault(nil)
				}
				staged, _ := targets.Siblings(d)
				write(t, staged, "personal file with a reserved name\n", 0600)
				before := describe(t, e.root)
				if err := e.converge(d, targets.Preserve); err == nil || !strings.Contains(err.Error(), staged) {
					t.Fatal("the write must stop and name the entry", err)
				}
				if describe(t, e.root) != before {
					t.Fatal("the write deleted or changed an entry that it did not stage")
				}
				if err := os.Remove(staged); err != nil {
					t.Fatal(err)
				}
				e.mustConverge(d, targets.Preserve)
			})
		}
	}
}

func TestRecoveryNeverMovesAnUnverifiedEntryToTheDestination(t *testing.T) {
	for _, name := range []string{"file", "block"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			f := fixtureNamed(name)
			d := f.first(e)
			e.mustConverge(d, targets.Preserve)
			if err := os.Remove(d.Path); err != nil {
				t.Fatal(err)
			}
			_, aside := targets.Siblings(d)
			write(t, aside, "personal file with a reserved name\n", 0600)
			before := describe(t, e.root)
			for _, change := range []func() error{func() error { return e.converge(d, targets.Preserve) }, func() error { return e.release(d, targets.Preserve) }} {
				if err := change(); err == nil || !strings.Contains(err.Error(), aside) || describe(t, e.root) != before {
					t.Fatal("recovery moved or deleted an entry that the record cannot verify", err)
				}
			}
		})
	}
}

type brokenSave struct{ targets.Kind }

func (brokenSave) Save(context.Context, targets.Desired, string) (string, error) {
	return "", errors.New("no space left on device")
}

func TestCaptureReportsAFailedCopy(t *testing.T) {
	e := newEnv(t)
	file := filepath.Join(e.root, "settings.toml")
	write(t, file, "personal = true\n", 0640)
	e.writer.Kinds = map[string]targets.Kind{"file": brokenSave{targets.FileKind("file")}}
	saved, _, err := e.writer.Capture(t.Context(), file, state.Record{Resource: "config.tool", Kind: "file"}, filepath.Join(e.writer.StateDir, "saved"))
	if err == nil || !strings.Contains(err.Error(), "no space left on device") || saved != "" {
		t.Fatalf("a failed copy must stop the capture: %q %v", saved, err)
	}
}
