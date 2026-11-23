//go:build darwin || linux

package platform_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

func TestFolderDiscoveryHonorsAbsentXDGPathWithoutWrites(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "redirected configuration")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", config)
	data := filepath.Join(home, "redirected data")
	t.Setenv("XDG_DATA_HOME", data)
	zsh := filepath.Join(home, "redirected zsh")
	t.Setenv("ZDOTDIR", zsh)
	folders, err := platform.DiscoverFolders()
	if err != nil || folders.Home != home || folders.Config != config || folders.Zsh != zsh || folders.Data != data || folders.GitConfig != filepath.Join(config, "git", "config") {
		t.Fatal(folders, err)
	}
	for _, path := range []string{config, zsh, data} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("discovery created a missing folder", path, err)
		}
	}
	t.Setenv("XDG_DATA_HOME", "relative/data")
	if _, err := platform.DiscoverFolders(); err == nil {
		t.Fatal("relative XDG data path accepted")
	}
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("ZDOTDIR", "relative/zsh")
	if _, err := platform.DiscoverFolders(); err == nil {
		t.Fatal("relative ZDOTDIR accepted")
	}
	t.Setenv("ZDOTDIR", "")
	t.Setenv("XDG_CONFIG_HOME", "relative/path")
	if _, err := platform.DiscoverFolders(); err == nil {
		t.Fatal("relative XDG configuration path accepted")
	}
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	folders, err = platform.DiscoverFolders()
	if err != nil || folders.GitIssue == "" {
		t.Fatal("GIT_CONFIG_GLOBAL override was not reported", folders, err)
	}
}
