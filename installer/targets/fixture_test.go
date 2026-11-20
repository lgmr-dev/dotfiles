package targets_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

type env struct {
	t      *testing.T
	home   string
	root   string
	store  *state.Store
	writer *targets.Writer
}

func (e *env) State() state.State                         { return e.store.View() }
func (e *env) Update(edit func(*state.State) error) error { return e.store.Update(edit) }

func newEnv(t *testing.T) *env {
	t.Helper()
	home, err := platform.ResolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(home, "state")
	store, err := state.Open(directory, runtime.GOOS+"/"+runtime.GOARCH+":"+home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		targets.SetFault(nil)
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return &env{t: t, home: home, root: filepath.Join(home, "cfg"), store: store, writer: &targets.Writer{StateDir: directory}}
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func describe(t *testing.T, root string) string {
	t.Helper()
	lines := []string{}
	var walk func(path, relative string)
	walk = func(path, relative string) {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			lines = append(lines, relative+"|absent")
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		mode := info.Mode().Perm()
		if runtime.GOOS == "windows" {
			mode = 0
		}
		switch {
		case info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0:
			target, err := os.Readlink(path)
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, relative+"|link|"+target)
		case info.IsDir():
			lines = append(lines, fmt.Sprintf("%s|directory|%o", relative, mode))
			children, err := os.ReadDir(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, child := range children {
				walk(filepath.Join(path, child.Name()), relative+"/"+child.Name())
			}
		default:
			lines = append(lines, fmt.Sprintf("%s|file|%o|%q", relative, mode, read(t, path)))
		}
	}
	walk(root, ".")
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func (e *env) converge(d targets.Desired, c targets.Choice) error {
	return e.writer.Converge(e.t.Context(), d, e, c)
}

func (e *env) release(d targets.Desired, c targets.Choice) error {
	return e.writer.Release(e.t.Context(), targets.Key(d.Path, d.Slot), e, c)
}

func (e *env) mustConverge(d targets.Desired, c targets.Choice) {
	e.t.Helper()
	if err := e.converge(d, c); err != nil {
		e.t.Fatal(err)
	}
	e.satisfied(d)
}

func (e *env) mustRelease(d targets.Desired, c targets.Choice) {
	e.t.Helper()
	if err := e.release(d, c); err != nil {
		e.t.Fatal(err)
	}
	if record, has := e.State().Targets[targets.Key(d.Path, d.Slot)]; has {
		e.t.Fatal("the release left a record", record)
	}
}

func (e *env) satisfied(d targets.Desired) {
	e.t.Helper()
	live, err := e.writer.Inspect(d, e)
	if err != nil || live.State != targets.StateApplied || live.Fingerprint != live.Want {
		e.t.Fatalf("target is not satisfied: %+v %v", live, err)
	}
	record := e.State().Targets[targets.Key(d.Path, d.Slot)]
	if record.Applied != live.Want || record.Next != "" || record.Release || record.Before == "" {
		e.t.Fatalf("record is not complete: %+v", record)
	}
}

func (e *env) baselines() []string {
	e.t.Helper()
	entries, err := os.ReadDir(filepath.Join(e.writer.StateDir, "baselines"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		e.t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, filepath.Join(e.writer.StateDir, "baselines", entry.Name()))
	}
	return names
}

var errFault = errors.New("interrupted")

func interruptAt(point string) *[]string {
	seen := []string{}
	fired := false
	targets.SetFault(func(current string) error {
		seen = append(seen, current)
		if current == point && !fired {
			fired = true
			return errFault
		}
		return nil
	})
	return &seen
}

func record() *[]string { return interruptAt("") }
