package targets_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/builds"
	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/targets"
	"github.com/luisgui1757/dotfiles/installer/tools"
)

var terminalPersonal = map[string]string{
	"owned-overlap": `// personal header
{
	"defaultProfile": "{personal}",
	"initialRows": 30,
	"profiles": {"defaults": {"font": {"features": {"ss01": 1}}}, "list": [{"guid": "{personal}", "name": "Personal"}]},
	"personal": 9007199254740993,
}
`,
	"empty-containers": `// personal header
{
	"defaultProfile": "{personal}",
	"profiles": {"defaults": {"font": {}}, "list": []},
	"schemes": [],
	"personal": 9007199254740993,
}
`,
}

func terminalProfiles(t *testing.T, e *env) (*hooks.Profiles, string) {
	t.Helper()
	loaded, err := catalog.Load(tools.FS)
	if err != nil {
		t.Fatal(err)
	}
	host := platform.Host{Context: platform.Context{OS: "windows", Arch: "amd64"}}
	pins, err := builds.HostPins(loaded.Pins, host)
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := filepath.Abs(filepath.Join("..", ".."))
	if err == nil {
		checkout, err = platform.ResolvePath(checkout)
	}
	if err != nil {
		t.Fatal(err)
	}
	e.writer.Kinds = hooks.ProfileKinds()
	local := filepath.Join(e.root, "local data")
	p := &hooks.Profiles{
		Catalog:  loaded,
		Context:  host.Context,
		Folders:  platform.Folders{Home: e.home, LocalAppData: local},
		Checkout: checkout,
		StateDir: e.writer.StateDir,
		Writer:   e.writer,
		Pins:     pins,
	}
	return p, filepath.Join(local, "Microsoft", "Windows Terminal", "settings.json")
}

func terminalChange(t *testing.T, e *env, p *hooks.Profiles, present, replace bool) error {
	t.Helper()
	r, ok := p.Catalog.Resource("integration.windows-terminal")
	if !ok {
		t.Fatal("the catalog has no Windows Terminal resource")
	}
	finding, err := p.Inspect(t.Context(), r, engine.Want{Present: present}, e)
	if err != nil {
		return err
	}
	finding.Replace = replace
	if present {
		return p.Apply(t.Context(), r, finding, e)
	}
	return p.Remove(t.Context(), r, finding, e)
}

func TestInterruptedWindowsTerminalChangeCompletesOnTheNextRun(t *testing.T) {
	for _, c := range []struct {
		original, point string
		release         bool
	}{{"owned-overlap", "move", false}, {"owned-overlap", "write", false}, {"owned-overlap", "move", true}, {"empty-containers", "move", true}} {
		name := "write"
		if c.release {
			name = "release"
		}
		t.Run(name+"/"+c.point+"/"+c.original, func(t *testing.T) {
			e := newEnv(t)
			p, path := terminalProfiles(t, e)
			original := terminalPersonal[c.original]
			write(t, path, original, 0600)
			if c.release {
				if err := terminalChange(t, e, p, true, true); err != nil {
					t.Fatal(err)
				}
			}
			interruptAt(c.point)
			if err := terminalChange(t, e, p, !c.release, true); !errors.Is(err, errFault) {
				t.Fatal(err)
			}
			targets.SetFault(nil)
			if err := terminalChange(t, e, p, !c.release, true); err != nil {
				t.Fatal("the next run must complete the interrupted change", err)
			}
			data := read(t, path)
			if !c.release {
				for _, kept := range []string{"// personal header", `"defaultProfile": "{personal}"`, `"ss01": 1`, `"name": "Personal"`, "9007199254740993", `"initialRows": 50`} {
					if !strings.Contains(data, kept) {
						t.Fatal("the completed write lost a personal setting", kept, data)
					}
				}
				if !strings.Contains(data, `"historySize":32767`) {
					t.Fatal("the completed write has no managed settings", data)
				}
				if err := terminalChange(t, e, p, false, false); err != nil {
					t.Fatal(err)
				}
				data = read(t, path)
			}
			if data != original {
				t.Fatalf("the release did not give back the personal settings: %q", data)
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil || len(entries) != 1 {
				t.Fatal("the recovery left other files next to the settings", entries, err)
			}
			if _, has := e.State().Targets[targets.Key(path, hooks.TerminalSlot)]; has {
				t.Fatal("the release left the record")
			}
		})
	}
}
