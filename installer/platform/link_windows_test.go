package platform_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

func TestWindowsDirectoryJunctionSupportsLongPaths(t *testing.T) {
	root := resolvedTemp(t)
	directory := filepath.Join(root, strings.Repeat("a", 120), strings.Repeat("b", 120))
	target := filepath.Join(directory, "payload")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "current")
	if err := platform.CreateDirectoryLink(target, link); err != nil {
		t.Fatal(err)
	}
	actual, err := platform.ResolvePath(link)
	if err != nil || actual != target {
		t.Fatal("native long-path junction resolution", actual, target, err)
	}
	file := filepath.Join(target, "pwsh.exe")
	writeFixture(t, file, "private path-resolution fixture")
	actual, err = platform.ResolvePath(filepath.Join(link, "pwsh.exe"))
	if err != nil || actual != file {
		t.Fatal("native long-path junction child resolution", actual, file, err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("junction removal touched its referent", err)
	}
}
