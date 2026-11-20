package hooks

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/builds"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
	"github.com/luisgui1757/dotfiles/installer/tools"
)

// ShellFzfColors holds the official Rose Pine colors for fzf, unchanged:
// https://github.com/rose-pine/fzf/blob/9af300f871df52331c362ca58c73535f69b6fb03/dist/rose-pine.sh (MIT)
const ShellFzfColors = "--color=fg:#908caa,bg:#191724,hl:#ebbcba --color=fg+:#e0def4,bg+:#26233a,hl+:#ebbcba --color=border:#403d52,header:#31748f,gutter:#191724 --color=spinner:#f6c177,info:#9ccfd8 --color=pointer:#c4a7e7,marker:#eb6f92,prompt:#908caa"

const shellGhosttyHandoff = `if [ -n "${ZSH_VERSION-}" ] && [ "${DOTFILES_ZSH_HANDOFF-}" = 1 ]; then
	export DOTFILES_ZSH_HANDOFF=2
	if [ -n "${GHOSTTY_RESOURCES_DIR-}" ] && [ -r "$GHOSTTY_RESOURCES_DIR/shell-integration/zsh/ghostty-integration" ]; then
		. "$GHOSTTY_RESOURCES_DIR/shell-integration/zsh/ghostty-integration"
	fi
fi
`

const shellPrependPath = `_dotfiles_prepend_path() {
	_dotfiles_rest=${PATH-}
	PATH=$1
	[ -n "$_dotfiles_rest" ] || return 0
	while :; do
		case $_dotfiles_rest in
			*:*) _dotfiles_entry=${_dotfiles_rest%%:*}; _dotfiles_rest=${_dotfiles_rest#*:}; _dotfiles_last=0 ;;
			*) _dotfiles_entry=$_dotfiles_rest; _dotfiles_last=1 ;;
		esac
		if [ "$_dotfiles_entry" != "$1" ]; then PATH=$PATH:$_dotfiles_entry; fi
		[ "$_dotfiles_last" = 0 ] || break
	done
}
`

var shellFeatures = []string{"config.zsh", "config.powershell", "integration.starship", "integration.fzf", "integration.lsd", "integration.zoxide", "integration.atuin", "integration.superfile"}

func shellPOSIXLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func shellPowerShellLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func shellPlaceholder(id string) string { return "# @dotfiles-feature:" + id + "@" }

func (p *Profiles) shellFragment(resource, source string) (string, error) {
	for _, fragment := range p.Catalog.Shell {
		if fragment.Resource == resource && fragment.Source == source {
			data, err := tools.FS.ReadFile(fragment.Path())
			return string(data), err
		}
	}
	return "", errors.New("the catalog has no shell fragment " + source + " for " + resource)
}

func (p *Profiles) shellCommand(id, command string) (string, error) {
	pin, ok := p.Pins[id]
	if !ok {
		return "", errors.New("no private package pin for " + id + " on this system")
	}
	path := builds.CommandPath(p.StateDir, id, command, pin)
	if path == "" {
		return "", errors.New("unknown private package command")
	}
	return path, nil
}

func (p *Profiles) shellRequiredFile(id, name string) (string, error) {
	pin, ok := p.Pins[id]
	if !ok {
		return "", errors.New("no private package pin for " + id + " on this system")
	}
	path := builds.RequiredFilePath(p.StateDir, id, name, pin)
	if path == "" {
		return "", errors.New("unknown private package entry file")
	}
	return path, nil
}

func (p *Profiles) shellCommandDirectories(selected []string) []string {
	directories := []string{filepath.Join(p.Folders.Home, ".local", "bin")}
	if p.Homebrew != nil {
		if homebrew := p.Homebrew(); homebrew != "" {
			for _, id := range selected {
				if resource, ok := p.Catalog.Resource(id); ok && resource.Bindings[p.Context.OS].Provider == "homebrew-formula" {
					directories = append(directories, homebrew)
					break
				}
			}
		}
	}
	ids := make([]string, 0, len(p.Pins))
	for id := range p.Pins {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		directories = append(directories, builds.BinaryDirectories(p.StateDir, id, p.Pins[id])...)
	}
	for _, directory := range p.LegacyCommandDirectories {
		if !slices.Contains(directories, directory) {
			directories = append(directories, directory)
		}
	}
	return directories
}

func (p *Profiles) shellStarshipConfig() (string, error) {
	resolved, err := p.Catalog.TargetsFor("config.starship", p.Context, p.Folders, p.Checkout)
	if err != nil {
		return "", err
	}
	if len(resolved) != 1 {
		return "", errors.New("Starship requires exactly one configuration file")
	}
	return resolved[0].Destination, nil
}

func shellZshHandoff(zsh string) string {
	z := shellPOSIXLiteral(zsh)
	return `if [ -n "${BASH_VERSION-}" ] && [ -z "${DOTFILES_ZSH_HANDOFF-}" ] && [ -z "${BASH_EXECUTION_STRING-}" ] && [ -t 0 ] && [ -t 1 ] && [ -x ` + z + ` ]; then
	case ${SHELL-} in
	*/zsh) ;;
	*)
		__dotfiles_had_shell=${SHELL+1}
		__dotfiles_shell=${SHELL-}
		if shopt -q execfail; then __dotfiles_execfail=; else __dotfiles_execfail=1; shopt -s execfail; fi
		export DOTFILES_ZSH_HANDOFF=1 SHELL=` + z + `
		if shopt -q login_shell; then exec ` + z + ` -l; else exec ` + z + `; fi
		if [ -n "$__dotfiles_execfail" ]; then shopt -u execfail; fi
		if [ -n "$__dotfiles_had_shell" ]; then SHELL=$__dotfiles_shell; else unset SHELL; fi
		unset DOTFILES_ZSH_HANDOFF __dotfiles_had_shell __dotfiles_shell __dotfiles_execfail
		;;
	esac
fi
`
}

func shellBashLogin(home string, st state.State) (string, error) {
	candidates := []string{}
	for _, name := range []string{".bash_profile", ".bash_login", ".profile"} {
		candidates = append(candidates, filepath.Join(home, name))
	}
	for _, candidate := range candidates {
		if record, ok := st.Targets[targets.Key(candidate, profileShells)]; ok && record.Resource == profileShells {
			return candidate, nil
		}
	}
	for _, candidate := range candidates {
		if _, err := os.Lstat(candidate); err == nil {
			return candidate, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return candidates[0], nil
}

func (p *Profiles) shellUnits(selected []string, st state.State) ([]targets.Desired, error) {
	root := filepath.Join(p.StateDir, "shells")
	features := map[string]string{}
	has := func(id string) bool { return slices.Contains(selected, id) }
	directories := p.shellCommandDirectories(selected)
	file := func(path, content string) targets.Desired {
		return targets.Desired{Kind: "file", Path: path, Content: []byte(content)}
	}
	block := func(path, script string) targets.Desired {
		return targets.Desired{Kind: "block", Path: path, Slot: profileShells, Content: []byte(script), BOM: p.Context.OS == "windows"}
	}
	var units []targets.Desired
	generated := 0
	if p.Context.OS == "windows" {
		if !filepath.IsAbs(p.Folders.Documents) {
			return nil, errors.New("shell integration needs the actual Documents known folder")
		}
		quoted := make([]string, len(directories))
		for i, directory := range directories {
			if strings.Contains(directory, ";") {
				return nil, errors.New("shell command directory cannot contain the PATH separator")
			}
			quoted[i] = shellPowerShellLiteral(directory)
		}
		interactive, err := p.shellFragment(profileShells, "shell/interactive.ps1")
		if err != nil {
			return nil, err
		}
		cache, err := p.shellFragment(profileShells, "shell/cache.ps1")
		if err != nil {
			return nil, err
		}
		agents := ""
		if has("tool.claude") {
			agents = "$env:DISABLE_UPDATES = '1'\n"
		}
		if has("tool.copilot") {
			agents += "$env:COPILOT_AUTO_UPDATE = 'false'\n"
		}
		if has("tool.opencode") {
			agents += "$env:OPENCODE_DISABLE_AUTOUPDATE = '1'\n"
		}
		loader := agents + "$dotfilesPaths = @(" + strings.Join(quoted, ", ") + ")\n" + `foreach ($directory in $dotfilesPaths) {
	if ((Test-Path -LiteralPath $directory -PathType Container) -and
		$directory -notin ($env:PATH -split [regex]::Escape([IO.Path]::PathSeparator))) {
		$env:PATH = $directory + [IO.Path]::PathSeparator + $env:PATH
	}
}
` + interactive + "\nif ($global:DotfilesShellReady) { return }\n" + cache
		for _, id := range []string{"config.powershell", "integration.starship", "integration.fzf", "integration.lsd", "integration.zoxide", "integration.atuin", "integration.superfile"} {
			if has(id) {
				loader += shellPlaceholder(id) + "\n"
			}
		}
		loader += "$global:DotfilesShellReady = $true\n"
		loaderPath := filepath.Join(root, "init.ps1")
		units, generated = []targets.Desired{file(loaderPath, loader)}, 1
		for _, directory := range []string{"PowerShell", "WindowsPowerShell"} {
			units = append(units, block(filepath.Join(p.Folders.Documents, directory, "profile.ps1"), "if (Test-Path -LiteralPath "+shellPowerShellLiteral(loaderPath)+" -PathType Leaf) { . "+shellPowerShellLiteral(loaderPath)+" }"))
		}
		for _, fragment := range [][2]string{{"config.powershell", "shell/powershell-core.ps1"}, {"integration.lsd", "shell/lsd.ps1"}, {"integration.fzf", "shell/fzf.ps1"}, {"integration.superfile", "shell/superfile.ps1"}} {
			script, err := p.shellFragment(fragment[0], fragment[1])
			if err != nil {
				return nil, err
			}
			if fragment[0] == "integration.fzf" {
				module, err := p.shellRequiredFile("powershell.psfzf", "PSFzf.psd1")
				if err != nil {
					return nil, err
				}
				script = "$script:DotfilesPSFzfPath = " + shellPowerShellLiteral(module) + "\n$script:RosePineFzfColors = " + shellPowerShellLiteral(ShellFzfColors) + "\n" + script
			}
			features[fragment[0]] = script
		}
	} else {
		zshRoot := p.Folders.Zsh
		if zshRoot == "" {
			zshRoot = p.Folders.Home
		}
		if !filepath.IsAbs(zshRoot) {
			return nil, errors.New("ZDOTDIR must be absolute")
		}
		environment := shellPrependPath
		for _, directory := range directories {
			if strings.Contains(directory, ":") {
				return nil, errors.New("shell command directory cannot contain the PATH separator")
			}
			environment += "_dotfiles_directory=" + shellPOSIXLiteral(directory) + "\n" + `if [ -d "$_dotfiles_directory" ]; then
	_dotfiles_prepend_path "$_dotfiles_directory"
fi
`
		}
		environment += "export PATH\nunset -f _dotfiles_prepend_path\nunset _dotfiles_directory _dotfiles_rest _dotfiles_entry _dotfiles_last\n"
		if has("tool.claude") {
			environment += "export DISABLE_UPDATES=1\n"
		}
		if has("tool.copilot") {
			environment += "export COPILOT_AUTO_UPDATE=false\n"
		}
		if has("tool.opencode") {
			environment += "export OPENCODE_DISABLE_AUTOUPDATE=1\n"
		}
		if has("tool.gh") {
			gh, err := p.shellCommand("tool.gh", "gh")
			if err != nil {
				return nil, err
			}
			environment += "export GH_PATH=" + shellPOSIXLiteral(gh) + "\n"
		}
		if has("config.nvim") {
			environment += "export EDITOR=nvim VISUAL=nvim\n"
			nvim, err := p.shellCommand("tool.nvim", "nvim")
			if err != nil {
				return nil, err
			}
			if !strings.ContainsAny(nvim, " \t\\") {
				environment += "export SUDO_EDITOR=" + shellPOSIXLiteral(nvim) + "\n"
			}
		}
		init := "case $- in *i*) ;; *) return 0 ;; esac\n[ \"${__DOTFILES_SHELL_READY-}\" != 1 ] || return 0\n"
		zsh := "/usr/bin/zsh"
		if p.Context.OS == "darwin" {
			zsh = "/bin/zsh"
		}
		if has("integration.zsh-handoff") {
			init += shellZshHandoff(zsh)
		}
		if has("config.zsh") || has("integration.lsd") {
			palette, err := p.shellFragment(profileShells, "shell/ls-colors.sh")
			if err != nil {
				return nil, err
			}
			init += palette + "\n"
		}
		if has("config.zsh") {
			init += "if [ -n \"${ZSH_VERSION-}\" ]; then\n" + shellPlaceholder("config.zsh") + "\nfi\n"
		}
		for _, id := range []string{"integration.starship", "integration.fzf", "integration.lsd", "integration.zoxide", "integration.atuin", "integration.superfile"} {
			if has(id) {
				init += shellPlaceholder(id) + "\n"
			}
		}
		if has("config.zsh") {
			local := shellPOSIXLiteral(filepath.Join(p.Folders.Home, ".zshrc.local"))
			init += "if [ -n \"${ZSH_VERSION-}\" ] && [ -r " + local + " ]; then . " + local + "; fi\n"
		}
		if has("integration.zsh-handoff") {
			init += shellGhosttyHandoff
		}
		init += "__DOTFILES_SHELL_READY=1\n"
		envPath, initPath := filepath.Join(root, "env.sh"), filepath.Join(root, "init.sh")
		attachEnv := "if [ -r " + shellPOSIXLiteral(envPath) + " ]; then . " + shellPOSIXLiteral(envPath) + "; fi\n"
		attachInit := attachEnv + "if [ -r " + shellPOSIXLiteral(initPath) + " ]; then . " + shellPOSIXLiteral(initPath) + "; fi\n"
		login, err := shellBashLogin(p.Folders.Home, st)
		if err != nil {
			return nil, err
		}
		localLogin := filepath.Join(p.Folders.Home, ".zprofile.local")
		zshenv := ":"
		if has("config.zsh") {
			zshenv = "skip_global_compinit=1"
		}
		units, generated = []targets.Desired{
			file(envPath, environment), file(initPath, init),
			block(filepath.Join(zshRoot, ".zshenv"), zshenv),
			block(filepath.Join(zshRoot, ".zprofile"), attachEnv+"if [ -r "+shellPOSIXLiteral(localLogin)+" ]; then . "+shellPOSIXLiteral(localLogin)+"; fi"),
			block(filepath.Join(zshRoot, ".zshrc"), attachInit),
			block(filepath.Join(p.Folders.Home, ".bashrc"), attachInit), block(login, attachInit),
		}, 2

		core, err := p.shellFragment("config.zsh", "shell/zsh-core.zsh")
		if err != nil {
			return nil, err
		}
		fzfTab, err := p.shellRequiredFile("plugin.fzf-tab", "fzf-tab.plugin.zsh")
		if err != nil {
			return nil, err
		}
		autosuggestions, err := p.shellRequiredFile("plugin.zsh-autosuggestions", "zsh-autosuggestions.zsh")
		if err != nil {
			return nil, err
		}
		features["config.zsh"] = "__DOTFILES_FZF_TAB=" + shellPOSIXLiteral(fzfTab) + "\n__DOTFILES_AUTOSUGGESTIONS=" + shellPOSIXLiteral(autosuggestions) + "\n" + core
		features["integration.lsd"] = "if command -v lsd >/dev/null 2>&1; then\n    alias ls='lsd' l='lsd -l' la='lsd -a' lla='lsd -la' lt='lsd --tree'\nfi"
		superfile, err := p.shellFragment("integration.superfile", "shell/superfile.sh")
		if err != nil {
			return nil, err
		}
		features["integration.superfile"] = superfile
		if _, available := p.Pins["tool.fzf"]; available {
			fzf, err := p.shellCommand("tool.fzf", "fzf")
			if err != nil {
				return nil, err
			}
			colors := "case \" ${FZF_DEFAULT_OPTS-} \" in\n    *\" " + ShellFzfColors + " \"*) ;;\n    *) export FZF_DEFAULT_OPTS=\"" + ShellFzfColors + "${FZF_DEFAULT_OPTS:+ $FZF_DEFAULT_OPTS}\" ;;\nesac\n"
			features["integration.fzf"] = colors + "if [ -n \"${ZSH_VERSION-}\" ]; then\n    source <(" + shellPOSIXLiteral(fzf) + " --zsh)\n    if whence -w fzf-tab-complete >/dev/null 2>&1; then\n        bindkey '^I' fzf-tab-complete\n        bindkey -M vicmd '^I' fzf-tab-complete\n    fi\nelif [ -n \"${BASH_VERSION-}\" ]; then\n    source <(" + shellPOSIXLiteral(fzf) + " --bash)\nfi"
		}
	}
	starshipConfig := ""
	for _, id := range []string{"starship", "zoxide", "atuin"} {
		pin, ok := p.Pins["tool."+id]
		if !ok {
			continue
		}
		if id == "starship" {
			var err error
			if starshipConfig, err = p.shellStarshipConfig(); err != nil {
				return nil, err
			}
		}
		command, err := p.shellCommand("tool."+id, id)
		if err != nil {
			return nil, err
		}
		prefix := ""
		if p.Context.OS == "windows" {
			suffix := ""
			if id == "starship" {
				prefix = "$env:STARSHIP_CONFIG = " + shellPowerShellLiteral(starshipConfig) + "\n"
				if has("config.powershell") {
					prefix += "$dotfilesViModeIndicator = try { (Get-PSReadLineOption).ViModeIndicator } catch { $null }\n"
					suffix = "\nif ($null -ne $dotfilesViModeIndicator) { Set-PSReadLineOption -ViModeIndicator $dotfilesViModeIndicator }"
				}
			}
			arguments := "@('init', 'powershell')"
			if id == "starship" {
				arguments = "@('init', 'powershell', '--print-full-init')"
			}
			if id == "atuin" {
				arguments = "@('init', 'powershell', '--disable-up-arrow', '--disable-ai')"
			}
			cache := filepath.Join(p.StateDir, "shell-cache", id+"-"+pin.SHA256+".ps1")
			features["integration."+id] = prefix + ". (Get-DotfilesInitScript -Command " + shellPowerShellLiteral(command) + " -Arguments " + arguments + " -CachePath " + shellPowerShellLiteral(cache) + ")" + suffix
		} else {
			if id == "starship" {
				prefix = "export STARSHIP_CONFIG=" + shellPOSIXLiteral(starshipConfig) + "\n"
			}
			arguments, zsh := "", ""
			if id == "atuin" {
				arguments, zsh = " --disable-up-arrow --disable-ai", "\n    bindkey -M vicmd '^R' atuin-search-vicmd"
			}
			features["integration."+id] = prefix + "if [ -n \"${ZSH_VERSION-}\" ]; then\n    eval \"$(" + shellPOSIXLiteral(command) + " init zsh" + arguments + ")\"" + zsh + "\nelif [ -n \"${BASH_VERSION-}\" ]; then\n    eval \"$(" + shellPOSIXLiteral(command) + " init bash" + arguments + ")\"\nfi"
		}
	}
	for i := range units[:generated] {
		script := string(units[i].Content)
		for _, id := range shellFeatures {
			if strings.Contains(script, shellPlaceholder(id)) {
				feature, ok := features[id]
				if !ok {
					return nil, errors.New("selected shell feature has no native recipe: " + id)
				}
				script = strings.ReplaceAll(script, shellPlaceholder(id), feature)
			}
		}
		if p.Context.OS == "windows" {
			script = "\xef\xbb\xbf" + script
		}
		units[i].Content = []byte(script)
	}
	return units, nil
}
