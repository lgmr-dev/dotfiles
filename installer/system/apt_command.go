package system

import (
	"regexp"
	"slices"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

var aptPlainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func aptShellLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func (a *APT) privileged(program string, environment []string, args ...string) platform.Command {
	environment = slices.Concat([]string{"LC_ALL=C", "DEBIAN_FRONTEND=noninteractive"}, environment)
	if a.Location.Root {
		return platform.Command{Path: program, Args: args, Env: environment}
	}
	return platform.Command{Path: a.Location.Sudo, Args: slices.Concat([]string{"-n"}, environment, []string{program}, args), Elevated: true}
}

var aptAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

func aptShellWord(word string) string {
	if aptPlainWord.MatchString(word) {
		return word
	}
	if name := aptAssignment.FindString(word); name != "" {
		return name + aptShellLiteral(word[len(name):])
	}
	return aptShellLiteral(word)
}

func aptShellLine(c platform.Command) string {
	words := []string{}
	for _, word := range slices.Concat(c.Env, []string{c.Path}, c.Args) {
		words = append(words, aptShellWord(word))
	}
	return strings.Join(words, " ")
}

func aptRefreshArguments() []string { return []string{"update"} }

func aptInstallArguments(action string, automatic bool, input string) []string {
	args := []string{"--yes", "--no-remove", "--no-install-recommends", "-o", "Dpkg::Use-Pty=0", "-o", "DPkg::Lock::Timeout=30",
		"-o", "Dpkg::Options::=--force-confold"}
	switch action {
	case "repair":
		args = append(args, "--reinstall")
		if automatic {
			args = append(args, "--mark-auto", "--only-upgrade")
		}
	case "update":
		if automatic {
			args = append(args, "--mark-auto")
		}
		args = append(args, "--only-upgrade")
	default:
		args = append(args, "--no-upgrade")
	}
	return append(args, "install", input)
}

func aptRemovalArguments(name string) []string { return []string{"remove", name} }
