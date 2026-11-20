package platform

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

func systemCommandDirectories() ([]string, error) {
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	root, err := windows.GetWindowsDirectory()
	if err != nil {
		return nil, err
	}
	return []string{system, root, filepath.Join(system, "Wbem")}, nil
}
