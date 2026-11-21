package targets_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

func TestRejectedWriteNeverGivesOwnershipOnTheNextRun(t *testing.T) {
	for name, values := range ownerFixtures() {
		for _, start := range []string{"absent", "personal"} {
			for _, point := range []string{"next", "recheck"} {
				t.Run(name+"/"+start+"/"+point, func(t *testing.T) {
					e := newEnv(t)
					d := fixtureNamed(name).first(e)
					choice := targets.Preserve
					if start == "personal" {
						ownerWrites(e, d, values[0])
						choice = targets.Replace
					}
					equal := ""
					targets.SetFault(func(current string) error {
						if current == point && equal == "" {
							ownerWrites(e, d, values[1])
							equal = describe(t, e.root)
						}
						return nil
					})
					if err := e.converge(d, choice); !errors.Is(err, targets.ErrChanged) || describe(t, e.root) != equal {
						t.Fatal("a change after the approval must stop the write", err)
					}
					targets.SetFault(nil)
					if record := e.State().Targets[targets.Key(d.Path, d.Slot)]; record.Next != "" || record.Applied != "" {
						t.Fatalf("the rejected write left its intended value in the record: %+v", record)
					}
					if err := e.converge(d, targets.Preserve); err != nil {
						t.Fatal(err)
					}
					record, has := e.State().Targets[targets.Key(d.Path, d.Slot)]
					if !has || record.Applied != "" || record.Next != "" {
						t.Fatalf("equal content that an editor wrote must not be owned: %+v", record)
					}
					if live, err := e.writer.Inspect(d, e); err != nil || !live.Satisfied {
						t.Fatal(live, err)
					}
					e.mustRelease(d, targets.Preserve)
					if describe(t, e.root) != equal {
						t.Fatal("the release changed content that setup did not write")
					}
				})
			}
		}
	}
}

func TestInterruptedApprovedReplacementNeverLeavesTheDestinationAbsent(t *testing.T) {
	for _, f := range fixtures() {
		for _, removal := range []bool{false, true} {
			t.Run(f.name, func(t *testing.T) {
				e := newEnv(t)
				if f.outside != nil {
					f.outside(e)
				}
				d := f.first(e)
				e.mustConverge(d, targets.Preserve)
				f.intruder(e)
				changed := describe(t, e.root)
				change := func(c targets.Choice) error {
					if removal {
						return e.release(d, c)
					}
					return e.converge(f.second(e), c)
				}
				interruptAt("move")
				if err := change(targets.Replace); !errors.Is(err, errFault) {
					t.Fatal(err)
				}
				targets.SetFault(nil)
				if _, err := os.Lstat(d.Path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("the fixture needs an absent destination after the interruption", err)
				}
				if err := change(targets.Preserve); !errors.Is(err, targets.ErrChanged) || describe(t, e.root) != changed {
					t.Fatalf("the next run must give the changed content back and keep it: %v\n%s", err, describe(t, e.root))
				}
				if err := change(targets.Replace); err != nil {
					t.Fatal("the next approved run must complete", err)
				}
				if !holdsIntruder(e) {
					t.Fatal("the replaced content has no copy")
				}
			})
		}
	}
}

func TestInterruptedPublicationKeepsAnEditedPreviousFile(t *testing.T) {
	edits := map[string][2]string{"block": {"export A=1", "export A=9"}, "field": {`"rose-pine"`, `"mine"`}}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			f := fixtureNamed(name)
			f.outside(e)
			d := f.first(e)
			e.mustConverge(d, targets.Preserve)
			_, aside := targets.Siblings(d)
			edited := ""
			targets.SetFault(func(point string) error {
				if point != "publish" || edited != "" {
					return nil
				}
				edited = strings.Replace(read(t, aside), edit[0], edit[1], 1)
				if !strings.Contains(edited, edit[1]) {
					t.Fatal("the fixture needs an edit inside the managed part", edited)
				}
				write(t, aside, edited, 0640)
				return errFault
			})
			if err := e.converge(f.second(e), targets.Preserve); !errors.Is(err, errFault) {
				t.Fatal(err)
			}
			targets.SetFault(nil)
			published := read(t, d.Path)
			err := e.converge(f.second(e), targets.Preserve)
			if err == nil || !strings.Contains(err.Error(), aside) {
				t.Fatal("the next run must keep the edited previous file and name it", err)
			}
			if _, err := os.Lstat(aside); err != nil || read(t, aside) != edited || read(t, d.Path) != published {
				t.Fatal("the edit in the previous file was lost", err)
			}
		})
	}
}

func TestPublicationKeepsAPreviousFileThatChangedAfterItsCheck(t *testing.T) {
	edits := map[string][2]string{"block": {"export A=1", "export A=9"}, "field": {`"rose-pine"`, `"mine"`}, "file": {"version one", "version mine"}}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			f := fixtureNamed(name)
			if f.outside != nil {
				f.outside(e)
			}
			e.mustConverge(f.first(e), targets.Preserve)
			d := f.second(e)
			_, aside := targets.Siblings(d)
			edited := ""
			targets.SetFault(func(point string) error {
				if point != "move" || edited != "" {
					return nil
				}
				edited = strings.Replace(read(t, aside), edit[0], edit[1], 1)
				if !strings.Contains(edited, edit[1]) {
					t.Fatal("the fixture needs an edit of the previous file", edited)
				}
				write(t, aside, edited, 0640)
				return nil
			})
			var output bytes.Buffer
			if err := e.writer.Converge(platform.WithOutput(t.Context(), &output), d, e, targets.Preserve); err != nil {
				t.Fatal("the destination is correct, so the run must complete", err)
			}
			targets.SetFault(nil)
			e.satisfied(d)
			if _, err := os.Lstat(aside); err != nil || read(t, aside) != edited {
				t.Fatal("the edit in the previous file was lost", err)
			}
			if !strings.Contains(output.String(), aside) {
				t.Fatalf("the run must name the kept previous file: %q", output.String())
			}
		})
	}
}

func TestRecoveryKeepsAPreviousEntryThatOnlyAnotherBackupMatches(t *testing.T) {
	others := map[string]struct{ path, content, part string }{
		"file":  {filepath.Join("app", "other.conf"), "kept elsewhere\n", ""},
		"field": {filepath.Join("app", "other.json"), `{"theme":"kept"}`, `"kept"`},
	}
	for name, other := range others {
		for _, pending := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				e := newEnv(t)
				f := fixtureNamed(name)
				if f.outside != nil {
					f.outside(e)
				}
				d := f.first(e)
				e.mustConverge(d, targets.Preserve)
				elsewhere := d
				elsewhere.Path = filepath.Join(e.root, other.path)
				e.mustConverge(elsewhere, targets.Preserve)
				write(t, elsewhere.Path, other.content, 0644)
				e.mustConverge(elsewhere, targets.Replace)
				if !holds(e, "kept") {
					t.Fatal("the fixture needs a backup of the other target", e.baselines())
				}
				if pending {
					interruptAt("next")
					if err := e.converge(f.second(e), targets.Preserve); !errors.Is(err, errFault) {
						t.Fatal(err)
					}
					targets.SetFault(nil)
				}
				_, aside := targets.Siblings(d)
				content := other.content
				if other.part != "" {
					content = strings.Replace(read(t, d.Path), `"rose-pine"`, other.part, 1)
				}
				write(t, aside, content, 0644)
				before := describe(t, e.root)
				err := e.converge(d, targets.Preserve)
				if pending != (err != nil) || pending && !strings.Contains(err.Error(), aside) {
					t.Fatal("recovery must keep the entry, and name it for an unfinished write", err)
				}
				if describe(t, e.root) != before {
					t.Fatal("recovery deleted an entry that the backup of another target matches")
				}
			})
		}
	}
}

func TestRecoveryCompletesAnApprovedReplacementWithItsOwnBackup(t *testing.T) {
	for _, name := range []string{"block", "field"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			f := fixtureNamed(name)
			f.outside(e)
			e.mustConverge(f.first(e), targets.Preserve)
			f.intruder(e)
			interruptAt("publish")
			if err := e.converge(f.second(e), targets.Replace); !errors.Is(err, errFault) {
				t.Fatal(err)
			}
			targets.SetFault(nil)
			d := f.second(e)
			_, aside := targets.Siblings(d)
			if _, err := os.Lstat(aside); err != nil {
				t.Fatal("the fixture needs a previous entry", err)
			}
			e.mustConverge(d, targets.Preserve)
			if _, err := os.Lstat(aside); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("recovery kept the previous entry of its own approved write", err)
			}
			if !holdsIntruder(e) {
				t.Fatal("the replaced content has no copy", e.baselines())
			}
		})
	}
}
