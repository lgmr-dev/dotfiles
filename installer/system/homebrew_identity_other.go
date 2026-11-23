//go:build !darwin

package system

import (
	"os"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

func homebrewDirectoryIdentity(file *os.File) (string, error) {
	return platform.DirectoryIdentity(file)
}
