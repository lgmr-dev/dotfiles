package v2import_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/system"
	"github.com/luisgui1757/dotfiles/installer/targets"
	"github.com/luisgui1757/dotfiles/installer/v2import"
)

func value(text string) string {
	return "value:" + base64.StdEncoding.EncodeToString([]byte(text))
}

func mustMap(t *testing.T, f *fx) v2import.Mapping {
	t.Helper()
	mapping, err := v2import.Map(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	return mapping
}

func rowOf(mapping v2import.Mapping, resource, target string) string {
	for _, row := range mapping.Rows {
		if row.Resource == resource && (target == "" || row.Target == target) {
			return row.Result
		}
	}
	return ""
}

func owned(t *testing.T, st state.State, key, resource, kind, before string, made ...string) {
	t.Helper()
	record, ok := st.Targets[key]
	if !ok || record.Resource != resource || record.Kind != kind || record.Applied == "" || record.Before != before || !slices.Equal(record.Made, made) || record.Next != "" || record.Release {
		t.Fatalf("%s: want an owned %s of %s with the baseline %q and created %v, got %+v (recorded: %v)", key, kind, resource, before, made, record, ok)
	}
}

func baselineOnly(t *testing.T, st state.State, key, resource, kind, before string) {
	t.Helper()
	record, ok := st.Targets[key]
	if !ok || record.Resource != resource || record.Kind != kind || record.Applied != "" || record.Before != before {
		t.Fatalf("%s: want a record of %s with only the baseline %q, got %+v (recorded: %v)", key, resource, before, record, ok)
	}
}

func TestMappingOfALinuxInstallation(t *testing.T) {
	f := linuxFixture(t)
	original := f.save()
	before := describe(t, f.root)
	mapping := mustMap(t, f)
	if after := describe(t, f.root); after != before {
		t.Fatal("the mapping wrote to the machine")
	}
	st := mapping.State
	if st.Schema != 2 || st.V2Restore != "switching" || st.Target != "linux/amd64:"+f.home || string(mapping.Original) != string(original) {
		t.Fatalf("mapped state header: %+v", st)
	}
	if !slices.Equal(st.Selected, []string{"fonts", "keyboard", "neovim", "sentinel", "shell", "tmux", "vscode"}) {
		t.Fatalf("the selection maps by identity, with the retired capability merged: %v", st.Selected)
	}
	if !slices.Equal(st.Keep, []string{"tool.lazygit"}) {
		t.Fatalf("keep must drop the retired Flatpak and gain the resource that only release v2 retained: %v", st.Keep)
	}
	if len(st.System) != 1 || !slices.Equal(st.System["apt"], []string{"code", "zsh"}) {
		t.Fatalf("only created and present system items get a record: %v", st.System)
	}

	nvim := filepath.Join(f.home, ".config", "nvim")
	owned(t, st, nvim, "config.nvim", "link", "absent", filepath.Join(f.home, ".config"))
	tmux := filepath.Join(f.home, ".tmux.conf")
	backup := filepath.Join(f.home, ".dotfiles-config-"+operation("config.tmux")+"-0", "previous")
	owned(t, st, tmux, "config.tmux", "link", "backup:"+backup)
	if read(t, backup) != "# my own tmux configuration\n" {
		t.Fatal("the baseline of release v2 must stay where it is")
	}
	shellsDir := filepath.Join(f.state, "shells")
	owned(t, st, filepath.Join(shellsDir, "env.sh"), shells, "file", "absent", shellsDir)
	owned(t, st, filepath.Join(shellsDir, "init.sh"), shells, "file", "absent")
	owned(t, st, targets.Key(filepath.Join(f.home, ".zshrc"), shells), shells, "block", "absent")
	bashrc := filepath.Join(f.home, ".bashrc")
	owned(t, st, targets.Key(bashrc, shells), shells, "block", "absent", bashrc)
	baselineOnly(t, st, targets.Key(filepath.Join(f.home, ".tmux.plugins.conf"), plugins), plugins, "block", "absent")
	settings := filepath.Join(f.home, ".config", "Code", "User", "settings.json")
	owned(t, st, targets.Key(settings, "workbench.colorTheme"), vscode, "field", value("\"Personal \\u003cDark\\u003e\""))
	owned(t, st, targets.Key(settings, "update.mode"), vscode, "field", "absent")
	owned(t, st, f.fontPath(), hooks.FontResource, "link", "absent", filepath.Dir(f.fontPath()), filepath.Dir(filepath.Dir(f.fontPath())))
	owned(t, st, targets.Key(f.fontPath(), hooks.FontSlot), hooks.FontResource, "font", "absent")
	owned(t, st, f.keyboard, "system.keyboard-access", "file", "absent")
	if st.Targets[f.keyboard].Applied != system.KeyboardRuleFingerprint([]byte(keyboardRule(operation("keyboard")))) {
		t.Fatal("the keyboard record must hold the fingerprint of the installed bytes")
	}
	owned(t, st, f.publicRuntime(), hooks.NvimResource, "link", "absent")
	checkout, err := hooks.SentinelCheckoutFingerprint(filepath.Join(f.state, "sentinel", "checkout"))
	if err != nil {
		t.Fatal(err)
	}
	baselineOnly(t, st, hooks.SentinelCheckoutKey(f.state), hooks.SentinelResource, "tree", "fp:"+checkout)
	if len(st.Targets) != 14 {
		t.Fatalf("unexpected records: %v", slices.Sorted(maps.Keys(st.Targets)))
	}
	for resource, want := range map[string]string{
		"tool.nvim":              "private payload",
		"integration.sentinel":   "retired Sentinel record: dropped",
		"skills.sentinel-claude": "retired Sentinel record: dropped",
		"tool.tmux":              "present before release v2",
		"tool.zsh":               "recorded as installed by setup",
		"tool.lazygit":           "private payload",
		"tool.flatpak":           "retired in release v2: its record is dropped and its files stay",
	} {
		if got := rowOf(mapping, resource, ""); !strings.Contains(got, want) {
			t.Fatalf("mapping row of %s: %q, want %q", resource, got, want)
		}
	}
	if len(mapping.Deferred) != 0 || len(mapping.Legacy) != 0 {
		t.Fatalf("Linux defers nothing: %v %v", mapping.Deferred, mapping.Legacy)
	}
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMappingGivesNoOwnershipWithoutAMatchingFingerprint(t *testing.T) {
	f := linuxFixture(t)
	f.save()
	nvim := filepath.Join(f.home, ".config", "nvim")
	if err := os.Remove(nvim); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(nvim, "init.lua"), "-- my own configuration\n", 0644)
	zshrc := filepath.Join(f.home, ".zshrc")
	write(t, zshrc, strings.Replace(read(t, zshrc), ". v2-init", ". edited", 1), 0644)
	settings := filepath.Join(f.home, ".config", "Code", "User", "settings.json")
	write(t, settings, strings.Replace(read(t, settings), `"none"`, `"default"`, 1), 0644)
	write(t, f.keyboard, strings.Replace(read(t, f.keyboard), "uaccess", "uaccess, MODE=\"0666\"", 1), 0644)
	if err := os.Remove(f.publicRuntime()); err != nil {
		t.Fatal(err)
	}
	link(t, filepath.Join(f.home, "my-runtime"), f.publicRuntime())
	f.present["tool.vscode"] = false
	mapping := mustMap(t, f)
	st := mapping.State
	baselineOnly(t, st, nvim, "config.nvim", "tree", "absent")
	baselineOnly(t, st, targets.Key(zshrc, shells), shells, "block", "absent")
	for _, generated := range []string{"env.sh", "init.sh"} {
		if _, has := st.Targets[filepath.Join(f.state, "shells", generated)]; has {
			t.Fatal("a generated file of a changed resource must get no record")
		}
	}
	baselineOnly(t, st, targets.Key(settings, "workbench.colorTheme"), vscode, "field", value("\"Personal \\u003cDark\\u003e\""))
	if _, has := st.Targets[targets.Key(settings, "update.mode")]; has {
		t.Fatal("a field without ownership and without an earlier value must get no record")
	}
	for _, key := range []string{f.keyboard, f.publicRuntime()} {
		if _, has := st.Targets[key]; has {
			t.Fatalf("%s must get no record without proof", key)
		}
	}
	if !slices.Equal(st.System["apt"], []string{"zsh"}) || !strings.Contains(rowOf(mapping, "tool.vscode", ""), "not present now") {
		t.Fatalf("a created system item that is absent now must get no record: %v", st.System)
	}
}

func TestMappingOfAMacInstallationDefersTheThreeApplications(t *testing.T) {
	f := darwinFixture(t)
	f.save()
	mapping := mustMap(t, f)
	st := mapping.State
	if !slices.Equal(st.System["brew"], []string{"tmux"}) || len(st.System["cask"]) != 0 {
		t.Fatalf("system records: %v", st.System)
	}
	application := filepath.Join(f.home, "Applications", "Visual Studio Code.app")
	owned(t, st, application, "desktop.vscode", "link", "absent", filepath.Dir(application))
	if !slices.Equal(mapping.Deferred, []string{"desktop.vscode", "infra.homebrew", "tool.vscode"}) {
		t.Fatalf("deferred resources: %v", mapping.Deferred)
	}
	legacy := filepath.Join(f.state, "packages", "tool.vscode", "current", "Visual Studio Code.app", "Contents", "Resources", "app", "bin")
	if !slices.Equal(mapping.Legacy, []string{legacy}) || !slices.Equal(v2import.LegacyCommandDirectories(f.state, st, f.ctx), []string{legacy}) {
		t.Fatalf("legacy command folders: %v", mapping.Legacy)
	}
	settings := filepath.Join(f.home, "Library", "Application Support", "Code", "User", "settings.json")
	owned(t, st, targets.Key(settings, "update.mode"), vscode, "field", "absent")
	if !slices.Equal(st.Selected, []string{"fonts", "neovim", "sentinel", "shell", "tmux", "vscode"}) {
		t.Fatalf("the selection keeps the deferred tool: %v", st.Selected)
	}
	if err := os.Remove(application); err != nil {
		t.Fatal(err)
	}
	if got := v2import.LegacyCommandDirectories(f.state, st, f.ctx); len(got) != 0 {
		t.Fatalf("after the release of the legacy link no legacy folder stays: %v", got)
	}
}

func TestMappingOfAWindowsInstallation(t *testing.T) {
	f := windowsFixture(t)
	f.save()
	mapping := mustMap(t, f)
	st := mapping.State
	if len(st.System) != 1 || !slices.Equal(st.System["windows"], []string{"tool.vcredist"}) {
		t.Fatalf("system records: %v", st.System)
	}
	owned(t, st, filepath.Join(f.home, ".config", "nvim"), "config.nvim", "tree", "absent", filepath.Join(f.home, ".config"))
	owned(t, st, filepath.Join(f.state, "shells", "init.ps1"), shells, "file", "absent", filepath.Join(f.state, "shells"))
	owned(t, st, targets.Key(filepath.Join(f.home, "Documents", "PowerShell", "profile.ps1"), shells), shells, "block", "absent")
	legacy := filepath.Join(f.home, "Documents", "WindowsPowerShell", "profile.ps1")
	owned(t, st, targets.Key(legacy, shells), shells, "block", "absent", legacy, filepath.Dir(legacy))
	if len(mapping.Deferred) != 0 || !slices.Equal(st.Selected, []string{"neovim", "shell", "vscode"}) {
		t.Fatalf("%v %v", mapping.Deferred, st.Selected)
	}
}

func TestImportRefusesAStateItCannotUseAndWritesNothing(t *testing.T) {
	cases := map[string]func(f *fx){
		"pending transaction": func(f *fx) { f.v1.Transaction = json.RawMessage(`{"plan":{"schema":1},"next":2}`) },
		"in-progress receipt": func(f *fx) {
			receipt := f.v1.Receipts["tool.zsh"]
			receipt.Status = "in-progress"
			f.v1.Receipts["tool.zsh"] = receipt
		},
		"missing baseline": func(f *fx) {
			if err := os.Remove(filepath.Join(f.state, f.recovery("config.nvim"), "configuration.json")); err != nil {
				f.t.Fatal(err)
			}
		},
		"baseline of another receipt": func(f *fx) {
			receipt := f.v1.Receipts[shells]
			receipt.Before.Fingerprint = "0000"
			f.v1.Receipts[shells] = receipt
		},
		"unknown created resource": func(f *fx) { f.receipt("tool.unknown", "archive", "created", true) },
		"retired resource":         func(f *fx) { f.receipt("integration.vscode", "profile-block", "created", true) },
		"other home":               func(f *fx) { f.v1.Target = "/home/someone-else" },
		"unknown field": func(f *fx) {
			f.save()
			path := filepath.Join(f.state, "state.json")
			write(f.t, path, strings.Replace(read(f.t, path), "\"schema\": 1,", "\"schema\": 1,\n  \"future\": true,", 1), 0600)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := linuxFixture(t)
			if name != "unknown field" {
				change(f)
				f.save()
			} else {
				change(f)
			}
			before := describe(t, f.root)
			_, err := f.preview()
			if !errors.Is(err, v2import.ErrRefused) || !strings.Contains(err.Error(), "check out fb15ce5 (v2.3.0), run its setup and finish or abandon the interrupted operation") {
				t.Fatalf("want a refusal with the remedy, got %v", err)
			}
			if after := describe(t, f.root); after != before {
				t.Fatal("a refused import wrote to the machine")
			}
		})
	}
}
