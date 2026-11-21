package catalog_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

func host(ctx platform.Context) platform.Host {
	return platform.Host{Context: ctx, Libc: "glibc"}
}

func plainPin() catalog.ArchivePin {
	return catalog.ArchivePin{Version: "1.0.0", URL: "https://example.com/tool", SHA256: strings.Repeat("a", 64), Format: "file", File: "tool", Commands: map[string]string{"tool": "tool"}, BinDirs: []string{"."}}
}

func TestArchiveSelectionRequiresObservedLibc(t *testing.T) {
	c := released(t)
	for _, libc := range []string{"", "musl", "glibc"} {
		_, nvim := c.Pins.Archive("tool.nvim", platform.Host{Context: linux, Libc: libc})
		_, starship := c.Pins.Archive("tool.starship", platform.Host{Context: linux, Libc: libc})
		if (nvim == nil) != (libc == "glibc") || starship != nil {
			t.Fatal("archive selection guessed libc or removed a static tool", libc, nvim, starship)
		}
		if nvim != nil && !errors.Is(nvim, catalog.ErrNoArchive) {
			t.Fatal("a libc mismatch must report a missing archive", nvim)
		}
	}
	for family, ctx := range map[string]platform.Context{"tool.kiro": windows, "tool.powershell": linux, "tool.missing": linux, "tool.zsh": linux} {
		if _, err := c.Pins.Archive(family, host(ctx)); !errors.Is(err, catalog.ErrNoArchive) {
			t.Error("a family without a pin for the host must report a missing archive", family, err)
		}
	}
	if _, err := c.Pins.Archive("tool.nvim", platform.Host{Context: platform.Context{OS: "darwin", Arch: "amd64"}}); err == nil || errors.Is(err, catalog.ErrNoArchive) {
		t.Fatal("an unsupported host must fail before pin selection", err)
	}
}

func TestUbuntu2404KeepsTheLastGhosttyForItsGTK(t *testing.T) {
	c := released(t)
	for _, ctx := range []platform.Context{linux, linuxARM} {
		selected := map[string]catalog.ArchivePin{}
		for _, release := range []struct{ distro, version string }{{"ubuntu", "24.04"}, {"ubuntu", "26.04"}, {"debian", "13"}, {"debian", ""}} {
			pin, err := c.Pins.Archive("tool.ghostty", platform.Host{Context: ctx, Distro: release.distro, DistroRelease: release.version, Libc: "glibc"})
			if err != nil {
				t.Fatal(err)
			}
			selected[release.distro+" "+release.version] = pin
		}
		oldest, current := selected["ubuntu 24.04"], selected["ubuntu 26.04"]
		if oldest.SHA256 == "" || oldest.Version == current.Version || !strings.Contains(oldest.URL, "_24.04.deb") {
			t.Fatal("Ubuntu 24.04 must keep the Ghostty build for its GTK", ctx, oldest, current)
		}
		if selected["debian 13"].SHA256 != current.SHA256 || selected["debian "].SHA256 != current.SHA256 {
			t.Fatal("a release without its own pin must use the common Ghostty build", ctx, selected)
		}
	}
}

func TestEachSelectedArchiveIsValidAndBoundToItsRecipe(t *testing.T) {
	c := released(t)
	images := 0
	for _, ctx := range contexts {
		key := ctx.OS + "/" + ctx.Arch
		python, err := c.Pins.Archive("tool.python", host(ctx))
		if err != nil {
			t.Fatal(err)
		}
		for family, platforms := range c.Pins.Archives {
			stated, exists := platforms[key]
			pin, err := c.Pins.Archive(family, host(ctx))
			if !exists {
				if !errors.Is(err, catalog.ErrNoArchive) {
					t.Fatal(family, key, err)
				}
				continue
			}
			if err != nil || pin.Validate() != nil || pin.SHA256 != stated.SHA256 {
				t.Fatal("selected archive is invalid", family, key, err, pin.Validate())
			}
			if (pin.Format == "dmg") != (pin.DMGRevision == 1) || stated.DMGRevision != 0 {
				t.Fatal("DMG transport revision is not bound to disk images only", family, key)
			}
			if pin.Format == "dmg" {
				images++
				for _, revision := range []int{-1, 0, 2} {
					pin.DMGRevision = revision
					if pin.Validate() == nil {
						t.Fatal("unknown transport accepted", revision)
					}
				}
				pin.DMGRevision, pin.Format, pin.AppBundle = 1, "zip", ""
				if pin.Validate() == nil {
					t.Fatal("DMG transport accepted for ZIP")
				}
			}
		}
		latex, err := c.Pins.Archive("tool.latex2text", host(ctx))
		if err != nil || latex.Latex2text.PythonPinID != catalog.ArchivePinID(python) || latex.Latex2text.ConsoleRevision != 1 || (latex.WindowsVenvRevision == 1) != (ctx.OS == "windows") {
			t.Fatal("latex2text is not bound to its interpreter identity", key, err)
		}
		yamllint, err := c.Pins.Archive("tool.yamllint", host(ctx))
		if err != nil || yamllint.Yamllint.PythonPinID != catalog.ArchivePinID(python) || (yamllint.WindowsVenvRevision == 1) != (ctx.OS == "windows") {
			t.Fatal("yamllint is not bound to its interpreter identity", key, err)
		}
		if c.Pins.Archives["tool.yamllint"][key].Yamllint.PythonPinID != "" || c.Pins.Archives["tool.latex2text"][key].Latex2text.PythonPinID != "" {
			t.Fatal("a selection changed the stated pin", key)
		}
	}
	if images == 0 {
		t.Fatal("the released pins hold no disk image")
	}
}

func TestLoadBindsPythonToolsToTheirPythonArchive(t *testing.T) {
	python := func(change func(pin map[string]any)) func(map[string]any) {
		return func(document map[string]any) {
			for _, pin := range at(document, "resources", "tool.python") {
				change(pin.(map[string]any))
			}
		}
	}
	for name, change := range map[string]func(map[string]any){
		"another Python ABI": python(func(pin map[string]any) { pin["version"] = "3.9.0" }),
		"no Python on one system": func(document map[string]any) {
			delete(at(document, "resources", "tool.python"), "linux/arm64")
		},
	} {
		files := releasedFiles(t)
		edit(t, files, "python/pins.json", change)
		if _, err := catalog.Load(files); err == nil {
			t.Error("Python tool accepted with", name)
		}
	}
	files := releasedFiles(t)
	edit(t, files, "python/pins.json", python(func(pin map[string]any) { pin["sha256"] = strings.Repeat("b", 64) }))
	changed, err := catalog.Load(files)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := released(t).Pins.Archive("tool.yamllint", host(linux))
	after, _ := changed.Pins.Archive("tool.yamllint", host(linux))
	if catalog.ArchivePinID(before) == catalog.ArchivePinID(after) {
		t.Fatal("a Python change must change the identity of a Python tool")
	}
}

func TestLoadRejectsInvalidArchivePinFiles(t *testing.T) {
	ghostty := func(document map[string]any) map[string]any { return at(document, "resources", "tool.ghostty") }
	for name, change := range map[string]func(map[string]any){
		"schema":                      func(d map[string]any) { d["schema"] = 2 },
		"no family":                   func(d map[string]any) { d["resources"] = map[string]any{} },
		"family without a resource":   func(d map[string]any) { d["resources"].(map[string]any)["tool.missing"] = ghostty(d) },
		"family of another folder":    func(d map[string]any) { d["resources"].(map[string]any)["tool.nvim"] = ghostty(d) },
		"family without a platform":   func(d map[string]any) { d["resources"].(map[string]any)["tool.ghostty"] = map[string]any{} },
		"release without common pin":  func(d map[string]any) { delete(ghostty(d), "linux/amd64") },
		"unknown distribution":        func(d map[string]any) { ghostty(d)["linux/amd64/fedora-42"] = ghostty(d)["linux/amd64"] },
		"unsupported platform":        func(d map[string]any) { ghostty(d)["darwin/amd64"] = ghostty(d)["linux/amd64"] },
		"platform without a binding":  func(d map[string]any) { ghostty(d)["windows/amd64"] = ghostty(d)["linux/amd64"] },
		"malformed platform":          func(d map[string]any) { ghostty(d)["linux"] = ghostty(d)["linux/amd64"] },
		"plain HTTP":                  func(d map[string]any) { at(ghostty(d), "linux/amd64")["url"] = "http://example.com/ghostty.deb" },
		"short checksum":              func(d map[string]any) { at(ghostty(d), "linux/amd64")["sha256"] = "abc" },
		"unknown libc":                func(d map[string]any) { at(ghostty(d), "linux/amd64")["libc"] = "bionic" },
		"unknown field":               func(d map[string]any) { at(ghostty(d), "linux/amd64")["command"] = "sh" },
		"changed fixed recipe layout": func(d map[string]any) { at(ghostty(d), "linux/amd64")["bin_dirs"] = []any{"bin"} },
		"application bundle in a DEB": func(d map[string]any) { at(ghostty(d), "linux/amd64")["app_bundle"] = "Ghostty.app" },
		"macOS pin of a cask":         func(d map[string]any) { ghostty(d)["darwin/arm64"] = ghostty(d)["linux/amd64"] },
	} {
		files := releasedFiles(t)
		edit(t, files, "ghostty/pins.json", change)
		if _, err := catalog.Load(files); err == nil {
			t.Error("invalid archive pins accepted:", name)
		}
	}
	files := releasedFiles(t)
	edit(t, files, "obsidian/pins.json", func(d map[string]any) {
		delete(at(d, "resources", "tool.obsidian", "darwin/arm64"), "app_bundle")
	})
	if _, err := catalog.Load(files); err == nil {
		t.Error("invalid archive pins accepted: disk image without a bundle")
	}
	files = releasedFiles(t)
	files["zsh/pins.json"] = files["ghostty/pins.json"]
	if _, err := catalog.Load(files); err == nil {
		t.Error("a pin file in the folder of another tool was accepted")
	}
	files = releasedFiles(t)
	delete(files, "ghostty/pins.json")
	if c, err := catalog.Load(files); err != nil || len(c.Pins.Archives["tool.ghostty"]) != 0 {
		t.Error("a resource without pins must load and report no archive", err)
	}
}

func TestArchivePinValidationRejectsUnsafeLayoutsAndPreparations(t *testing.T) {
	pin := plainPin()
	if err := pin.Validate(); err != nil {
		t.Fatal(err)
	}
	legacy := catalog.ArchivePinID(pin)
	data, err := json.Marshal(pin)
	if err != nil || strings.Contains(string(data), "replacements") || strings.Contains(string(data), "excluded_files") || strings.Contains(string(data), "yamllint") {
		t.Fatal("an optional preparation changed the shape of a plain pin", err)
	}
	var restored catalog.ArchivePin
	if err := json.Unmarshal(data, &restored); err != nil || catalog.ArchivePinID(restored) != legacy {
		t.Fatal("pin identity changed after a round trip", err)
	}
	for name, change := range map[string]func(*catalog.ArchivePin){
		"no version":               func(p *catalog.ArchivePin) { p.Version = "" },
		"URL credentials":          func(p *catalog.ArchivePin) { p.URL = "https://user@example.com/tool" },
		"URL fragment":             func(p *catalog.ArchivePin) { p.URL = "https://example.com/tool#part" },
		"unknown format":           func(p *catalog.ArchivePin) { p.Format = "rar" },
		"deep strip":               func(p *catalog.ArchivePin) { p.Format, p.StripComponents = "tar.gz", 5 },
		"strip of a single file":   func(p *catalog.ArchivePin) { p.StripComponents = 1 },
		"no content":               func(p *catalog.ArchivePin) { p.Commands, p.BinDirs = nil, nil },
		"command without a folder": func(p *catalog.ArchivePin) { p.BinDirs = nil },
		"command name":             func(p *catalog.ArchivePin) { p.Commands = map[string]string{"Tool Name": "tool"} },
		"command escape":           func(p *catalog.ArchivePin) { p.Commands = map[string]string{"tool": "../tool"} },
		"binary folder escape":     func(p *catalog.ArchivePin) { p.BinDirs = []string{"/usr/bin"} },
		"required file escape":     func(p *catalog.ArchivePin) { p.RequiredFiles = []string{"a/../../b"} },
		"file name":                func(p *catalog.ArchivePin) { p.File = "NUL.txt" },
		"excluded escape":          func(p *catalog.ArchivePin) { p.ExcludedFiles = []string{"../outside"} },
		"excluded command":         func(p *catalog.ArchivePin) { p.ExcludedFiles = []string{"tool"} },
		"excluded required file":   func(p *catalog.ArchivePin) { p.RequiredFiles, p.ExcludedFiles = []string{"config"}, []string{"config"} },
		"repeated exclusion":       func(p *catalog.ArchivePin) { p.ExcludedFiles = []string{"tests/run", "tests/run"} },
		"replacement escape": func(p *catalog.ArchivePin) {
			p.Replacements = []catalog.ArchiveReplacement{{File: "../outside", Before: "a", After: "b"}}
		},
		"replacement without a match": func(p *catalog.ArchivePin) {
			p.Replacements = []catalog.ArchiveReplacement{{File: "tool", After: "b"}}
		},
		"replacement without a change": func(p *catalog.ArchivePin) {
			p.Replacements = []catalog.ArchiveReplacement{{File: "tool", Before: "a", After: "a"}}
		},
		"negative replacement count": func(p *catalog.ArchivePin) {
			p.Replacements = []catalog.ArchiveReplacement{{File: "tool", Before: "a", After: "b", Count: -1}}
		},
		"replacement in an excluded file": func(p *catalog.ArchivePin) {
			p.ExcludedFiles = []string{"test"}
			p.Replacements = []catalog.ArchiveReplacement{{File: "test", Before: "a", After: "b"}}
		},
		"Python library without an interpreter":           func(p *catalog.ArchivePin) { p.PythonStdlib = "lib/python3.14" },
		"Rust target without components":                  func(p *catalog.ArchivePin) { p.RustTarget = "x86_64-unknown-linux-gnu" },
		"Zen revision without its recipe":                 func(p *catalog.ArchivePin) { p.ZenProfileRevision = 1 },
		"PortableGit revision without its recipe":         func(p *catalog.ArchivePin) { p.PortableGitRevision = 1 },
		"Windows environment revision without its recipe": func(p *catalog.ArchivePin) { p.WindowsVenvRevision = 1 },
		"Windows Rust launcher without its toolchain": func(p *catalog.ArchivePin) {
			p.WindowsRustLauncher = &catalog.WindowsRustLauncherPin{Revision: 1}
		},
	} {
		invalid := plainPin()
		change(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Error("invalid pin accepted:", name)
		}
	}
	pin.Replacements = []catalog.ArchiveReplacement{{File: "tool", Before: "a", After: "b"}}
	if err := pin.Validate(); err != nil || catalog.ArchivePinID(pin) == legacy {
		t.Fatal("a reviewed replacement must be valid and part of the pin identity", err)
	}
	for _, name := range []string{"../escape", "/absolute", "a/../../escape", `a\escape`, "a:stream", "NUL.txt", "a./tool", "COM1", "a//tool", "lpt9.log", "tool ", "."} {
		if catalog.PortablePath(name) {
			t.Error("unsafe path accepted:", name)
		}
	}
	for _, name := range []string{"tool", "bin/tool.exe", "usr/share/ghostty/themes/Rose Pine", "COM10", "a.b/c"} {
		if !catalog.PortablePath(name) {
			t.Error("portable path rejected:", name)
		}
	}
}

func TestFixedRecipesRejectChangedOrCombinedPreparations(t *testing.T) {
	c := released(t)
	selected := func(family string, ctx platform.Context) catalog.ArchivePin {
		pin, err := c.Pins.Archive(family, host(ctx))
		if err != nil {
			t.Fatal(family, err)
		}
		data, err := json.Marshal(pin)
		if err != nil {
			t.Fatal(err)
		}
		var copied catalog.ArchivePin
		if err := json.Unmarshal(data, &copied); err != nil {
			t.Fatal(err)
		}
		return copied
	}
	for name, test := range map[string]struct {
		family string
		ctx    platform.Context
		change func(*catalog.ArchivePin)
	}{
		"yamllint source archive":  {"tool.yamllint", linux, func(p *catalog.ArchivePin) { p.File = "yamllint.tar.gz" }},
		"yamllint without Python":  {"tool.yamllint", linux, func(p *catalog.ArchivePin) { p.Yamllint.PythonPinID = "" }},
		"yamllint pathspec source": {"tool.yamllint", linux, func(p *catalog.ArchivePin) { p.Yamllint.Pathspec.File = "pathspec.tar.gz" }},
		"yamllint PyYAML source":   {"tool.yamllint", linux, func(p *catalog.ArchivePin) { p.Yamllint.PyYAML.File = "pyyaml-6.0.3.tar.gz" }},
		"yamllint nested recipe":   {"tool.yamllint", linux, func(p *catalog.ArchivePin) { p.Yamllint.PyYAML.Yamllint = &catalog.YamllintPin{} }},
		"yamllint dependency command": {"tool.yamllint", linux, func(p *catalog.ArchivePin) {
			p.Yamllint.PyYAML.Commands = map[string]string{"unreviewed": "unreviewed"}
		}},
		"yamllint with PortableGit":   {"tool.yamllint", linux, func(p *catalog.ArchivePin) { p.PortableGit = true }},
		"yamllint with latex2text":    {"tool.yamllint", linux, func(p *catalog.ArchivePin) { p.Latex2text = &catalog.Latex2textPin{} }},
		"yamllint second command":     {"tool.yamllint", linux, func(p *catalog.ArchivePin) { p.Commands["extra"] = "bin/extra" }},
		"yamllint old Windows recipe": {"tool.yamllint", windows, func(p *catalog.ArchivePin) { p.WindowsVenvRevision = 0 }},
		"latex2text wheel":            {"tool.latex2text", linux, func(p *catalog.ArchivePin) { p.File = "pylatexenc.whl" }},
		"latex2text backend source":   {"tool.latex2text", linux, func(p *catalog.ArchivePin) { p.Latex2text.Backend.File = "setuptools.tar.gz" }},
		"latex2text unknown console":  {"tool.latex2text", linux, func(p *catalog.ArchivePin) { p.Latex2text.ConsoleRevision = 2 }},
		"Rust component count":        {"tool.rust", linux, func(p *catalog.ArchivePin) { p.RustComponents = p.RustComponents[:4] }},
		"Rust component version":      {"tool.rust", linux, func(p *catalog.ArchivePin) { p.RustComponents[0].Version = "0.0.1" }},
		"Rust component order": {"tool.rust", linux, func(p *catalog.ArchivePin) {
			p.RustComponents[0], p.RustComponents[1] = p.RustComponents[1], p.RustComponents[0]
		}},
		"Rust unknown target":          {"tool.rust", linux, func(p *catalog.ArchivePin) { p.RustTarget = "riscv64gc-unknown-linux-gnu" }},
		"Rust launcher revision":       {"tool.rust", windows, func(p *catalog.ArchivePin) { p.WindowsRustLauncher.Revision = 2 }},
		"Rust launcher checksum":       {"tool.rust", windows, func(p *catalog.ArchivePin) { p.WindowsRustLauncher.SHA256 = "abc" }},
		"PortableGit another host":     {"tool.git", windows, func(p *catalog.ArchivePin) { p.URL = strings.Replace(p.URL, "github.com", "example.com", 1) }},
		"PortableGit missing runtime":  {"tool.git", windows, func(p *catalog.ArchivePin) { p.RequiredFiles = p.RequiredFiles[:3] }},
		"PortableGit unknown revision": {"tool.git", windows, func(p *catalog.ArchivePin) { p.PortableGitRevision = 2 }},
		"Zen unknown revision":         {"tool.zen", linux, func(p *catalog.ArchivePin) { p.ZenProfileRevision = 2 }},
		"Zen command":                  {"tool.zen", linux, func(p *catalog.ArchivePin) { p.Commands = map[string]string{"zen": "zen-bin"} }},
		"Ghostty missing runtime":      {"tool.ghostty", linux, func(p *catalog.ArchivePin) { p.RequiredFiles = []string{"usr/bin/ghostty-bin"} }},
		"T3 Code command":              {"tool.t3-code-app", linux, func(p *catalog.ArchivePin) { p.Commands, p.BinDirs = map[string]string{"t3": "t3-code"}, []string{"."} }},
		"Azure CLI Python recipe":      {"tool.az", linux, func(p *catalog.ArchivePin) { p.AzureCLI.Python.PythonStdlib = "lib/python3.14" }},
		"Azure CLI Python version":     {"tool.az", linux, func(p *catalog.ArchivePin) { p.AzureCLI.Python.Version = "3.13.1" }},
		"Azure CLI command":            {"tool.az", linux, func(p *catalog.ArchivePin) { p.Commands = map[string]string{"az": "bin/az"} }},
	} {
		pin := selected(test.family, test.ctx)
		if err := pin.Validate(); err != nil {
			t.Fatal(test.family, err)
		}
		test.change(&pin)
		if err := pin.Validate(); err == nil {
			t.Error("changed recipe accepted:", name)
		}
	}
	rust := selected("tool.rust", windows)
	if rust.WindowsRustLauncher == nil || rust.WindowsRustLauncher.SHA256 != "" {
		t.Fatal("the catalog must leave the launcher checksum to the process boundary")
	}
	rust.WindowsRustLauncher.SHA256 = strings.Repeat("c", 64)
	if err := rust.Validate(); err != nil {
		t.Fatal("a bound launcher checksum was rejected", err)
	}
	if got := catalog.RustComponentNames("x86_64-unknown-linux-gnu"); len(got) != 5 || got[1] != "rust-std-x86_64-unknown-linux-gnu" {
		t.Fatal("unexpected Rust components", got)
	}
}

func TestLoadRejectsInvalidSystemPinFiles(t *testing.T) {
	for name, test := range map[string]struct {
		file   string
		change func(map[string]any)
	}{
		"Code host":             {"vscode/linux-code-pins.json", func(d map[string]any) { at(d, "pins", "amd64")["url"] = "https://example.com/code_1.0.0-1_amd64.deb" }},
		"Code architecture":     {"vscode/linux-code-pins.json", func(d map[string]any) { at(d, "pins", "amd64")["architecture"] = "arm64" }},
		"Code size":             {"vscode/linux-code-pins.json", func(d map[string]any) { at(d, "pins", "amd64")["size"] = 0 }},
		"Code one architecture": {"vscode/linux-code-pins.json", func(d map[string]any) { delete(at(d, "pins"), "arm64") }},
		"Code undeclared modes": {"vscode/linux-code-pins.json", func(d map[string]any) { d["modes"] = map[string]any{} }},
		"Code sandbox mode":     {"vscode/linux-code-pins.json", func(d map[string]any) { at(d, "modes", "1.141.2-1791980535")["sandbox"] = "777" }},
		"Code root path":        {"vscode/linux-code-pins.json", func(d map[string]any) { at(d, "modes", "1.141.2-1791980535")["root_only"] = "/etc/passwd" }},
		"Code schema":           {"vscode/linux-code-pins.json", func(d map[string]any) { d["schema"] = 2 }},
		"Code unknown field":    {"vscode/linux-code-pins.json", func(d map[string]any) { d["source"] = "apt" }},
		"package URL": {"tailscale/linux-package-pins.json", func(d map[string]any) {
			at(d, "resources", "tool.tailscale", "amd64")["url"] = "https://example.com/tailscale_1.0.0_amd64.deb"
		}},
		"package name":         {"tailscale/linux-package-pins.json", func(d map[string]any) { at(d, "resources", "tool.tailscale", "amd64")["package"] = "other" }},
		"package architecture": {"tailscale/linux-package-pins.json", func(d map[string]any) { at(d, "resources", "tool.tailscale", "amd64")["architecture"] = "arm64" }},
		"package version":      {"tailscale/linux-package-pins.json", func(d map[string]any) { at(d, "resources", "tool.tailscale", "amd64")["version"] = "latest" }},
		"package of no resource": {"tailscale/linux-package-pins.json", func(d map[string]any) {
			d["resources"] = map[string]any{"tool.other": at(d, "resources", "tool.tailscale")}
		}},
		"package without a family": {"tailscale/linux-package-pins.json", func(d map[string]any) { d["resources"] = map[string]any{} }},
		"Build Tools components":   {"compiler/buildtools-pin.json", func(d map[string]any) { d["Components"] = []any{"Microsoft.VisualStudio.Workload.VCTools"} }},
		"Build Tools host":         {"compiler/buildtools-pin.json", func(d map[string]any) { d["URL"] = "https://example.com/vs_BuildTools.exe" }},
		"Build Tools checksum":     {"compiler/buildtools-pin.json", func(d map[string]any) { d["SHA256"] = "abc" }},
		"runtime file name": {"vcredist/vcredist-pin.json", func(d map[string]any) {
			d["url"] = "https://download.visualstudio.microsoft.com/download/pr/x/VC_redist.arm64.exe"
		}},
		"runtime version": {"vcredist/vcredist-pin.json", func(d map[string]any) { d["version"] = "15.0.0.0" }},
		"runtime bundle":  {"vcredist/vcredist-pin.json", func(d map[string]any) { d["bundle_id"] = "0e3bb569" }},
		"runtime size":    {"vcredist/vcredist-pin.json", func(d map[string]any) { d["size"] = 1 << 30 }},
		"application repository": {"t3-code-app/windows-app-pins.json", func(d map[string]any) {
			at(d, "app.t3-code-windows")["url"] = "https://github.com/other/t3code/releases/download/v0.1.8/T3-Code-0.1.8-x64.exe"
		}},
		"application executable":   {"t3-code-app/windows-app-pins.json", func(d map[string]any) { at(d, "app.t3-code-windows")["executable"] = `..\T3.exe` }},
		"application product code": {"t3-code-app/windows-app-pins.json", func(d map[string]any) { at(d, "app.t3-code-windows")["product_code"] = "{e9197887}" }},
		"application of no resource": {"t3-code-app/windows-app-pins.json", func(d map[string]any) {
			d["app.other"] = d["app.t3-code-windows"]
			delete(d, "app.t3-code-windows")
		}},
	} {
		files := releasedFiles(t)
		edit(t, files, test.file, test.change)
		if _, err := catalog.Load(files); err == nil {
			t.Error("invalid pin file accepted:", name)
		}
	}
	for _, file := range []string{"vscode/linux-code-pins.json", "tailscale/linux-package-pins.json", "compiler/buildtools-pin.json", "vcredist/vcredist-pin.json", "t3-code-app/windows-app-pins.json"} {
		files := releasedFiles(t)
		delete(files, file)
		if _, err := catalog.Load(files); err == nil {
			t.Error("a resource lost its pin file without an error:", file)
		}
		files = releasedFiles(t)
		files["zsh/"+file[strings.Index(file, "/")+1:]] = files[file]
		if _, err := catalog.Load(files); err == nil {
			t.Error("a pin file in a second folder was accepted:", file)
		}
	}
}
