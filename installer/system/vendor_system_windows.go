package system

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// DiscoverWindowsPowerShell returns the Windows PowerShell of the native
// system directory. It does not use PATH or the SystemRoot variable, which a
// caller can change.
func DiscoverWindowsPowerShell() (string, error) {
	directory, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	program := filepath.Join(directory, "WindowsPowerShell", "v1.0", "powershell.exe")
	info, err := os.Stat(program)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", os.ErrInvalid
	}
	return program, nil
}

// DiscoverWindowsBuildToolsDirectory returns the dedicated Build Tools
// installation folder below the 64-bit Program Files known folder.
func DiscoverWindowsBuildToolsDirectory() (string, error) {
	root, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFilesX64, windows.KF_FLAG_DONT_VERIFY)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "DotfilesBuildTools"), nil
}
