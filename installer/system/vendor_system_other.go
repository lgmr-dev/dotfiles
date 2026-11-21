//go:build !windows

package system

import "errors"

// DiscoverWindowsPowerShell returns the Windows PowerShell of the system
// directory. It fails on each other system.
func DiscoverWindowsPowerShell() (string, error) {
	return "", errors.New("system Windows PowerShell discovery requires native Windows")
}

// DiscoverWindowsBuildToolsDirectory returns the dedicated Build Tools
// installation folder. It fails on each other system.
func DiscoverWindowsBuildToolsDirectory() (string, error) {
	return "", errors.New("Build Tools installation directory requires native Windows")
}
