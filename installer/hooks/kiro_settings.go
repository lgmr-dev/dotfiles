package hooks

import (
	"path/filepath"

	"github.com/luisgui1757/dotfiles/installer/targets"
)

func (p *Profiles) kiroUnits() []targets.Desired {
	path := filepath.Join(sentinelKiroFolder(p.Folders.Home, p.KiroHome), "settings", "cli.json")
	return profileFields(path, []profileField{{"app.disableAutoupdates", "true"}})
}
