package system

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
)

type macPrerequisiteFixture struct {
	d                                      *MacPrerequisites
	root                                   string
	commands                               []string
	selectedError, findError, versionError error
	selected, outside, version, sdk        string
}

func newMacPrerequisiteFixture(t *testing.T) *macPrerequisiteFixture {
	t.Helper()
	root := macTestRoot(t)
	f := &macPrerequisiteFixture{d: NewMacPrerequisites(), root: root, selected: root, sdk: filepath.Join(root, "SDKs", "macos.sdk")}
	if err := os.MkdirAll(f.sdk, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"clang", "clang++", "make", "pbcopy", "pbpaste"} {
		macTestWrite(t, filepath.Join(root, "usr", "bin", name), "fixture executable; never executed", 0755)
	}
	f.d.clipboardCommands = []string{filepath.Join(root, "usr", "bin", "pbcopy"), filepath.Join(root, "usr", "bin", "pbpaste")}
	f.d.Query = func(_ context.Context, program string, args ...string) ([]byte, error) {
		f.commands = append(f.commands, program+" "+strings.Join(args, " "))
		switch program {
		case "/usr/bin/xcode-select":
			return []byte(f.selected + "\n"), f.selectedError
		case "/usr/bin/xcrun":
			if slices.Contains(args, "--show-sdk-path") {
				return []byte(f.sdk + "\n"), f.findError
			}
			if f.outside != "" {
				return []byte(f.outside + "\n"), nil
			}
			return []byte(filepath.Join(root, "usr", "bin", args[len(args)-1]) + "\n"), f.findError
		default:
			if f.version != "" {
				return []byte(f.version), f.versionError
			}
			if filepath.Base(program) == "make" {
				return []byte("GNU Make 3.81\nfixture build\n"), f.versionError
			}
			return []byte("Apple clang version 17.0.0\nfixture build\n"), f.versionError
		}
	}
	return f
}

func (f *macPrerequisiteFixture) inspect(id string) (engine.Finding, error) {
	return f.d.Inspect(context.Background(), catalog.Resource{ID: id}, engine.Want{Present: true}, newMacTestRecords())
}

func TestMacPrerequisitesUseTheSelectedToolsWithoutARecord(t *testing.T) {
	f := newMacPrerequisiteFixture(t)
	rec := newMacTestRecords()
	for _, id := range []string{"tool.compiler", "tool.make", "tool.clipboard"} {
		found, err := f.d.Inspect(context.Background(), catalog.Resource{ID: id}, engine.Want{Present: true}, rec)
		if err != nil || found.Status != engine.Satisfied || found.Token == "" || found.Pending || found.Privileged || len(found.Commands) != 0 {
			t.Fatal(id, found, err)
		}
		again, err := f.inspect(id)
		if err != nil || again.Token != found.Token {
			t.Fatal("an unchanged prerequisite changed its token", id, err)
		}
		if err := f.d.Apply(context.Background(), catalog.Resource{ID: id}, found, rec); err == nil {
			t.Fatal("setup claimed an Apple prerequisite", id)
		}
		if err := f.d.Remove(context.Background(), catalog.Resource{ID: id}, found, rec); err == nil {
			t.Fatal("setup removed an Apple prerequisite", id)
		}
	}
	if len(rec.State().System) != 0 || len(rec.State().Pending) != 0 {
		t.Fatal("a prerequisite wrote a record", rec.State())
	}
	clipboard, _ := f.inspect("tool.clipboard")
	if len(clipboard.Notes) != 1 || !strings.Contains(clipboard.Notes[0], "clipboard contents were not read or changed") {
		t.Fatal("the clipboard limit is not disclosed", clipboard.Notes)
	}
	if _, err := f.inspect("tool.git"); err == nil {
		t.Fatal("a resource that is not an Apple prerequisite was accepted")
	}
}

func TestMacPrerequisitesMissingSelectionNeverRunsADeveloperShim(t *testing.T) {
	f := newMacPrerequisiteFixture(t)
	f.selectedError = errMacNoDeveloperSelection
	for _, id := range []string{"tool.compiler", "tool.make"} {
		found, err := f.inspect(id)
		if err != nil || found.Status != engine.Unknown || !strings.Contains(found.Reason, "xcode-select --install") {
			t.Fatal(found, err)
		}
	}
	for _, command := range f.commands {
		if !strings.HasPrefix(command, "/usr/bin/xcode-select ") {
			t.Fatal("a missing selection reached another command", command)
		}
	}
}

func TestMacPrerequisitesRejectUnselectedWrongOrIncompleteTools(t *testing.T) {
	f := newMacPrerequisiteFixture(t)
	outside := filepath.Join(macTestRoot(t), "clang")
	macTestWrite(t, outside, "outside", 0755)
	f.outside = outside
	if _, err := f.inspect("tool.compiler"); err == nil || !strings.Contains(err.Error(), "outside its selected developer directory") {
		t.Fatal("a command outside the selected directory was accepted", err)
	}
	f = newMacPrerequisiteFixture(t)
	f.version = "gcc (GCC) 14\n"
	for _, id := range []string{"tool.compiler", "tool.make"} {
		if found, err := f.inspect(id); err != nil || found.Status != engine.Blocked || !strings.Contains(found.Reason, "failed its native health check") {
			t.Fatal(id, found, err)
		}
	}
	f = newMacPrerequisiteFixture(t)
	if err := os.RemoveAll(f.sdk); err != nil {
		t.Fatal(err)
	}
	if found, err := f.inspect("tool.compiler"); err != nil || found.Status != engine.Blocked || !strings.Contains(found.Reason, "SDK") {
		t.Fatal(found, err)
	}
	if found, err := f.inspect("tool.make"); err != nil || found.Status != engine.Satisfied {
		t.Fatal("GNU Make needs no SDK", found, err)
	}
	f = newMacPrerequisiteFixture(t)
	f.findError = errors.New("license not accepted")
	if found, err := f.inspect("tool.make"); err != nil || found.Status != engine.Unknown || !strings.Contains(found.Reason, "cannot resolve make") {
		t.Fatal(found, err)
	}
	f = newMacPrerequisiteFixture(t)
	if err := os.Remove(f.d.clipboardCommands[1]); err != nil {
		t.Fatal(err)
	}
	if found, err := f.inspect("tool.clipboard"); err != nil || found.Status != engine.Blocked {
		t.Fatal(found, err)
	}
}

func TestMacPrerequisitesRejectInvalidPathsAndCancellation(t *testing.T) {
	for _, path := range []string{"relative", "/", "/a/../b", "/tools\nother", ""} {
		if _, err := macosDeveloperPath([]byte(path + "\n")); err == nil {
			t.Fatal("invalid developer path accepted", path)
		}
	}
	f := newMacPrerequisiteFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.d.Inspect(ctx, catalog.Resource{ID: "tool.compiler"}, engine.Want{Present: true}, newMacTestRecords()); !errors.Is(err, context.Canceled) || len(f.commands) != 0 {
		t.Fatal("a cancelled inspection ran a command", err, f.commands)
	}
}

func TestMacPrerequisiteQueryIgnoresForeignToolchainOverrides(t *testing.T) {
	root := macTestRoot(t)
	program := filepath.Join(root, "print-environment")
	macTestWrite(t, program, "#!/bin/sh\nprintf '%s|%s|%s|%s\\n' \"${DEVELOPER_DIR-unset}\" \"${SDKROOT-unset}\" \"${TOOLCHAINS-unset}\" \"${LC_ALL-unset}\"\n", 0755)
	t.Setenv("DEVELOPER_DIR", "/foreign/Xcode.app")
	t.Setenv("SDKROOT", "/foreign/sdk")
	t.Setenv("TOOLCHAINS", "foreign")
	output, err := MacPrerequisiteQuery(context.Background(), program)
	if err != nil || string(output) != "unset|unset|unset|C\n" {
		t.Fatal("the inspection inherited a toolchain override", string(output), err)
	}
	if _, err := MacPrerequisiteQuery(context.Background(), "relative"); err == nil {
		t.Fatal("a relative program was accepted")
	}
}

func TestMacZshIsPresentOnly(t *testing.T) {
	root := macTestRoot(t)
	d := NewMacZsh()
	if d.path != "/bin/zsh" {
		t.Fatal(d.path)
	}
	d.path = filepath.Join(root, "zsh")
	r, rec := catalog.Resource{ID: "tool.zsh"}, newMacTestRecords()
	found, err := d.Inspect(context.Background(), r, engine.Want{Present: true}, rec)
	if err != nil || found.Status != engine.Blocked || !strings.Contains(found.Reason, "/bin/zsh") {
		t.Fatal(found, err)
	}
	macTestWrite(t, d.path, "shell", 0644)
	if found, err := d.Inspect(context.Background(), r, engine.Want{Present: true}, rec); err != nil || found.Status != engine.Blocked {
		t.Fatal("a shell without the execute bit was accepted", found, err)
	}
	if err := os.Chmod(d.path, 0755); err != nil {
		t.Fatal(err)
	}
	found, err = d.Inspect(context.Background(), r, engine.Want{Present: true}, rec)
	if err != nil || found.Status != engine.Satisfied || found.Token == "" || found.Pending || len(found.Commands) != 0 {
		t.Fatal(found, err)
	}
	if d.Apply(context.Background(), r, found, rec) == nil || d.Remove(context.Background(), r, found, rec) == nil {
		t.Fatal("setup changed the system shell")
	}
	if len(rec.State().System) != 0 || len(rec.State().Pending) != 0 {
		t.Fatal("the system shell got a record", rec.State())
	}
}
