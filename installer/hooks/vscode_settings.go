package hooks

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/state"
	"github.com/lgmr-dev/dotfiles/installer/targets"
)

const vscodeUpdateMode = "update.mode"

func vscodeSettingsPath(ctx platform.Context, f platform.Folders) string {
	root := filepath.Join(f.Config, "Code")
	if ctx.OS == "darwin" {
		root = filepath.Join(f.Home, "Library", "Application Support", "Code")
	} else if ctx.OS == "windows" {
		root = filepath.Join(f.AppData, "Code")
	}
	return filepath.Join(root, "User", "settings.json")
}

func vscodeSettingsFile(path string) bool {
	return filepath.Base(path) == "settings.json" && filepath.Base(filepath.Dir(path)) == "User" && filepath.Base(filepath.Dir(filepath.Dir(path))) == "Code"
}

func (p *Profiles) vscodeUpdateModeManaged(st state.State) bool {
	if p.Context.OS != "darwin" {
		return true
	}
	if p.VSCodeCask == nil {
		return false
	}
	recorded := slices.ContainsFunc(p.recorded(st, profileVSCode), func(key string) bool {
		return strings.HasSuffix(key, "#"+vscodeUpdateMode)
	})
	return recorded && !p.VSCodeCask(st)
}

func (p *Profiles) vscodeUnits(st state.State) []targets.Desired {
	fields := []profileField{
		{"workbench.colorTheme", `"Rosé Pine"`},
		{"workbench.preferredDarkColorTheme", `"Rosé Pine"`},
		{"workbench.preferredLightColorTheme", `"Rosé Pine"`},
		{"window.autoDetectColorScheme", `false`},
		{"editor.fontFamily", `"'Hack Nerd Font', Consolas, monospace"`},
		{"terminal.integrated.fontFamily", `"'Hack Nerd Font', Consolas, monospace"`},
	}
	if p.vscodeUpdateModeManaged(st) {
		fields = append(fields, profileField{vscodeUpdateMode, `"none"`})
	}
	return profileFields(vscodeSettingsPath(p.Context, p.Folders), fields)
}

func (p *Profiles) vscodeNotes() []string {
	if p.Context.OS != "darwin" {
		return nil
	}
	path, err := platform.BindDestination(vscodeSettingsPath(p.Context, p.Folders))
	if err != nil {
		return nil
	}
	data, err := profileRead(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	value, present, err := targets.Field{JSONC: vscodeSettingsFile}.Extract(data, targets.Desired{Path: path, Slot: vscodeUpdateMode})
	if err != nil || !present || string(value) != `"none"` {
		return nil
	}
	return []string{"update.mode: none"}
}
