package platform

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Folders holds the known folders of the current user, found at the process
// boundary. Zsh is ZDOTDIR or Home. Data is XDG_DATA_HOME or the native
// default. Programs is FOLDERID_Programs on Windows. GitConfig is the complete
// global Git configuration path, which follows HOME and XDG_CONFIG_HOME also
// on Windows. GitIssue blocks only the Git integration.
type Folders struct {
	Home, Config, LocalAppData, AppData, Documents string
	Zsh                                            string
	Data                                           string
	Programs                                       string
	GitConfig                                      string
	GitIssue                                       string
}

// DiscoverStateDirectory returns the installer state folder: the local
// application data folder on Windows, XDG_STATE_HOME or ~/.local/state on the
// other systems, with the name dotfiles.
func DiscoverStateDirectory(ctx Context, f Folders) (string, error) {
	root := os.Getenv("XDG_STATE_HOME")
	if ctx.OS == "windows" {
		root = f.LocalAppData
	} else if root == "" {
		root = filepath.Join(f.Home, ".local", "state")
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || strings.ContainsAny(root, "\x00\r\n") {
		return "", errors.New("installer state folder must be an absolute canonical path")
	}
	return filepath.Join(root, "dotfiles"), nil
}
