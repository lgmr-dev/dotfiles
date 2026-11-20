package hooks_test

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestShellPathKeepsTheLegacyCommandFoldersAfterTheManagedOnes(t *testing.T) {
	for _, system := range profileSystems(t, "darwin") {
		e := newProfileEnv(t, system)
		e.selected = []string{"config.zsh"}
		before := shellDirectories(shellText(e, "env.sh"), "_dotfiles_directory='", "'")
		code := filepath.Join(e.p.StateDir, "packages", "tool.vscode", "current", "Visual Studio Code.app", "Contents", "Resources", "app", "bin")
		ghostty := filepath.Join(e.p.StateDir, "packages", "tool.ghostty", "current", "Ghostty.app", "Contents", "MacOS")
		e.p.LegacyCommandDirectories = []string{code, ghostty, before[0]}
		after := shellDirectories(shellText(e, "env.sh"), "_dotfiles_directory='", "'")
		if !slices.Equal(after, append(slices.Clone(before), code, ghostty)) {
			t.Fatalf("the legacy command folders must follow the managed folders, one time each:\n got %q\nwant %q", after, append(before, code, ghostty))
		}
		e.p.LegacyCommandDirectories = nil
		if again := shellDirectories(shellText(e, "env.sh"), "_dotfiles_directory='", "'"); !slices.Equal(again, before) {
			t.Fatalf("without legacy folders the managed PATH must not change: %q", again)
		}
	}
}
