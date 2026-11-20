package hooks

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/targets"
)

func (p *Profiles) tmuxUnits() ([]targets.Desired, error) {
	if p.Context.OS == "windows" {
		return nil, errors.New("tmux is not available on Windows")
	}
	if !filepath.IsAbs(p.Checkout) {
		return nil, errors.New("tmux integration requires the absolute checkout path")
	}
	arguments := []string{shellPOSIXLiteral(filepath.Join(p.Checkout, "tmux", "plugins.sh"))}
	for _, name := range []string{"sensible", "yank", "resurrect", "continuum"} {
		entry, err := p.shellRequiredFile("tmux."+name, name+".tmux")
		if err != nil {
			return nil, err
		}
		arguments = append(arguments, shellPOSIXLiteral(filepath.Dir(entry)))
	}
	quoted := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "$", "\\$", "#", "##").Replace(strings.Join(arguments, " "))
	return []targets.Desired{{Kind: "block", Path: filepath.Join(p.Folders.Home, ".tmux.plugins.conf"), Slot: profileTmux, Content: []byte("run-shell \"" + quoted + "\"")}}, nil
}
