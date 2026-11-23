package targets_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/targets"
)

func sharedParent(e *env) []targets.Desired {
	config := filepath.Join(e.root, ".config")
	return []targets.Desired{
		{Resource: "config.one", Kind: "file", Path: filepath.Join(config, "a", "x"), Content: []byte("x\n")},
		{Resource: "config.two", Kind: "file", Path: filepath.Join(config, "b", "y"), Content: []byte("y\n")},
		{Resource: "config.three", Kind: "link", Path: filepath.Join(config, "z"), Link: source(e, "one", "linked\n")},
	}
}

func orders() map[string]func([]targets.Desired) []targets.Desired {
	return map[string]func([]targets.Desired) []targets.Desired{
		"install order": func(units []targets.Desired) []targets.Desired { return units },
		"reverse order": func(units []targets.Desired) []targets.Desired {
			reversed := slices.Clone(units)
			slices.Reverse(reversed)
			return reversed
		},
		"middle first": func(units []targets.Desired) []targets.Desired {
			return []targets.Desired{units[1], units[0], units[2]}
		},
	}
}

func TestSharedCreatedFolderGoesWithTheLastRelease(t *testing.T) {
	for name, order := range orders() {
		for _, interrupted := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				e := newEnv(t)
				units := sharedParent(e)
				for _, d := range units {
					e.mustConverge(d, targets.Preserve)
				}
				listed := 0
				for _, record := range e.State().Targets {
					if slices.Contains(record.Made, filepath.Join(e.root, ".config")) {
						listed++
					}
				}
				if listed != 1 {
					t.Fatal("a created folder must be in exactly one record", listed)
				}
				for i, d := range order(units) {
					if interrupted {
						interruptAt("made")
						if err := e.release(d, targets.Preserve); !errors.Is(err, errFault) {
							t.Fatal(err)
						}
						targets.SetFault(nil)
					}
					e.mustRelease(d, targets.Preserve)
					if _, err := os.Lstat(filepath.Join(e.root, ".config")); i < len(units)-1 && err != nil {
						t.Fatal("a folder that holds another target was removed", err)
					}
				}
				if _, err := os.Lstat(e.root); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("the folders that setup created stayed:\n%s", describe(t, e.root))
				}
			})
		}
	}
}

func TestSharedCreatedFolderStaysWithPersonalContent(t *testing.T) {
	for name, order := range orders() {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			units := sharedParent(e)
			for _, d := range units {
				e.mustConverge(d, targets.Preserve)
			}
			personal := filepath.Join(e.root, ".config", "personal.txt")
			write(t, personal, "keep", 0600)
			for _, d := range order(units) {
				e.mustRelease(d, targets.Preserve)
			}
			entries, err := os.ReadDir(filepath.Join(e.root, ".config"))
			if err != nil || len(entries) != 1 || read(t, personal) != "keep" {
				t.Fatalf("the folder of a personal file must stay with that file only:\n%s", describe(t, e.root))
			}
		})
	}
}

func TestFolderThatExistedBeforeSetupIsNeverRemoved(t *testing.T) {
	for name, order := range orders() {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			config := filepath.Join(e.root, ".config")
			if err := os.MkdirAll(filepath.Join(config, "b"), 0755); err != nil {
				t.Fatal(err)
			}
			origin := describe(t, e.root)
			units := sharedParent(e)
			for _, d := range units {
				e.mustConverge(d, targets.Preserve)
			}
			for _, d := range order(units) {
				e.mustRelease(d, targets.Preserve)
			}
			if after := describe(t, e.root); after != origin {
				t.Fatalf("the release changed folders that existed before setup:\n%s\nwant:\n%s", after, origin)
			}
		})
	}
}

func TestConfigurationOfTwoResourcesRemovesTheSharedCreatedFolder(t *testing.T) {
	for _, first := range []string{"config.one", "config.two"} {
		t.Run(first, func(t *testing.T) {
			e := newEnv(t)
			mode := ""
			if runtime.GOOS != "windows" {
				mode = "copy"
			}
			tool := catalog.Tool{Schema: 1,
				Resources: []catalog.Resource{{ID: "config.one", Name: "One", Action: "config", Scope: "user"}, {ID: "config.two", Name: "Two", Action: "config", Scope: "user"}},
				Targets: []catalog.Target{
					{Resource: "config.one", Source: "one/config", Folder: "config", Path: "one/config", Mode: mode},
					{Resource: "config.two", Source: "two/config", Folder: "config", Path: "two/config", Mode: mode},
					{Resource: "config.two", Source: "two/other", Folder: "home", Path: ".two"},
				}}
			data, err := json.Marshal(tool)
			noError(t, err)
			loaded, err := catalog.Load(fstest.MapFS{"fixture/tool.json": &fstest.MapFile{Data: data}})
			noError(t, err)
			checkout := filepath.Join(e.home, "checkout")
			for _, name := range []string{"one/config", "two/config", "two/other"} {
				write(t, filepath.Join(checkout, filepath.FromSlash(name)), name, 0644)
			}
			provider := &targets.ConfigProvider{Catalog: loaded, Context: platform.Context{OS: runtime.GOOS, Arch: runtime.GOARCH},
				Folders: platform.Folders{Home: e.root, Config: filepath.Join(e.root, ".config")}, Checkout: checkout, Writer: e.writer}
			ids := []string{"config.one", "config.two"}
			for _, id := range ids {
				r, _ := loaded.Resource(id)
				finding, err := provider.Inspect(t.Context(), r, engine.Want{Present: true}, e)
				noError(t, err)
				noError(t, provider.Apply(t.Context(), r, finding, e))
			}
			if first == "config.two" {
				slices.Reverse(ids)
			}
			for _, id := range ids {
				r, _ := loaded.Resource(id)
				finding, err := provider.Inspect(t.Context(), r, engine.Want{}, e)
				noError(t, err)
				noError(t, provider.Remove(t.Context(), r, finding, e))
			}
			if _, err := os.Lstat(e.root); !errors.Is(err, os.ErrNotExist) || len(e.State().Targets) != 0 {
				t.Fatalf("the removal of both resources left folders that setup created:\n%s", describe(t, e.root))
			}
		})
	}
}
