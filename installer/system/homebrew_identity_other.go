//go:build !darwin

package system

import (
	"os"

	"github.com/luisgui1757/dotfiles/installer/platform"
)

func homebrewDirectoryIdentity(file *os.File) (string, error) {
	return platform.DirectoryIdentity(file)
}
