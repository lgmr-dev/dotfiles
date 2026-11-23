package v2import

import "github.com/lgmr-dev/dotfiles/installer/platform"

func SetFault(f func(point string) error) { fault = f }

func SetRegistryFonts(on bool) func() {
	old := registryFonts
	registryFonts = func(platform.Context) bool { return on }
	return func() { registryFonts = old }
}
