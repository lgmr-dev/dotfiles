package targets_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/targets"
)

func readOnlyParent(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("the fixture needs a folder that refuses a new entry")
	}
	parent := filepath.Dir(path)
	if err := os.Chmod(parent, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
}

func TestFailedWriteWithAnUnchangedDestinationLeavesTheRecordAsItWas(t *testing.T) {
	for _, name := range []string{"file", "link", "block", "field"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			f := fixtureNamed(name)
			d := f.first(e)
			e.mustConverge(d, targets.Preserve)
			if err := os.RemoveAll(d.Path); err != nil {
				t.Fatal(err)
			}
			before := e.State()
			readOnlyParent(t, d.Path)
			tree := describe(t, e.root)
			err := e.converge(d, targets.Preserve)
			if err == nil || errors.Is(err, targets.ErrChanged) {
				t.Fatal("the fixture needs a write that fails", err)
			}
			if after := e.State(); !reflect.DeepEqual(after, before) {
				t.Fatalf("a failed write that changed nothing changed the state:\n%+v\nwant:\n%+v", after.Targets, before.Targets)
			}
			if describe(t, e.root) != tree {
				t.Fatal("a failed write left an entry")
			}
			if err := os.Chmod(filepath.Dir(d.Path), 0700); err != nil {
				t.Fatal(err)
			}
			e.mustConverge(d, targets.Preserve)
		})
	}
}

func TestFailedFirstWriteWithAnUnchangedDestinationLeavesNoRecord(t *testing.T) {
	for _, name := range []string{"file", "link", "block", "field"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			d := fixtureNamed(name).first(e)
			if err := os.MkdirAll(filepath.Dir(d.Path), 0700); err != nil {
				t.Fatal(err)
			}
			readOnlyParent(t, d.Path)
			tree := describe(t, e.root)
			if err := e.converge(d, targets.Preserve); err == nil {
				t.Fatal("the fixture needs a write that fails")
			}
			if after := e.State(); len(after.Targets) != 0 || len(e.baselines()) != 0 || describe(t, e.root) != tree {
				t.Fatalf("a failed first write that changed nothing left a record or an entry: %+v", after.Targets)
			}
		})
	}
}

func TestFailedReleaseWithAnUnchangedDestinationLeavesTheRecordAsItWas(t *testing.T) {
	for _, name := range []string{"file", "link"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			d := fixtureNamed(name).first(e)
			e.mustConverge(d, targets.Preserve)
			before := e.State()
			readOnlyParent(t, d.Path)
			tree := describe(t, e.root)
			if err := e.release(d, targets.Preserve); err == nil {
				t.Fatal("the fixture needs a release that fails")
			}
			if after := e.State(); !reflect.DeepEqual(after, before) || describe(t, e.root) != tree {
				t.Fatalf("a failed release that changed nothing changed the state: %+v", after.Targets)
			}
		})
	}
}

func TestFailedWriteAfterADestinationChangeKeepsItsIntendedValue(t *testing.T) {
	for _, f := range fixtures() {
		t.Run(f.name, func(t *testing.T) {
			e := newEnv(t)
			if f.outside != nil {
				f.outside(e)
			}
			e.mustConverge(f.first(e), targets.Preserve)
			d := f.second(e)
			interruptAt("move")
			if err := e.converge(d, targets.Preserve); !errors.Is(err, errFault) {
				t.Fatal(err)
			}
			targets.SetFault(nil)
			if record := e.State().Targets[targets.Key(d.Path, d.Slot)]; record.Next == "" {
				t.Fatalf("recovery needs the intended value after the destination changed: %+v", record)
			}
			e.mustConverge(d, targets.Preserve)
		})
	}
}
