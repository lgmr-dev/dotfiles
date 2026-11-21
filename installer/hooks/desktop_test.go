package hooks_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/system"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

var desktopPins = map[string]catalog.ArchivePin{
	"tool.ghostty":           {Commands: map[string]string{"ghostty": "usr/bin/ghostty"}, RequiredFiles: []string{"usr/share/icons/hicolor/256x256/apps/com.mitchellh.ghostty.png"}},
	"tool.zen":               {Commands: map[string]string{"zen": "zen"}, RequiredFiles: []string{"browser/chrome/icons/default/default128.png"}},
	"tool.openwhispr":        {Commands: map[string]string{"open-whispr": "open-whispr"}, RequiredFiles: []string{"resources/src/assets/icon.png"}},
	"tool.obsidian-portable": {RequiredFiles: []string{"obsidian", "resources/icon.png"}},
	"tool.t3-code-app":       {RequiredFiles: []string{"t3-code", "usr/share/icons/hicolor/256x256/apps/t3code.png"}},
	"tool.xremap":            {Commands: map[string]string{"xremap": "xremap"}},
	"vscode.rose-pine":       {RequiredFiles: []string{"extension/package.json"}},
	"tool.vscode":            {RequiredFiles: []string{"Code.exe"}},
	"tool.windows-terminal":  {RequiredFiles: []string{"WindowsTerminal.exe"}},
	"tool.aerospace":         {RequiredFiles: []string{"AeroSpace.app/Contents/Info.plist"}},
	"tool.obsidian":          {RequiredFiles: []string{"Obsidian.app/Contents/Info.plist"}},
}

func desktopBuild(t *testing.T, stateDir, id string) string {
	t.Helper()
	build := filepath.Join(stateDir, "tools", id, "1.0-aaaaaaaaaaaa-1")
	pin := desktopPins[id]
	for _, name := range pin.RequiredFiles {
		sentinelWrite(t, filepath.Join(build, filepath.FromSlash(name)), "fixture", 0644)
	}
	for _, name := range pin.Commands {
		sentinelWrite(t, filepath.Join(build, filepath.FromSlash(name)), "#!/bin/sh\n", 0755)
	}
	if err := os.Symlink(build, filepath.Join(stateDir, "tools", id, "current")); err != nil {
		t.Fatal(err)
	}
	return build
}

func desktopSkipWithoutPOSIXPaths(t *testing.T, provider string) {
	t.Helper()
	if os.PathSeparator != '/' {
		t.Skip(provider + " uses POSIX paths")
	}
}

func desktopProvider(e *sentinelEnv, os string) *hooks.Desktop {
	arch := "amd64"
	if os == "darwin" {
		arch = "arm64"
	}
	return &hooks.Desktop{Context: platform.Context{OS: os, Arch: arch}, Folders: platform.Folders{Home: e.home}, StateDir: e.state, Pins: desktopPins}
}

func desktopInspect(t *testing.T, p engine.Provider, e *sentinelEnv, id string, present bool) engine.Finding {
	t.Helper()
	finding, err := p.Inspect(context.Background(), catalog.Resource{ID: id}, engine.Want{Present: present}, e)
	if err != nil {
		t.Fatal(id, err)
	}
	return finding
}

func desktopApply(t *testing.T, p engine.Provider, e *sentinelEnv, id string) {
	t.Helper()
	if err := p.Apply(context.Background(), catalog.Resource{ID: id}, desktopInspect(t, p, e, id, true), e); err != nil {
		t.Fatal(id, err)
	}
	if finding := desktopInspect(t, p, e, id, true); finding.Status != engine.Satisfied {
		t.Fatal(id, "is not satisfied after its apply", finding)
	}
}

func desktopRemove(t *testing.T, p engine.Provider, e *sentinelEnv, id string) {
	t.Helper()
	if err := p.Remove(context.Background(), catalog.Resource{ID: id}, desktopInspect(t, p, e, id, false), e); err != nil {
		t.Fatal(id, err)
	}
	if finding := desktopInspect(t, p, e, id, false); finding.Status != engine.Absent || len(e.State().Targets) != 0 {
		t.Fatal(id, "left a record after its removal", finding, e.State().Targets)
	}
}

func TestDesktopLinuxEntriesNameTheStableCommandAndTheWindowClass(t *testing.T) {
	cases := []struct {
		id, pkg, command, file string
		want                   []string
	}{
		{"integration.ghostty", "tool.ghostty", "usr/bin/ghostty", "dotfiles-ghostty.desktop", []string{"Name=Ghostty\n", "StartupWMClass=com.mitchellh.ghostty\n", "Categories=Development;\n", "Terminal=false\n"}},
		{"desktop.zen", "tool.zen", "zen", "dotfiles-zen.desktop", []string{"\" %u\n", "StartupWMClass=zen\n", "Categories=Network;WebBrowser;\n", "MimeType=text/html;x-scheme-handler/http;x-scheme-handler/https;\n"}},
		{"desktop.openwhispr", "tool.openwhispr", "open-whispr", "dotfiles-openwhispr.desktop", []string{"StartupWMClass=open-whispr\n"}},
		{"desktop.obsidian", "tool.obsidian-portable", "obsidian", "dotfiles-obsidian.desktop", []string{"\" --no-sandbox %U\n", "StartupWMClass=md.obsidian.Obsidian\n", "Categories=Office;\nMimeType=x-scheme-handler/obsidian;\n"}},
		{"desktop.t3-code", "tool.t3-code-app", "t3-code", "dotfiles-t3-code-app.desktop", []string{"\" %U\n", "StartupWMClass=com.t3tools.T3Code\n", "Categories=Development;\nMimeType=x-scheme-handler/t3code;\n"}},
	}
	desktopSkipWithoutPOSIXPaths(t, "a Linux desktop entry")
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			e := newSentinelEnv(t)
			p := desktopProvider(e, "linux")
			if finding := desktopInspect(t, p, e, c.id, true); finding.Status != engine.Missing {
				t.Fatal("a missing entry is not reported", finding)
			}
			if sentinelExists(filepath.Join(e.home, ".local")) || sentinelExists(filepath.Join(e.state, "desktop")) {
				t.Fatal("the inspection prepared a file")
			}
			err := p.Apply(context.Background(), catalog.Resource{ID: c.id}, desktopInspect(t, p, e, c.id, true), e)
			if err == nil || !strings.Contains(err.Error(), "requires its verified private application") {
				t.Fatal("the entry was written without its application", err)
			}
			desktopBuild(t, e.state, c.pkg)
			desktopApply(t, p, e, c.id)
			entry := filepath.Join(e.home, ".local", "share", "applications", c.file)
			content := sentinelRead(t, entry)
			command := filepath.Join(e.state, "tools", c.pkg, "current", filepath.FromSlash(c.command))
			for _, want := range append(c.want, "[Desktop Entry]\nType=Application\n", "\nExec=\""+command+"\"", "\nIcon="+filepath.Join(e.state, "tools", c.pkg, "current")) {
				if !strings.Contains(content, want) {
					t.Fatalf("missing %q in\n%s", want, content)
				}
			}
			if strings.Count(content, "StartupWMClass=") != 1 || strings.Count(content, "\nExec=") != 1 {
				t.Fatal("the entry repeats a key", content)
			}
			if info, err := os.Stat(entry); err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("unexpected entry mode", info, err)
			}
			desktopRemove(t, p, e, c.id)
			if sentinelExists(entry) || sentinelExists(filepath.Join(e.home, ".local")) {
				t.Fatal("the removal left the entry or a folder that setup created")
			}
		})
	}
}

func TestDesktopEntryEscapesFieldCodesAndShellMetacharacters(t *testing.T) {
	desktopSkipWithoutPOSIXPaths(t, "a Linux desktop entry")
	e := newSentinelEnv(t)
	p := desktopProvider(e, "linux")
	p.StateDir = filepath.Join(e.home, `ü $x %f "x" \ state`)
	if _, err := p.Inspect(context.Background(), catalog.Resource{ID: "integration.ghostty"}, engine.Want{Present: true}, e); err != nil {
		t.Fatal(err)
	}
	build := filepath.Join(p.StateDir, "tools", "tool.ghostty", "1.0-aaaaaaaaaaaa-1")
	sentinelWrite(t, filepath.Join(build, "usr", "bin", "ghostty"), "#!/bin/sh\n", 0755)
	sentinelWrite(t, filepath.Join(build, "usr", "share", "icons", "hicolor", "256x256", "apps", "com.mitchellh.ghostty.png"), "", 0644)
	if err := os.Symlink(build, filepath.Join(p.StateDir, "tools", "tool.ghostty", "current")); err != nil {
		t.Fatal(err)
	}
	desktopApply(t, p, e, "integration.ghostty")
	entry := sentinelRead(t, filepath.Join(e.home, ".local", "share", "applications", "dotfiles-ghostty.desktop"))
	for _, expected := range []string{`%%f`, `\\$x`, `\\"x\\"`, `\\\\ state`, "\nIcon=" + filepath.Join(e.home, `ü $x %f "x" \\ state`)} {
		if !strings.Contains(entry, expected) {
			t.Fatalf("missing %q in %s", expected, entry)
		}
	}
	p.StateDir = filepath.Join(e.home, "x=y")
	if _, err := p.Inspect(context.Background(), catalog.Resource{ID: "integration.ghostty"}, engine.Want{Present: true}, e); err == nil || !strings.Contains(err.Error(), "invalid desktop entry path") {
		t.Fatal("accepted the forbidden Exec program character =", err)
	}
}

func TestDesktopKeepsAPersonalLauncherUntilItsReplacementIsApproved(t *testing.T) {
	e := newSentinelEnv(t)
	p := desktopProvider(e, "linux")
	desktopBuild(t, e.state, "tool.zen")
	entry := filepath.Join(e.home, ".local", "share", "applications", "dotfiles-zen.desktop")
	sentinelWrite(t, entry, "personal\n", 0644)
	finding := desktopInspect(t, p, e, "desktop.zen", true)
	if finding.Status != engine.Changed || len(finding.Preserved) != 1 {
		t.Fatal("a personal launcher is not preserved", finding)
	}
	if err := p.Apply(context.Background(), catalog.Resource{ID: "desktop.zen"}, finding, e); !errors.Is(err, targets.ErrChanged) || sentinelRead(t, entry) != "personal\n" {
		t.Fatal("the personal launcher was replaced without approval", err)
	}
	finding.Replace = true
	if err := p.Apply(context.Background(), catalog.Resource{ID: "desktop.zen"}, finding, e); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sentinelRead(t, entry), "Name=Zen\n") {
		t.Fatal("the approved replacement was not written")
	}
	sentinelWrite(t, entry, "edited after setup\n", 0600)
	if changed := desktopInspect(t, p, e, "desktop.zen", false); changed.Status != engine.Changed {
		t.Fatal("an edited launcher is not kept at removal", changed)
	}
	desktopApplyReplace(t, p, e, "desktop.zen")
	desktopRemove(t, p, e, "desktop.zen")
	if sentinelRead(t, entry) != "personal\n" {
		t.Fatal("the removal did not restore the personal launcher")
	}
}

func desktopApplyReplace(t *testing.T, p engine.Provider, e *sentinelEnv, id string) {
	t.Helper()
	finding := desktopInspect(t, p, e, id, true)
	finding.Replace = true
	if err := p.Apply(context.Background(), catalog.Resource{ID: id}, finding, e); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopMacKeyboardAutostartEntryShape(t *testing.T) {
	desktopSkipWithoutPOSIXPaths(t, "the Linux autostart entry")
	e := newSentinelEnv(t)
	p := desktopProvider(e, "linux")
	p.Folders.Config = filepath.Join(e.home, "xdg config")
	desktopBuild(t, e.state, "tool.xremap")
	desktopApply(t, p, e, "desktop.xremap")
	content := sentinelRead(t, filepath.Join(p.Folders.Config, "autostart", "dotfiles-mac-keyboard.desktop"))
	command := filepath.Join(e.state, "tools", "tool.xremap", "current", "xremap")
	if strings.Count(content, "\nExec=") != 1 || !strings.Contains(content, `Exec="/bin/sh" "-c" "case \\"\\$XDG_SESSION_TYPE\\"`) ||
		!strings.Contains(content, `--watch=config,device --no-window-logging \\"\\$config\\"" "`+command+`" "`+filepath.Join(p.Folders.Config, "xremap", "config.yml")+`"`+"\n") ||
		!strings.Contains(content, "Name=Mac keyboard shortcuts (dotfiles)\n") || !strings.HasSuffix(content, "Terminal=false\nNoDisplay=true\nX-GNOME-Autostart-enabled=true\nX-GNOME-AutoRestart=true\n") {
		t.Fatal("unexpected autostart entry", content)
	}
}

func TestDesktopRosePineExtensionIsALinkToTheStablePayload(t *testing.T) {
	e := newSentinelEnv(t)
	p := desktopProvider(e, "linux")
	desktopBuild(t, e.state, "vscode.rose-pine")
	desktopApply(t, p, e, "desktop.rose-pine")
	link := filepath.Join(e.home, ".vscode", "extensions", "dotfiles.rose-pine")
	if target, err := os.Readlink(link); err != nil || target != filepath.Join(e.state, "tools", "vscode.rose-pine", "current", "extension") {
		t.Fatal("unexpected extension link", target, err)
	}
	if record := e.State().Targets[link]; record.Kind != "link" || record.Resource != "desktop.rose-pine" {
		t.Fatal("the link is not a recorded link target", record)
	}
	desktopRemove(t, p, e, "desktop.rose-pine")
	if sentinelExists(link) {
		t.Fatal("the removal left the link")
	}
}

type desktopPowerShell struct {
	t       *testing.T
	runs    int
	corrupt bool
}

var desktopStage = regexp.MustCompile(`\$destination='([^']+)'`)

func (r *desktopPowerShell) Run(_ context.Context, c platform.Command) ([]byte, error) {
	r.t.Helper()
	r.runs++
	if len(c.Args) != 5 || c.Args[3] != "-EncodedCommand" {
		r.t.Fatal("unexpected PowerShell arguments", c.Args)
	}
	raw, err := base64.StdEncoding.DecodeString(c.Args[4])
	if err != nil {
		r.t.Fatal(err)
	}
	units := make([]uint16, len(raw)/2)
	for i := range units {
		units[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	script := string(utf16.Decode(units))
	match := desktopStage.FindStringSubmatch(script)
	if match == nil || !strings.Contains(script, "$link.IconLocation=$icon+',0'") || !strings.Contains(script, "$target='") {
		r.t.Fatal("unexpected shortcut script", script)
	}
	data := []byte("shortcut bytes " + match[1])
	if err := os.WriteFile(match[1], data, 0600); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if r.corrupt {
		sum = sha256.Sum256(nil)
	}
	return []byte("#< CLIXML\n<Objs Version=\"1.1.0.1\"></Objs>\n" + hex.EncodeToString(sum[:]) + "\r\n"), nil
}

func TestDesktopWindowsShortcutIsPreparedOneTimeAfterApproval(t *testing.T) {
	e := newSentinelEnv(t)
	p := desktopProvider(e, "windows")
	if _, err := p.Inspect(context.Background(), catalog.Resource{ID: "desktop.vscode"}, engine.Want{Present: true}, e); err == nil || !strings.Contains(err.Error(), "Programs known folder") {
		t.Fatal("setup invented a Start menu known folder", err)
	}
	runner := &desktopPowerShell{t: t, corrupt: true}
	p.Folders.Programs, p.PowerShell, p.Runner = filepath.Join(e.home, "Programs"), filepath.Join(e.home, "powershell.exe"), runner
	desktopBuild(t, e.state, "tool.vscode")
	finding := desktopInspect(t, p, e, "desktop.vscode", true)
	if finding.Status != engine.Missing || !strings.Contains(finding.Reason, "prepares the Start menu shortcut") || runner.runs != 0 || sentinelExists(filepath.Join(e.state, "desktop")) {
		t.Fatal("the inspection prepared the shortcut", finding)
	}
	shortcut := filepath.Join(p.Folders.Programs, "Dotfiles Visual Studio Code.lnk")
	err := p.Apply(context.Background(), catalog.Resource{ID: "desktop.vscode"}, finding, e)
	if err == nil || !strings.Contains(err.Error(), "shortcut bytes differ from native preparation result") || !strings.Contains(err.Error(), "PowerShell CLIXML: true") || sentinelExists(shortcut) {
		t.Fatal("a shortcut with other bytes than the native result was published", err)
	}
	if again := desktopInspect(t, p, e, "desktop.vscode", true); again.Token != finding.Token || again.Status != engine.Missing {
		t.Fatal("a failed preparation changed the inspection", again)
	}
	runner.corrupt = false
	desktopApply(t, p, e, "desktop.vscode")
	if runner.runs != 2 || !strings.HasPrefix(sentinelRead(t, shortcut), "shortcut bytes ") {
		t.Fatal("the shortcut was not published from the second preparation", runner.runs)
	}
	desktopApply(t, p, e, "desktop.vscode")
	if err := os.Remove(shortcut); err != nil {
		t.Fatal(err)
	}
	desktopApply(t, p, e, "desktop.vscode")
	if runner.runs != 2 {
		t.Fatal("a repair prepared the shortcut again", runner.runs)
	}
	desktopRemove(t, p, e, "desktop.vscode")
	inputs, err := os.ReadDir(filepath.Join(e.state, "desktop", "inputs"))
	if sentinelExists(shortcut) || err != nil || len(inputs) != 1 || !inputs[0].IsDir() {
		t.Fatal("the removal left the shortcut or its input, or deleted the failed attempt", inputs, err)
	}
}

type desktopCasks struct {
	specs     map[string]system.CaskSpec
	installed map[string]string
}

func (c *desktopCasks) Application(_ context.Context, r catalog.Resource, _ engine.Records) (string, error) {
	if path := c.installed[r.ID]; path != "" {
		return path, nil
	}
	return "", errors.New(c.specs[r.ID].Name + " has no verified application bundle")
}

func (c *desktopCasks) Cask(_ context.Context, r catalog.Resource) (system.CaskSpec, error) {
	return c.specs[r.ID], nil
}

func TestDesktopMacCommandLinksPointIntoTheCaskApplications(t *testing.T) {
	desktopSkipWithoutPOSIXPaths(t, "a macOS cask application")
	e := newSentinelEnv(t)
	p := desktopProvider(e, "darwin")
	casks := &desktopCasks{specs: system.MacCasks(e.home, "/opt/homebrew"), installed: map[string]string{}}
	p.Catalog = &catalog.Catalog{Resources: []catalog.Resource{{ID: "tool.vscode"}, {ID: "tool.ghostty"}, {ID: "tool.openwhispr"}}}
	p.Casks, p.CommandDirectory = casks, filepath.Join(e.home, ".local", "bin")
	for id, want := range map[string][2]string{
		"desktop.vscode":      {"code", "/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code"},
		"integration.ghostty": {"ghostty", filepath.Join(e.home, "Applications", "Ghostty.app", "Contents", "MacOS", "ghostty")},
	} {
		pkg := map[string]string{"desktop.vscode": "tool.vscode", "integration.ghostty": "tool.ghostty"}[id]
		before := desktopInspect(t, p, e, id, true)
		if before.Status != engine.Missing || !strings.Contains(before.Reason, "is not installed") {
			t.Fatal("a command link without its application is not missing", before)
		}
		casks.installed[pkg] = casks.specs[pkg].Application
		if id == "integration.ghostty" {
			casks.installed[pkg] = casks.specs[pkg].UserApplication
		}
		if after := desktopInspect(t, p, e, id, true); after.Token != before.Token || after.Status != engine.Missing {
			t.Fatal("the install of the application changed the token of its command link", after)
		}
		desktopApply(t, p, e, id)
		link := filepath.Join(p.CommandDirectory, want[0])
		if target, err := os.Readlink(link); err != nil || target != want[1] {
			t.Fatal("unexpected command link", target, err)
		}
		if sentinelExists(filepath.Join(p.CommandDirectory, "code-tunnel")) {
			t.Fatal("setup created a code-tunnel link")
		}
		desktopRemove(t, p, e, id)
		if sentinelExists(link) {
			t.Fatal("the removal left the command link")
		}
	}
	if finding := desktopInspect(t, p, e, "desktop.openwhispr", true); finding.Status != engine.Missing {
		t.Fatal("OpenWhispr without its application is not missing", finding)
	}
	casks.installed["tool.openwhispr"] = casks.specs["tool.openwhispr"].Application
	if finding := desktopInspect(t, p, e, "desktop.openwhispr", true); finding.Status != engine.Satisfied || len(e.State().Targets) != 0 {
		t.Fatal("OpenWhispr with its cask application needs a file", finding)
	}
}

func TestDesktopMacReleasesAnEarlierApplicationLinkAfterTheCaskIsPresent(t *testing.T) {
	e := newSentinelEnv(t)
	p := desktopProvider(e, "darwin")
	casks := &desktopCasks{specs: system.MacCasks(e.home, "/opt/homebrew"), installed: map[string]string{}}
	p.Catalog, p.Casks, p.CommandDirectory = &catalog.Catalog{Resources: []catalog.Resource{{ID: "tool.openwhispr"}}}, casks, filepath.Join(e.home, ".local", "bin")
	legacy, payload := filepath.Join(e.home, "Applications", "OpenWhispr.app"), filepath.Join(e.home, "payload", "OpenWhispr.app")
	sentinelWrite(t, filepath.Join(payload, "Contents", "Info.plist"), "", 0644)
	writer := &targets.Writer{StateDir: e.state}
	if err := writer.Converge(context.Background(), targets.Desired{Resource: "desktop.openwhispr", Kind: "link", Path: legacy, Link: payload}, e, targets.Preserve); err != nil {
		t.Fatal(err)
	}
	if finding := desktopInspect(t, p, e, "desktop.openwhispr", true); finding.Status != engine.Unhealthy || !sentinelExists(legacy) {
		t.Fatal("the earlier link does not stay until the cask application is present", finding)
	}
	casks.installed["tool.openwhispr"] = casks.specs["tool.openwhispr"].Application
	desktopApply(t, p, e, "desktop.openwhispr")
	if sentinelExists(legacy) || len(e.State().Targets) != 0 || !sentinelExists(payload) {
		t.Fatal("the earlier application link was not released", e.State().Targets)
	}
}

func TestDesktopMacApplicationLinkOfAPrivateBuild(t *testing.T) {
	e := newSentinelEnv(t)
	p := desktopProvider(e, "darwin")
	desktopBuild(t, e.state, "tool.aerospace")
	desktopApply(t, p, e, "desktop.aerospace")
	link := filepath.Join(e.home, "Applications", "AeroSpace.app")
	if target, err := os.Readlink(link); err != nil || target != filepath.Join(e.state, "tools", "tool.aerospace", "current", "AeroSpace.app") {
		t.Fatal("unexpected application link", target, err)
	}
	for _, id := range []string{"desktop.windows-terminal", "desktop.zen"} {
		if _, err := p.Inspect(context.Background(), catalog.Resource{ID: id}, engine.Want{Present: true}, e); err == nil {
			t.Fatal(id, "is offered on macOS")
		}
	}
	if !hooks.DesktopResource("integration.ghostty") || hooks.DesktopResource("tool.ghostty") {
		t.Fatal("unexpected desktop routing")
	}
}
