//go:build darwin || linux

package hooks_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/platform"
)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func shellApplied(t *testing.T, home string, selected []string) *profileEnv {
	t.Helper()
	e := newProfileEnvAt(t, "linux", home)
	e.selected = selected
	e.mustApply(shellsID, false)
	return e
}

func shellRoot(t *testing.T) string {
	t.Helper()
	root, err := platform.ResolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSharedShellEnvironmentIsQuietAndQuotesPaths(t *testing.T) {
	home := filepath.Join(shellRoot(t), "home space '$ ` * [x]")
	shellApplied(t, home, nil)
	local := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(local, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bash", "zsh"} {
		shell, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		profile := ".bash_profile"
		if name == "zsh" {
			profile = ".zprofile"
		}
		script := ". " + shellQuote(filepath.Join(home, profile)) + "; . " + shellQuote(filepath.Join(home, profile)) + "; printf '%s\\n' \"$PATH\""
		cmd := exec.Command(shell, "-c", script)
		cmd.Env = append(os.Environ(), "HOME="+home, "PATH=/usr/bin:/bin")
		output, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(output)) != local+":/usr/bin:/bin" {
			t.Fatalf("quiet %s path setup: %q %v", name, output, err)
		}
	}
}

func TestManagedFoldersComeBeforeSystemAndPersonalPathEntries(t *testing.T) {
	home := filepath.Join(shellRoot(t), "home")
	e := shellApplied(t, home, []string{"tool.gh"})
	local := filepath.Join(home, ".local", "bin")
	gh := filepath.Join(e.p.StateDir, "tools", "tool.gh", "current", "bin")
	for _, directory := range []string{local, gh} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	env := filepath.Join(e.p.StateDir, "shells", "env.sh")
	script := ". " + shellQuote(env) + "; . " + shellQuote(env) + "; printf '%s' \"$PATH\""
	for _, shell := range [][]string{{"bash", "--noprofile", "--norc", "-c"}, {"zsh", "-f", "-c"}, {"sh", "-c"}} {
		cmd := exec.Command(shell[0], append(shell[1:], script)...)
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:" + local + ":/personal::/bin"}
		output, err := cmd.CombinedOutput()
		if want := gh + ":" + local + ":/usr/bin:/personal::/bin"; err != nil || string(output) != want {
			t.Fatalf("%s: PATH %q, want %q: %v", shell[0], output, want, err)
		}
	}
}

func TestNeovimIsTheShellEditorAndGhNamesItsStablePath(t *testing.T) {
	for _, test := range []struct {
		name     string
		selected []string
		space    bool
		want     string
	}{
		{"selected", []string{"config.zsh", "config.nvim", "tool.gh"}, false, "nvim|nvim|<nvim>|<gh>"},
		{"unselected keeps personal values", []string{"config.zsh"}, false, "nano|nano|nano|personal"},
		{"whitespace path leaves SUDO_EDITOR", []string{"config.nvim"}, true, "nvim|nvim|nano|personal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := filepath.Join(shellRoot(t), "home")
			if test.space {
				home = filepath.Join(shellRoot(t), "with space", "home")
			}
			e := shellApplied(t, home, test.selected)
			tools := filepath.Join(e.p.StateDir, "tools")
			want := strings.NewReplacer("<nvim>", filepath.Join(tools, "tool.nvim", "current", "bin", "nvim"), "<gh>", filepath.Join(tools, "tool.gh", "current", "bin", "gh")).Replace(test.want)
			script := ". " + shellQuote(filepath.Join(e.p.StateDir, "shells", "env.sh")) + `; printf '%s|%s|%s|%s' "${EDITOR-unset}" "${VISUAL-unset}" "${SUDO_EDITOR-unset}" "${GH_PATH-unset}"`
			for _, shell := range [][]string{{"bash", "--noprofile", "--norc", "-c"}, {"zsh", "-f", "-c"}} {
				cmd := exec.Command(shell[0], append(shell[1:], script)...)
				cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "EDITOR=nano", "VISUAL=nano", "SUDO_EDITOR=nano", "GH_PATH=personal"}
				output, err := cmd.CombinedOutput()
				if err != nil || string(output) != want {
					t.Fatalf("%s: environment %q, want %q: %v", shell[0], output, want, err)
				}
			}
		})
	}
}

func TestSelectedPOSIXPaletteWorksWithoutZsh(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		selected []string
		override string
		want     string
	}{
		{"lsd-default", []string{"integration.lsd"}, "", "di=38;2;246;193;119"},
		{"zsh-default-in-bash", []string{"config.zsh"}, "", "di=38;2;246;193;119"},
		{"shared-default", []string{"config.zsh", "integration.lsd"}, "", "di=38;2;246;193;119"},
		{"explicit-override", []string{"integration.lsd"}, "di=custom:*.txt=custom", "di=custom:*.txt=custom"},
		{"unselected-preserves-personal", nil, "", "di=personal"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			home := filepath.Join(shellRoot(t), "home")
			e := shellApplied(t, home, fixture.selected)
			init := filepath.Join(e.p.StateDir, "shells", "init.sh")
			if output, err := exec.Command("bash", "-n", init).CombinedOutput(); err != nil {
				t.Fatalf("shared initialization must parse in Bash: %s: %v", output, err)
			}
			bin := filepath.Join(home, "bin")
			profileWrite(t, filepath.Join(bin, "lsd"), "#!/bin/sh\nexit 0\n")
			if err := os.Chmod(filepath.Join(bin, "lsd"), 0700); err != nil {
				t.Fatal(err)
			}
			stderrPath := filepath.Join(home, "initialization.stderr")
			script := "set -e; . " + shellQuote(init) + " 2>" + shellQuote(stderrPath) + "; test \"$__DOTFILES_SHELL_READY\" = 1; "
			if slices.Contains(fixture.selected, "integration.lsd") {
				script += "test \"$(alias ls)\" = \"alias ls='lsd'\"; "
			}
			script += "printf '%s' \"$LS_COLORS\""
			cmd := exec.Command("bash", "--noprofile", "--norc", "+m", "-ic", script)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			for _, variable := range os.Environ() {
				key, _, _ := strings.Cut(variable, "=")
				if key != "HOME" && key != "LS_COLORS" && key != "DOTFILES_LS_COLORS" && key != "__DOTFILES_SHELL_READY" && key != "BASH_ENV" && key != "ENV" {
					cmd.Env = append(cmd.Env, variable)
				}
			}
			cmd.Env = append(cmd.Env, "HOME="+home, "LS_COLORS=di=personal", "DOTFILES_LS_COLORS="+fixture.override, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			output, err := cmd.Output()
			initStderr, readErr := os.ReadFile(stderrPath)
			if err != nil || readErr != nil || len(initStderr) != 0 || !strings.Contains(string(output), fixture.want) {
				t.Fatalf("active selected palette: %q; initialization stderr %q; process stderr %q; want %q: %v (stderr read: %v)", output, initStderr, stderr.String(), fixture.want, err, readErr)
			}
			if (fixture.override != "" || fixture.selected == nil) && string(output) != fixture.want {
				t.Fatalf("explicit or unselected palette changed: %q", output)
			}
		})
	}
}

func TestZshHandoffNamesTheObservedZshAndStaysInertWithoutATerminal(t *testing.T) {
	for _, handoff := range []bool{false, true} {
		home := filepath.Join(shellRoot(t), "home")
		selected := []string{"config.zsh"}
		if handoff {
			selected = append(selected, "integration.zsh-handoff")
		}
		e := shellApplied(t, home, selected)
		init := filepath.Join(e.p.StateDir, "shells", "init.sh")
		script := profileReadFile(t, init)
		if handoff != strings.Contains(script, "DOTFILES_ZSH_HANDOFF") || handoff != strings.Contains(script, "ghostty-integration") {
			t.Fatalf("handoff selection and generated initialization disagree:\n%s", script)
		}
		if handoff && strings.Count(script, "'/usr/bin/zsh'") != 4 {
			t.Fatalf("handoff must name the observed zsh exactly four times:\n%s", script)
		}
		for _, shell := range []string{"bash", "zsh"} {
			if output, err := exec.Command(shell, "-n", init).CombinedOutput(); err != nil {
				t.Fatalf("shared initialization must parse in %s: %s: %v", shell, output, err)
			}
		}
		cmd := exec.Command("bash", "--noprofile", "--norc", "+m", "-ic", ". "+shellQuote(init)+"; printf '%s|%s' \"${BASH_VERSION:+bash}\" \"${DOTFILES_ZSH_HANDOFF-unset}\"")
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "SHELL=/bin/bash"}
		output, err := cmd.Output()
		if err != nil || string(output) != "bash|unset" {
			t.Fatalf("a bash without a terminal was handed over: %q %v", output, err)
		}
	}
}
