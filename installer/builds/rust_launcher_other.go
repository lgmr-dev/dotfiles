//go:build !windows

package builds

// RunWindowsRustLauncher reports that the process is not a Rust launcher: the
// launcher exists only on Windows.
func RunWindowsRustLauncher() (bool, int) { return false, 0 }
