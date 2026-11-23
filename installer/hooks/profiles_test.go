package hooks_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/hooks"
	"github.com/lgmr-dev/dotfiles/installer/targets"
)

func TestProfileResourcesAreTheProfileRoutes(t *testing.T) {
	e := newProfileEnv(t, "linux")
	for _, resource := range e.p.Catalog.Resources {
		want := resource.Action == "profile" || resource.ID == tmuxID || resource.ID == terminalID
		if hooks.ProfileResource(resource.ID) != want {
			t.Fatal("profile route", resource.ID, resource.Action)
		}
	}
}

func TestProfilesLifecycleKeepsPersonalContentAndRestoresTheBaseline(t *testing.T) {
	for _, system := range profileSystems(t, "linux", "darwin", "windows") {
		t.Run(system, func(t *testing.T) {
			e := newProfileEnv(t, system)
			e.selected = []string{"config.zsh", "config.powershell", "integration.lsd", "tool.claude"}
			personal := map[string]string{
				filepath.Join(e.home, ".zshrc"):  "export PERSONAL=zsh",
				filepath.Join(e.home, ".bashrc"): "export PERSONAL=bash\n",
			}
			if system == "windows" {
				personal = map[string]string{
					filepath.Join(e.p.Folders.Documents, "PowerShell", "profile.ps1"): "\xef\xbb\xbf$personal = 1\r\n",
				}
			}
			for path, content := range personal {
				profileWrite(t, path, content)
			}
			before := profileTree(t, e.home)
			if finding := e.inspect(shellsID, true); finding.Status != engine.Missing || finding.Token == "" {
				t.Fatalf("first inspection: %+v", finding)
			}
			if tree := profileTree(t, e.home); tree != before {
				t.Fatal("an inspection wrote to the home folder")
			}
			e.mustApply(shellsID, false)
			for _, unit := range e.units(shellsID) {
				data := profileReadFile(t, unit.Path)
				if unit.Kind == "file" {
					if data != string(unit.Content) || filepath.Dir(unit.Path) != filepath.Join(e.p.StateDir, "shells") {
						t.Fatal("generated init file", unit.Path)
					}
					continue
				}
				begin := "# >>> dotfiles:integration.shells >>>"
				if strings.Count(data, begin) != 1 || !strings.Contains(strings.ReplaceAll(data, "\r\n", "\n"), begin+"\n"+strings.TrimRight(string(unit.Content), "\n")+"\n# <<< dotfiles:integration.shells <<<\n") {
					t.Fatalf("loader block of %s: %q", unit.Path, data)
				}
				if original, ok := personal[unit.Path]; ok && !strings.HasPrefix(data, original) {
					t.Fatalf("the personal content of %s changed: %q", unit.Path, data)
				}
			}
			if system == "windows" {
				created := profileReadFile(t, filepath.Join(e.p.Folders.Documents, "WindowsPowerShell", "profile.ps1"))
				if !strings.HasPrefix(created, "\xef\xbb\xbf\n# >>> dotfiles:") {
					t.Fatalf("a new PowerShell profile needs a byte order mark: %q", created)
				}
				existing := profileReadFile(t, filepath.Join(e.p.Folders.Documents, "PowerShell", "profile.ps1"))
				if !strings.Contains(existing, "\r\n# >>> dotfiles:integration.shells >>>\r\n") {
					t.Fatalf("the block did not keep the line endings of the profile: %q", existing)
				}
			}

			edited := e.units(shellsID)[len(e.units(shellsID))-1].Path
			profileWrite(t, edited, "# personal header\n"+profileReadFile(t, edited)+"# personal footer\n")
			if finding := e.inspect(shellsID, true); finding.Status != engine.Satisfied {
				t.Fatalf("a personal edit outside the block changed the ownership: %+v", finding)
			}
			first := e.inspect(shellsID, true).Token
			e.selected = append(e.selected, "integration.zoxide")
			if finding := e.inspect(shellsID, true); finding.Status != engine.Unhealthy || finding.Token == first {
				t.Fatalf("a new selection needs a new generated file: %+v", finding)
			}
			e.mustApply(shellsID, false)
			if data := profileReadFile(t, edited); !strings.HasPrefix(data, "# personal header\n") || !strings.HasSuffix(data, "# personal footer\n") {
				t.Fatalf("an update lost the personal edit: %q", data)
			}

			inside := strings.Replace(profileReadFile(t, edited), "# <<< dotfiles:integration.shells <<<", "export INSIDE=1\n# <<< dotfiles:integration.shells <<<", 1)
			profileWrite(t, edited, inside)
			for _, present := range []bool{true, false} {
				finding := e.inspect(shellsID, present)
				if finding.Status != engine.Changed || len(finding.Preserved) != 1 || !strings.HasPrefix(finding.Preserved[0], edited) {
					t.Fatalf("a changed block was not reported: %+v", finding)
				}
			}
			if err := e.apply(shellsID, false); !errors.Is(err, targets.ErrChanged) {
				t.Fatal("setup replaced a changed block without approval", err)
			}
			if err := e.p.Remove(t.Context(), e.resource(shellsID), e.inspect(shellsID, false), e); !errors.Is(err, targets.ErrChanged) {
				t.Fatal("setup removed a changed block without approval", err)
			}
			if got := profileReadFile(t, edited); got != inside {
				t.Fatal("the changed block did not stay")
			}
			e.mustApply(shellsID, true)
			saved, err := filepath.Glob(filepath.Join(e.p.StateDir, "baselines", "replaced-*"))
			if err != nil || len(saved) != 1 || !strings.Contains(profileReadFile(t, saved[0]), "export INSIDE=1") {
				t.Fatal("the approved replacement did not save the changed block", saved, err)
			}

			e.mustRemove(shellsID, false)
			profileWrite(t, edited, strings.TrimSuffix(strings.TrimPrefix(profileReadFile(t, edited), "# personal header\n"), "# personal footer\n"))
			if data, err := os.ReadFile(edited); err == nil && (len(data) == 0 || string(data) == "\xef\xbb\xbf") {
				if err := errors.Join(os.Remove(edited), os.Remove(filepath.Dir(edited))); err != nil && system == "windows" {
					t.Fatal(err)
				}
			}
			if after := profileTree(t, e.home); after != before {
				t.Fatalf("removal did not restore the baseline:\n got %s\nwant %s", after, before)
			}
			if _, err := os.Lstat(filepath.Join(e.p.StateDir, "shells")); !os.IsNotExist(err) {
				t.Fatal("removal left the generated init files", err)
			}
			if len(e.State().Targets) != 0 {
				t.Fatal("removal left a record", e.State().Targets)
			}
		})
	}
}

func TestProfilesReleaseALoaderThatMovedWithZDOTDIR(t *testing.T) {
	profileSystems(t, "linux")
	e := newProfileEnv(t, "linux")
	e.mustApply(shellsID, false)
	e.p.Folders.Zsh = filepath.Join(e.home, "zdot")
	if finding := e.inspect(shellsID, true); finding.Status != engine.Unhealthy {
		t.Fatalf("a new ZDOTDIR needs a repair: %+v", finding)
	}
	e.mustApply(shellsID, false)
	if _, err := os.Lstat(filepath.Join(e.home, ".zshrc")); !os.IsNotExist(err) {
		t.Fatal("the loader at the earlier ZDOTDIR stayed", err)
	}
	if !strings.Contains(profileReadFile(t, filepath.Join(e.home, "zdot", ".zshrc")), "dotfiles:integration.shells") {
		t.Fatal("the loader did not move to the new ZDOTDIR")
	}
	e.mustRemove(shellsID, false)
	if tree := profileTree(t, e.home); tree != "./" {
		t.Fatal("removal left content", tree)
	}
}

func TestProfilesInspectReportsAnUnreadableProfile(t *testing.T) {
	profileSystems(t, "linux")
	e := newProfileEnv(t, "linux")
	damaged := "# >>> dotfiles:integration.shells >>>\n# >>> dotfiles:integration.shells >>>\n"
	profileWrite(t, filepath.Join(e.home, ".zshrc"), damaged)
	finding := e.inspect(shellsID, true)
	if finding.Status != engine.Unknown || !strings.Contains(finding.Reason, "duplicated or incomplete") {
		t.Fatalf("damaged markers were not reported: %+v", finding)
	}
	if err := e.apply(shellsID, true); err == nil {
		t.Fatal("setup wrote a profile with damaged markers")
	}
	if got := profileReadFile(t, filepath.Join(e.home, ".zshrc")); got != damaged {
		t.Fatal("a damaged profile changed")
	}
}
