package targets_test

import (
	"errors"
	"os"
	"testing"

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
