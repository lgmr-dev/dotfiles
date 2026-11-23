package hooks_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

const tmuxID = "tmux.plugins"

func TestTmuxPluginsBlockQuotesEachBoundary(t *testing.T) {
	root, err := platform.ResolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := newProfileEnvAt(t, "linux", filepath.Join(root, "it's $HOME #1", "home"))
	e.p.Checkout = filepath.Join(root, `check "out" $x #2`)
	unit := e.unit(tmuxID, ".tmux.plugins.conf")
	plugin := func(name string) string {
		return "'" + filepath.Join(e.p.StateDir, "tools", "tmux."+name, "current") + "'"
	}
	command := "'" + filepath.Join(e.p.Checkout, "tmux", "plugins.sh") + "' " + strings.Join([]string{plugin("sensible"), plugin("yank"), plugin("resurrect"), plugin("continuum")}, " ")
	command = strings.ReplaceAll(command, "it's", `it'"'"'s`)
	want := `run-shell "` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, `#`, `##`).Replace(command) + `"`
	if unit.Kind != "block" || unit.Slot != tmuxID || unit.Path != filepath.Join(e.home, ".tmux.plugins.conf") || string(unit.Content) != want {
		t.Fatalf("tmux plugins block\n got %s\nwant %s", unit.Content, want)
	}
	if strings.Contains(string(unit.Content), "continuum.tmux") {
		t.Fatal("the block must name the plugin folders, not the continuum handler")
	}
}

func TestTmuxPluginsBlockLifecycle(t *testing.T) {
	e := newProfileEnv(t, "linux")
	path := filepath.Join(e.home, ".tmux.plugins.conf")
	e.mustApply(tmuxID, false)
	written := profileReadFile(t, path)
	if !strings.HasPrefix(written, "\n# >>> dotfiles:tmux.plugins >>>\nrun-shell \"") || !strings.HasSuffix(written, "\"\n# <<< dotfiles:tmux.plugins <<<\n") {
		t.Fatalf("tmux plugins block: %q", written)
	}
	profileWrite(t, path, "set -g mouse on\n"+written+"bind r source-file ~/.tmux.conf\n")
	e.mustApply(tmuxID, false)
	e.mustRemove(tmuxID, false)
	if got := profileReadFile(t, path); got != "set -g mouse on\nbind r source-file ~/.tmux.conf\n" {
		t.Fatalf("removal lost a personal line or left the block: %q", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	e.mustApply(tmuxID, false)
	e.mustRemove(tmuxID, false)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("removal left the file that setup created", err)
	}
	if _, err := newProfileEnv(t, "windows").p.Desired(tmuxID, e); err == nil {
		t.Fatal("tmux plugins are not available on Windows")
	}
}
