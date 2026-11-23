package hooks_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/builds"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/hooks"
	"github.com/lgmr-dev/dotfiles/installer/targets"
)

const shellsID = "integration.shells"

func shellText(e *profileEnv, base string) string {
	e.t.Helper()
	return string(e.unit(shellsID, base).Content)
}

func shellDirectories(text, prefix, suffix string) []string {
	directories := []string{}
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			directories = append(directories, strings.TrimSuffix(rest, suffix))
		}
	}
	return directories
}

func TestShellProfilesHaveOneOwnerAcrossAllResources(t *testing.T) {
	for _, system := range profileSystems(t, "linux", "darwin", "windows") {
		t.Run(system, func(t *testing.T) {
			e := newProfileEnv(t, system)
			owners := map[string]string{}
			record := func(key, id string) {
				t.Helper()
				if old, ok := owners[key]; ok {
					t.Fatalf("profile has competing owners: %s: %s and %s", key, old, id)
				}
				owners[key] = id
			}
			for _, resource := range e.p.Catalog.Resources {
				if !resource.Available(e.p.Context) {
					continue
				}
				if resource.Action == "config" {
					resolved, err := e.p.Catalog.TargetsFor(resource.ID, e.p.Context, e.p.Folders, e.p.Checkout)
					if err != nil {
						t.Fatal(err)
					}
					for _, entry := range resolved {
						record(entry.Destination, resource.ID)
					}
				}
				if hooks.ProfileResource(resource.ID) {
					for _, unit := range e.units(resource.ID) {
						record(unit.Key, resource.ID)
					}
				}
			}
			if system == "windows" {
				for _, directory := range []string{"PowerShell", "WindowsPowerShell"} {
					if owners[targets.Key(filepath.Join(e.p.Folders.Documents, directory, "profile.ps1"), shellsID)] != shellsID {
						t.Fatal("all-host profile missed actual Documents folder")
					}
				}
			} else if owners[targets.Key(filepath.Join(e.p.Folders.Zsh, ".zshrc"), shellsID)] != shellsID {
				t.Fatal("actual ZDOTDIR not used")
			}
		})
	}
}

func TestShellPathPutsManagedFoldersFirstOnEachSystem(t *testing.T) {
	for _, system := range profileSystems(t, "linux", "darwin", "windows") {
		for _, formula := range []bool{false, true} {
			e := newProfileEnv(t, system)
			e.p.Homebrew = func() string { return "/opt/homebrew/bin" }
			e.selected = []string{"config.zsh"}
			if formula {
				e.selected = append(e.selected, "tool.tmux")
			}
			want := []string{filepath.Join(e.home, ".local", "bin")}
			if formula && system == "darwin" {
				want = append(want, "/opt/homebrew/bin")
			}
			ids := []string{}
			for id := range e.p.Pins {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			for _, id := range ids {
				want = append(want, builds.BinaryDirectories(e.p.StateDir, id, e.p.Pins[id])...)
			}
			var got []string
			if system == "windows" {
				text := shellText(e, "init.ps1")
				if !strings.HasPrefix(text, "\xef\xbb\xbf$dotfilesPaths = @('") {
					t.Fatalf("the PowerShell loader must start with a byte order mark and the managed folders: %q", text[:80])
				}
				line, _, _ := strings.Cut(strings.TrimPrefix(text, "\xef\xbb\xbf$dotfilesPaths = @('"), "')\n")
				got = strings.Split(line, "', '")
				if !strings.Contains(text, "$env:PATH = $directory + [IO.Path]::PathSeparator + $env:PATH") {
					t.Fatal("the PowerShell loader does not put a managed folder before the inherited PATH")
				}
			} else {
				text := shellText(e, "env.sh")
				got = shellDirectories(text, "_dotfiles_directory='", "'")
				if strings.Count(text, `_dotfiles_prepend_path "$_dotfiles_directory"`) != len(want) || !strings.Contains(text, "PATH=$1\n") {
					t.Fatal("the shared environment does not prepend each managed folder")
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("%s formula=%v: managed PATH folders\n got %q\nwant %q", system, formula, got, want)
			}
			for _, directory := range got[1:] {
				if directory != "/opt/homebrew/bin" && !slices.Contains(strings.Split(directory, string(filepath.Separator)), "current") {
					t.Fatal("a managed command folder does not use the stable current path", directory)
				}
			}
		}
	}
}

func TestShellInitFollowsTheSelection(t *testing.T) {
	for _, test := range []struct {
		name     string
		system   string
		selected []string
		file     string
		has, not []string
	}{
		{"nothing", "linux", nil, "init.sh", []string{"case $- in *i*) ;; *) return 0 ;; esac\n", "__DOTFILES_SHELL_READY=1\n"}, []string{"LS_COLORS", "ZSH_VERSION", "starship", "DOTFILES_ZSH_HANDOFF", "@dotfiles-feature"}},
		{"zsh", "linux", []string{"config.zsh"}, "init.sh", []string{"LS_COLORS", "__DOTFILES_FZF_TAB='", "plugin.fzf-tab/current/fzf-tab.plugin.zsh", "plugin.zsh-autosuggestions/current/zsh-autosuggestions.zsh", ".zshrc.local"}, []string{"alias ls='lsd'", "@dotfiles-feature"}},
		{"lsd alone gets the palette", "linux", []string{"integration.lsd"}, "init.sh", []string{"LS_COLORS", "alias ls='lsd' l='lsd -l'"}, []string{"__DOTFILES_FZF_TAB", ".zshrc.local"}},
		{"starship", "linux", []string{"integration.starship"}, "init.sh", []string{"export STARSHIP_CONFIG='", "tool.starship/current/", " init zsh)\"", " init bash)\""}, []string{"zoxide", "LS_COLORS"}},
		{"zoxide", "darwin", []string{"integration.zoxide"}, "init.sh", []string{"tool.zoxide/current/", " init zsh)\""}, []string{"starship"}},
		{"atuin after fzf", "linux", []string{"integration.fzf", "integration.atuin"}, "init.sh", []string{hooks.ShellFzfColors, " --zsh)", " --bash)", "bindkey '^I' fzf-tab-complete", " init zsh --disable-up-arrow --disable-ai)\"", "bindkey -M vicmd '^R' atuin-search-vicmd"}, nil},
		{"superfile", "linux", []string{"integration.superfile"}, "init.sh", []string{"spf"}, nil},
		{"handoff linux", "linux", []string{"config.zsh", "integration.zsh-handoff"}, "init.sh", []string{"exec '/usr/bin/zsh' -l", "export DOTFILES_ZSH_HANDOFF=2", "ghostty-integration"}, []string{"/bin/zsh' -l; else exec '/bin/zsh'"}},
		{"handoff macOS", "darwin", []string{"config.zsh", "integration.zsh-handoff"}, "init.sh", []string{"exec '/bin/zsh' -l"}, []string{"/usr/bin/zsh"}},
		{"agents", "linux", []string{"tool.claude", "tool.copilot", "tool.opencode"}, "env.sh", []string{"export DISABLE_UPDATES=1\n", "export COPILOT_AUTO_UPDATE=false\n", "export OPENCODE_DISABLE_AUTOUPDATE=1\n"}, []string{"GH_PATH", "EDITOR"}},
		{"no agents", "linux", []string{"config.zsh"}, "env.sh", []string{"export PATH\n"}, []string{"DISABLE_UPDATES", "COPILOT_AUTO_UPDATE", "OPENCODE_DISABLE_AUTOUPDATE"}},
		{"gh and editor", "linux", []string{"tool.gh", "config.nvim"}, "env.sh", []string{"export GH_PATH='", "tool.gh/current/", "export EDITOR=nvim VISUAL=nvim\n", "export SUDO_EDITOR='"}, nil},
		{"windows agents", "windows", []string{"tool.claude", "tool.copilot", "tool.opencode"}, "init.ps1", []string{"$env:DISABLE_UPDATES = '1'\n", "$env:COPILOT_AUTO_UPDATE = 'false'\n", "$env:OPENCODE_DISABLE_AUTOUPDATE = '1'\n", "$global:DotfilesShellReady = $true\n"}, []string{"Get-DotfilesInitScript -Command", "@dotfiles-feature"}},
		{"windows features", "windows", []string{"config.powershell", "integration.starship", "integration.fzf", "integration.lsd", "integration.zoxide", "integration.atuin", "integration.superfile"}, "init.ps1",
			[]string{"$env:STARSHIP_CONFIG = '", "@('init', 'powershell', '--print-full-init')", "@('init', 'powershell', '--disable-up-arrow', '--disable-ai')", "$script:DotfilesPSFzfPath = '", "PSFzf.psd1'", "$script:RosePineFzfColors = '" + hooks.ShellFzfColors + "'", "$dotfilesViModeIndicator", "shell-cache"}, []string{"@dotfiles-feature", "DISABLE_UPDATES"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			profileSystems(t, test.system)
			e := newProfileEnv(t, test.system)
			e.selected = test.selected
			text := shellText(e, test.file)
			for _, part := range test.has {
				if !strings.Contains(text, part) {
					t.Fatalf("generated %s lacks %q:\n%s", test.file, part, text)
				}
			}
			for _, part := range test.not {
				if strings.Contains(text, part) {
					t.Fatalf("generated %s has %q without its selection:\n%s", test.file, part, text)
				}
			}
		})
	}
}

func TestShellFeatureOrderKeepsAtuinAfterFzf(t *testing.T) {
	profileSystems(t, "linux")
	e := newProfileEnv(t, "linux")
	e.selected = []string{"config.zsh", "integration.starship", "integration.fzf", "integration.lsd", "integration.zoxide", "integration.atuin", "integration.superfile", "integration.zsh-handoff"}
	text := shellText(e, "init.sh")
	order := []string{"DOTFILES_ZSH_HANDOFF=1 SHELL=", "LS_COLORS", "__DOTFILES_FZF_TAB=", "export STARSHIP_CONFIG=", "FZF_DEFAULT_OPTS", "alias ls='lsd'", "tool.zoxide/current", "tool.atuin/current", "] && [ -r '", "export DOTFILES_ZSH_HANDOFF=2", "__DOTFILES_SHELL_READY=1"}
	at := -1
	for _, part := range order {
		next := strings.Index(text, part)
		if next <= at {
			t.Fatalf("%q is missing or out of order in the generated initialization", part)
		}
		at = next
	}
}

func TestShellLoadersAttachTheGeneratedFiles(t *testing.T) {
	profileSystems(t, "linux")
	e := newProfileEnv(t, "linux")
	e.p.Folders.Zsh = filepath.Join(e.home, "zdot")
	env, init := filepath.Join(e.p.StateDir, "shells", "env.sh"), filepath.Join(e.p.StateDir, "shells", "init.sh")
	attachEnv := "if [ -r '" + env + "' ]; then . '" + env + "'; fi\n"
	attachInit := attachEnv + "if [ -r '" + init + "' ]; then . '" + init + "'; fi\n"
	local := filepath.Join(e.home, ".zprofile.local")
	want := map[string]string{
		filepath.Join(e.home, "zdot", ".zshenv"):   ":",
		filepath.Join(e.home, "zdot", ".zprofile"): attachEnv + "if [ -r '" + local + "' ]; then . '" + local + "'; fi",
		filepath.Join(e.home, "zdot", ".zshrc"):    attachInit,
		filepath.Join(e.home, ".bashrc"):           attachInit,
		filepath.Join(e.home, ".bash_profile"):     attachInit,
	}
	units := e.units(shellsID)
	if len(units) != 7 || units[0].Kind != "file" || units[0].Path != env || units[1].Kind != "file" || units[1].Path != init {
		t.Fatalf("the generated files must be file targets in the state folder: %+v", units)
	}
	for _, unit := range units[2:] {
		script, ok := want[unit.Path]
		if !ok || unit.Kind != "block" || unit.Slot != shellsID || string(unit.Content) != script {
			t.Fatalf("unexpected loader %s (%s#%s): %q", unit.Path, unit.Kind, unit.Slot, unit.Content)
		}
	}
	e.selected = []string{"config.zsh"}
	if got := string(e.unit(shellsID, ".zshenv").Content); got != "skip_global_compinit=1" {
		t.Fatal("selected zsh does not skip the global compinit", got)
	}
}

func TestShellBashLoginKeepsItsRecordedFile(t *testing.T) {
	profileSystems(t, "linux")
	e := newProfileEnv(t, "linux")
	profile := filepath.Join(e.home, ".profile")
	profileWrite(t, profile, "export PERSONAL=1\n")
	e.mustApply(shellsID, false)
	if !strings.Contains(profileReadFile(t, profile), "# >>> dotfiles:integration.shells >>>") {
		t.Fatal("the existing login profile did not get the loader")
	}
	profileWrite(t, filepath.Join(e.home, ".bash_profile"), "export LATER=1\n")
	if finding := e.inspect(shellsID, true); finding.Status != engine.Satisfied {
		t.Fatalf("a new personal login profile moved the owned block: %+v", finding)
	}
	e.mustApply(shellsID, false)
	if got := profileReadFile(t, filepath.Join(e.home, ".bash_profile")); got != "export LATER=1\n" {
		t.Fatalf("the later login profile changed: %q", got)
	}
	e.mustRemove(shellsID, false)
	if got := profileReadFile(t, profile); got != "export PERSONAL=1\n" {
		t.Fatalf("removal did not restore the login profile: %q", got)
	}
}

func TestShellSelectionsDoNotShareGeneratedContent(t *testing.T) {
	profileSystems(t, "linux")
	e := newProfileEnv(t, "linux")
	results := map[string]string{}
	for i := 0; i < 6; i++ {
		id := []string{"integration.starship", "integration.zoxide"}[i%2]
		e.selected = []string{id}
		text := shellText(e, "init.sh")
		if old, ok := results[id]; ok && old != text {
			t.Fatal("same selection had unstable recipe")
		}
		results[id] = text
	}
	if results["integration.starship"] == results["integration.zoxide"] {
		t.Fatal("selection did not bind its own generated setup")
	}
	if tree := profileTree(t, e.home); tree != "./" {
		t.Fatal("a preview wrote to home", tree)
	}
}
