package hooks

import (
	"path/filepath"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/targets"
)

func kiroSettingsHome(home, value string) string {
	fallback := filepath.Join(home, ".kiro")
	if value == "" || !filepath.IsAbs(value) || strings.ContainsAny(value, "\x00\r\n") {
		return fallback
	}
	value = filepath.Clean(value)
	if filepath.Dir(value) == value {
		return fallback
	}
	if relative, err := filepath.Rel(value, home); err == nil && (relative == "." || filepath.IsLocal(relative)) {
		return fallback
	}
	return value
}

func (p *Profiles) kiroUnits() []targets.Desired {
	path := filepath.Join(kiroSettingsHome(p.Folders.Home, p.KiroHome), "settings", "cli.json")
	return profileFields(path, []profileField{{"app.disableAutoupdates", "true"}})
}
