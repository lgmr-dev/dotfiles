package platform

import "strings"

// WindowsExtendedPath returns the extended-length form of an absolute Windows
// path. It is pure, so a POSIX host can also check Windows recipes.
func WindowsExtendedPath(path string) string {
	if strings.HasPrefix(path, `\\?\`) {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(path, `\\`)
	}
	return `\\?\` + path
}
