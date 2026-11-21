package hooks_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/builds"
	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

// TestWriteActiveShellFixtures writes the generated shell profiles of four
// selections into the empty folder DOTFILES_WRITE_SHELL_FIXTURES. The shell
// and Pester runners in tests/ call it: it uses the provider and fixture
// commands, and it installs nothing.
func TestWriteActiveShellFixtures(t *testing.T) {
	root := os.Getenv("DOTFILES_WRITE_SHELL_FIXTURES")
	if root == "" {
		t.Skip("shell/Pester runner supplies a fresh temporary export directory")
	}
	root, err := platform.ResolvePath(root)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("shell fixtures require an existing empty directory", err)
	}
	variants := []string{"windows"}
	if runtime.GOOS != "windows" {
		variants = append(variants, "zsh", "posix", "atuin")
	}
	for _, variant := range variants {
		home := filepath.Join(root, variant)
		system := "linux"
		if variant == "windows" {
			system = "windows"
		}
		e := newProfileEnvIn(t, system, home, filepath.Join(home, "state"))
		e.p.Folders = platform.Folders{Home: home, Config: filepath.Join(home, "config"), Documents: filepath.Join(home, "documents"), Zsh: home}
		e.p.Pins = map[string]catalog.ArchivePin{}
		add := func(id, file, command, content string) {
			pin := catalog.ArchivePin{Version: "fixture", URL: "https://example.invalid/fixture.zip", SHA256: strings.Repeat("1", 64), Format: "zip", RequiredFiles: []string{file}}
			if command != "" {
				pin.Commands = map[string]string{command: file}
				pin.BinDirs = []string{"bin"}
			}
			e.p.Pins[id] = pin
			path := filepath.Join(builds.CurrentDir(e.p.StateDir, id), filepath.FromSlash(file))
			profileWrite(t, path, content)
			if err := os.Chmod(path, 0700); err != nil {
				t.Fatal(err)
			}
		}
		e.selected = []string{"config.zsh", "integration.lsd"}
		if variant == "windows" {
			e.selected = []string{"config.powershell", "integration.starship", "integration.fzf", "integration.lsd", "integration.zoxide", "integration.atuin", "integration.superfile"}
			add("tool.starship", "bin/starship.ps1", "starship", "$global:LASTEXITCODE = 0\n'function global:prompt { return \"fixture> \" }'\n'Set-PSReadLineOption -ViModeIndicator script -ViModeChangeHandler { [Microsoft.PowerShell.PSConsoleReadLine]::InvokePrompt() }'\n")
			add("tool.zoxide", "bin/zoxide.ps1", "zoxide", "$global:LASTEXITCODE = 0\n'function global:z { }'\n")
			add("tool.fzf", "bin/fzf.ps1", "fzf", "# External picker fixture; no terminal picker is invoked.\n")
			add("tool.atuin", "bin/atuin.ps1", "atuin", "$global:LASTEXITCODE = 0\n'$global:DotfilesFixtureAtuin = ''' + ($args -join ' ') + '|'' + ($global:DotfilesFixtureFzf -join '','')'\n")
			add("powershell.psfzf", "PSFzf.psd1", "", "@{ RootModule = 'PSFzf.psm1'; ModuleVersion = '0.0.1'; FunctionsToExport = @('Set-PsFzfOption') }\n")
			profileWrite(t, filepath.Join(builds.CurrentDir(e.p.StateDir, "powershell.psfzf"), "PSFzf.psm1"), `function Set-PsFzfOption {
	[CmdletBinding()]
	param([string]$PSReadlineChordProvider, [string]$PSReadlineChordReverseHistory, [string]$PSReadlineChordSetLocation)
	$global:DotfilesFixtureFzf = @((Get-PSReadLineOption).EditMode.ToString(), $PSReadlineChordProvider, $PSReadlineChordReverseHistory, $PSReadlineChordSetLocation)
}
`)
		} else {
			add("plugin.fzf-tab", "fzf-tab.plugin.zsh", "", "fzf-tab-complete() { zle menu-select }\nzle -N fzf-tab-complete\n")
			add("plugin.zsh-autosuggestions", "zsh-autosuggestions.zsh", "", "_zsh_autosuggest_start() { :; }\n")
			if variant == "posix" || variant == "atuin" {
				e.selected = append(e.selected, "integration.starship", "integration.fzf", "integration.zoxide", "integration.zsh-handoff", "config.nvim", "integration.superfile")
				add("tool.nvim", "bin/nvim", "nvim", "#!/bin/sh\nexit 0\n")
				add("tool.starship", "bin/starship", "starship", "#!/bin/sh\nprintf '%s\\n' 'export DOTFILES_FIXTURE_STARSHIP=loaded'\n")
				add("tool.zoxide", "bin/zoxide", "zoxide", "#!/bin/sh\nprintf '%s\\n' 'z() { :; }'\n")
				add("tool.fzf", "bin/fzf", "fzf", `#!/bin/sh
cat <<'ZSH'
fzf-completion() { zle menu-select }
zle -N fzf-completion
bindkey '^I' fzf-completion
fzf-history-widget() { zle history-incremental-search-backward }
zle -N fzf-history-widget
bindkey -M viins '^R' fzf-history-widget
bindkey -M vicmd '^R' fzf-history-widget
ZSH
`)
			}
			if variant == "atuin" {
				e.selected = append(e.selected, "integration.atuin")
				add("tool.atuin", "bin/atuin", "atuin", `#!/bin/sh
[ "$1" = init ] && [ "$2" = zsh ] || exit 0
cat <<'ZSH'
_atuin_search() { zle history-incremental-search-backward }
for widget in atuin-search atuin-search-viins atuin-search-vicmd atuin-up-search-viins atuin-up-search-vicmd; do zle -N $widget _atuin_search; done
bindkey -M emacs '^r' atuin-search
bindkey -M viins '^r' atuin-search-viins
bindkey -M vicmd '/' atuin-search
ZSH
case " $* " in
*" --disable-up-arrow "*) ;;
*) printf '%s\n' "bindkey -M viins '^[[A' atuin-up-search-viins" "bindkey -M vicmd '^[[A' atuin-up-search-vicmd" ;;
esac
case " $* " in
*" --disable-ai "*) ;;
*) printf '%s\n' "bindkey -M viins '?' atuin-search-viins" ;;
esac
`)
			}
		}
		e.mustApply(shellsID, false)
	}
}
