package hooks_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/builds"
	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
	"github.com/luisgui1757/dotfiles/installer/tools"
)

type profileEnv struct {
	t        *testing.T
	home     string
	store    *state.Store
	p        *hooks.Profiles
	selected []string
}

func (e *profileEnv) State() state.State                         { return e.store.View() }
func (e *profileEnv) Update(edit func(*state.State) error) error { return e.store.Update(edit) }

func profileHost(system string) platform.Host {
	switch system {
	case "darwin":
		return platform.Host{Context: platform.Context{OS: "darwin", Arch: "arm64"}}
	case "windows":
		return platform.Host{Context: platform.Context{OS: "windows", Arch: "amd64"}}
	}
	return platform.Host{Context: platform.Context{OS: "linux", Arch: "amd64"}, Distro: "ubuntu", DistroRelease: "24.04", Libc: "glibc"}
}

func profileSystems(t *testing.T, systems ...string) []string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return systems
	}
	if !slices.Contains(systems, "windows") {
		t.Skip("a Windows volume is not a valid POSIX PATH entry: the POSIX shell profiles need a POSIX host")
	}
	return []string{"windows"}
}

func profileCheckout(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err == nil {
		root, err = platform.ResolvePath(root)
	}
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func newProfileEnv(t *testing.T, system string) *profileEnv {
	t.Helper()
	root, err := platform.ResolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newProfileEnvAt(t, system, filepath.Join(root, "home"))
}

func newProfileEnvAt(t *testing.T, system, home string) *profileEnv {
	t.Helper()
	return newProfileEnvIn(t, system, home, filepath.Join(filepath.Dir(home), "state"))
}

func newProfileEnvIn(t *testing.T, system, home, directory string) *profileEnv {
	t.Helper()
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	loaded, err := catalog.Load(tools.FS)
	if err != nil {
		t.Fatal(err)
	}
	host := profileHost(system)
	pins, err := builds.HostPins(loaded.Pins, host)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(directory, runtime.GOOS+"/"+runtime.GOARCH+":"+home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	e := &profileEnv{t: t, home: home, store: store}
	e.p = &hooks.Profiles{
		Catalog: loaded,
		Context: host.Context,
		Folders: platform.Folders{Home: home, Config: filepath.Join(home, ".config"), Documents: filepath.Join(home, "redirected documents"),
			LocalAppData: filepath.Join(home, "native local data"), AppData: filepath.Join(home, "roaming"), Zsh: home},
		Checkout: profileCheckout(t),
		StateDir: directory,
		Writer:   &targets.Writer{StateDir: directory, Kinds: hooks.ProfileKinds()},
		Pins:     pins,
		Wanted:   func() []string { return e.selected },
	}
	return e
}

func (e *profileEnv) resource(id string) catalog.Resource {
	e.t.Helper()
	r, ok := e.p.Catalog.Resource(id)
	if !ok {
		e.t.Fatal("unknown resource", id)
	}
	return r
}

func (e *profileEnv) inspect(id string, present bool) engine.Finding {
	e.t.Helper()
	finding, err := e.p.Inspect(e.t.Context(), e.resource(id), engine.Want{Present: present}, e)
	if err != nil {
		e.t.Fatal(err)
	}
	return finding
}

func (e *profileEnv) apply(id string, replace bool) error {
	e.t.Helper()
	finding := e.inspect(id, true)
	finding.Replace = replace
	return e.p.Apply(e.t.Context(), e.resource(id), finding, e)
}

func (e *profileEnv) mustApply(id string, replace bool) {
	e.t.Helper()
	if err := e.apply(id, replace); err != nil {
		e.t.Fatal(err)
	}
	if finding := e.inspect(id, true); finding.Status != engine.Satisfied {
		e.t.Fatalf("%s is not satisfied after its apply: %+v", id, finding)
	}
}

func (e *profileEnv) mustRemove(id string, replace bool) {
	e.t.Helper()
	finding := e.inspect(id, false)
	finding.Replace = replace
	if err := e.p.Remove(e.t.Context(), e.resource(id), finding, e); err != nil {
		e.t.Fatal(err)
	}
	if finding := e.inspect(id, false); finding.Status != engine.Absent {
		e.t.Fatalf("%s left a record after its removal: %+v", id, finding)
	}
}

func (e *profileEnv) units(id string) []targets.Desired {
	e.t.Helper()
	units, err := e.p.Desired(id, e)
	if err != nil {
		e.t.Fatal(err)
	}
	return units
}

func (e *profileEnv) unit(id, base string) targets.Desired {
	e.t.Helper()
	for _, unit := range e.units(id) {
		if filepath.Base(unit.Path) == base {
			return unit
		}
	}
	e.t.Fatal("no profile unit", id, base)
	return targets.Desired{}
}

func profileWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func profileReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func profileTree(t *testing.T, root string) string {
	t.Helper()
	lines := []string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			lines = append(lines, relative+"/")
			return nil
		}
		data, err := os.ReadFile(path)
		lines = append(lines, fmt.Sprintf("%s=%q", relative, data))
		return err
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
