package system

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

var caskTestRequirements = map[string]struct{ token, application, command, requirement string }{
	"app.zen": {"zen", "/Applications/Zen.app", "",
		`anchor apple generic and identifier "app.zen-browser.zen" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "9V5K9TP787"`},
	"app.t3-code": {"t3-code", "/Applications/T3 Code (Beta).app", "",
		`anchor apple generic and identifier "com.t3tools.t3code" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "ARK85ZXQ4Z"`},
	"tool.vscode": {"visual-studio-code", "/Applications/Visual Studio Code.app", "/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code",
		`anchor apple generic and identifier "com.microsoft.VSCode" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "UBF8T346G9"`},
	"tool.ghostty": {"ghostty", "/Applications/Ghostty.app", "/Applications/Ghostty.app/Contents/MacOS/ghostty",
		`anchor apple generic and identifier "com.mitchellh.ghostty" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "24VZTF6M5V"`},
	"tool.openwhispr": {"openwhispr", "/Applications/OpenWhispr.app", "",
		`anchor apple generic and identifier "com.gizmolabs.openwhispr" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "T832773L2J"`},
}

func TestCaskRecipesHoldTheReviewedSigningRequirements(t *testing.T) {
	macTestSkipWithoutPOSIXPaths(t)
	specs := MacCasks("/Users/fixture", "/opt/homebrew")
	if len(specs) != len(caskTestRequirements) || len(specs) != len(catalog.ReviewedCasks) {
		t.Fatal("the reviewed casks, their recipes and this table differ", len(specs), len(catalog.ReviewedCasks))
	}
	for id, want := range caskTestRequirements {
		spec := specs[id]
		if err := spec.Validate(); err != nil {
			t.Fatal(id, err)
		}
		if spec.Resource != id || spec.Token != want.token || catalog.ReviewedCasks[id] != want.token || spec.Application != want.application || spec.Requirement() != want.requirement {
			t.Errorf("%s differs from its reviewed recipe: %+v\n%s", id, spec, spec.Requirement())
		}
		if spec.UserApplication != filepath.Join("/Users/fixture/Applications", filepath.Base(want.application)) || spec.Caskroom() != "/opt/homebrew/Caskroom/"+want.token {
			t.Error(id, spec.UserApplication, spec.Caskroom())
		}
		if got := spec.CommandTarget(spec.Application); got != want.command {
			t.Errorf("%s links its command to %q, want %q", id, got, want.command)
		}
		if strings.Contains(spec.Command+spec.CommandPath, "tunnel") {
			t.Error("setup creates no code-tunnel link")
		}
	}
	unreviewed := specs["tool.openwhispr"]
	unreviewed.Team = ""
	if err := unreviewed.Validate(); err == nil || !strings.Contains(err.Error(), "is not reviewed in this release") || unreviewed.Requirement() != "" {
		t.Fatal("a recipe without a reviewed signing identity was accepted", err)
	}
	query := func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("an unreviewed recipe reached codesign")
		return nil, nil
	}
	if _, _, err := CaskIdentity(context.Background(), query, unreviewed, t.TempDir()); err == nil {
		t.Fatal("an unreviewed recipe verified an application")
	}
	for _, change := range []func(*CaskSpec){
		func(s *CaskSpec) { s.Token = "Open Whispr" },
		func(s *CaskSpec) { s.Application = "Applications/OpenWhispr.app" },
		func(s *CaskSpec) { s.Bundle = `x" or anchor trusted` },
		func(s *CaskSpec) { s.Artifacts = []CaskArtifact{{"binary", "../outside", "bin/x"}} },
		func(s *CaskSpec) { s.Artifacts = []CaskArtifact{{"binary", "Contents/MacOS/x", "/usr/local/bin/x"}} },
		func(s *CaskSpec) { s.Command, s.CommandPath = "x", "../x" },
	} {
		invalid := specs["tool.openwhispr"]
		change(&invalid)
		if invalid.Validate() == nil {
			t.Fatalf("invalid recipe accepted: %+v", invalid)
		}
	}
}

func TestCaskMovesInTheReleasedCatalog(t *testing.T) {
	c, err := catalog.Load(os.DirFS(filepath.Join("..", "tools")))
	if err != nil {
		t.Fatal(err)
	}
	darwin := platform.Context{OS: "darwin", Arch: "arm64"}
	for id, token := range catalog.ReviewedCasks {
		r, ok := c.Resource(id)
		if !ok || r.Bindings["darwin"] != (catalog.Binding{Provider: "homebrew-cask", Package: token}) || !slices.Contains(r.Dependencies(darwin), "infra.homebrew") {
			t.Errorf("%s has no reviewed cask binding with Homebrew on macOS: %+v", id, r.Bindings["darwin"])
		}
		if _, pinned := c.Pins.Archives[id]["darwin/arm64"]; pinned {
			t.Errorf("%s keeps a macOS archive pin beside its cask", id)
		}
		if provider, name, ok := engine.SystemRecord(r, darwin); !ok || provider != "cask" || name != token {
			t.Error("the engine does not map the cask record", id, provider, name)
		}
	}
	for _, id := range []string{"tool.vscode", "tool.ghostty", "tool.openwhispr"} {
		if _, moved := catalog.ReviewedCasks[id]; !moved {
			t.Error("the application did not move to a cask on macOS:", id)
		}
	}
}

func caskTestMetadata(t *testing.T, token string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "casks", token+".json"))
	if err != nil {
		t.Fatal(err)
	}
	document := map[string]any{}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func caskTestJSON(t *testing.T, document any, replace ...string) []byte {
	t.Helper()
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.NewReplacer(replace...).Replace(string(data)))
}

func TestCaskMetadataAcceptsTheReviewedRecipesInBothPathForms(t *testing.T) {
	macTestSkipWithoutPOSIXPaths(t)
	for id, spec := range MacCasks("/Users/fixture", "/opt/homebrew") {
		document := caskTestMetadata(t, spec.Token)
		for _, installed := range []bool{false, true} {
			if err := ValidateCaskMetadata(caskTestJSON(t, document), spec, installed); err != nil {
				t.Error(id, "public metadata refused:", err)
			}
			if err := ValidateCaskMetadata(caskTestJSON(t, document, "$APPDIR", "/Applications", "$HOMEBREW_PREFIX", "/opt/homebrew"), spec, installed); err != nil {
				t.Error(id, "expanded metadata refused:", err)
			}
		}
		kinds := map[string]int{}
		for _, artifact := range document["artifacts"].([]any) {
			for kind := range artifact.(map[string]any) {
				if kind != "target" {
					kinds[kind]++
				}
			}
		}
		reviewed := map[string]int{"app": 1, "zap": kinds["zap"], "uninstall": kinds["uninstall"]}
		for _, artifact := range spec.Artifacts {
			if kinds[artifact.Kind] != 0 {
				reviewed[artifact.Kind]++
			}
		}
		for kind, count := range kinds {
			if reviewed[kind] != count {
				t.Errorf("%s: the public cask has %d %s artifacts, the reviewed recipe covers %d", id, count, kind, reviewed[kind])
			}
		}
	}
	specs := MacCasks("/Users/fixture", "/opt/homebrew")
	if kinds := specs["tool.vscode"].Artifacts; len(kinds) != 2 || kinds[0].Kind != "binary" || kinds[1].Kind != "binary" || kinds[0].Active() {
		t.Fatal("VS Code has an application, two binary entries and its uninstall entry", kinds)
	}
	active := 0
	for _, artifact := range specs["tool.ghostty"].Artifacts {
		if artifact.Active() {
			active++
		}
	}
	if active != 5 || len(specs["tool.openwhispr"].Artifacts) != 0 {
		t.Fatal("Ghostty has two manual pages and three completions, OpenWhispr only an application", active)
	}
}

func TestCaskMetadataRefusesEachChangeOfTheReviewedContract(t *testing.T) {
	macTestSkipWithoutPOSIXPaths(t)
	specs := MacCasks("/Users/fixture", "/opt/homebrew")
	artifacts := func(d map[string]any) []any { return d["artifacts"].([]any) }
	find := func(d map[string]any, kind string) map[string]any {
		for _, artifact := range artifacts(d) {
			if _, ok := artifact.(map[string]any)[kind]; ok {
				return artifact.(map[string]any)
			}
		}
		t.Fatal("the fixture has no artifact", kind)
		return nil
	}
	add := func(artifact map[string]any) func(map[string]any) {
		return func(d map[string]any) { d["artifacts"] = append(artifacts(d), artifact) }
	}
	for name, test := range map[string]struct {
		id        string
		change    func(map[string]any)
		installed bool
	}{
		"other tap":          {"app.zen", func(d map[string]any) { d["tap"] = "other/tap" }, true},
		"other token":        {"app.zen", func(d map[string]any) { d["token"] = "zen-nightly" }, true},
		"tapped token":       {"app.zen", func(d map[string]any) { d["full_token"] = "other/tap/zen" }, true},
		"no checksum":        {"tool.vscode", func(d map[string]any) { d["sha256"] = "no_check" }, false},
		"no updater":         {"tool.vscode", func(d map[string]any) { d["auto_updates"] = false }, false},
		"formula dependency": {"tool.ghostty", func(d map[string]any) { d["depends_on"] = map[string]any{"formula": []string{"other"}} }, false},
		"cask dependency": {"tool.ghostty", func(d map[string]any) {
			d["depends_on"] = map[string]any{"cask": []string{"other"}, "macos": map[string]any{}}
		}, false},
		"package installer":    {"tool.openwhispr", add(map[string]any{"pkg": []string{"OpenWhispr.pkg"}}), true},
		"installer script":     {"tool.openwhispr", add(map[string]any{"installer": []any{map[string]any{"script": map[string]any{"executable": "install.sh"}}}}), true},
		"preflight":            {"tool.openwhispr", add(map[string]any{"preflight": nil}), true},
		"uninstall postflight": {"tool.openwhispr", add(map[string]any{"uninstall_postflight": nil}), true},
		"unreviewed binary":    {"tool.vscode", add(map[string]any{"binary": []string{"$APPDIR/Visual Studio Code.app/Contents/MacOS/Electron"}, "target": "$HOMEBREW_PREFIX/bin/electron"}), true},
		"binary for an app":    {"tool.openwhispr", add(map[string]any{"binary": []string{"$APPDIR/OpenWhispr.app/Contents/MacOS/OpenWhispr"}}), true},
		"duplicate binary":     {"tool.vscode", func(d map[string]any) { d["artifacts"] = append(artifacts(d), find(d, "binary")) }, true},
		"redirected binary":    {"tool.vscode", func(d map[string]any) { find(d, "binary")["target"] = "$HOMEBREW_PREFIX/bin/git" }, true},
		"redirected manual":    {"tool.ghostty", func(d map[string]any) { find(d, "manpage")["target"] = "/usr/local/share/man/man5/ghostty.5" }, true},
		"other completion": {"tool.ghostty", func(d map[string]any) {
			find(d, "zsh_completion")["zsh_completion"] = []string{"$APPDIR/Ghostty.app/Contents/Resources/zsh/site-functions/_git"}
		}, true},
		"redirected app": {"tool.ghostty", func(d map[string]any) { find(d, "app")["target"] = "/Applications/Other.app" }, true},
		"other app":      {"tool.ghostty", func(d map[string]any) { find(d, "app")["app"] = []string{"Other.app"} }, true},
		"duplicate app":  {"tool.ghostty", func(d map[string]any) { d["artifacts"] = append(artifacts(d), find(d, "app")) }, true},
		"no app":         {"tool.openwhispr", func(d map[string]any) { d["artifacts"] = []any{} }, true},
		"uninstall script": {"tool.ghostty", func(d map[string]any) {
			find(d, "uninstall")["uninstall"] = []any{map[string]any{"quit": "com.mitchellh.ghostty", "script": "remove.sh"}}
		}, true},
		"uninstall delete": {"tool.vscode", func(d map[string]any) {
			find(d, "uninstall")["uninstall"] = []any{map[string]any{"launchctl": "com.microsoft.VSCode.ShipIt", "quit": "com.microsoft.VSCode", "delete": "/Library/Other"}}
		}, true},
		"uninstall for an app": {"tool.openwhispr", add(map[string]any{"uninstall": []any{map[string]any{"delete": "/Library/Other"}}}), true},
		"two uninstall":        {"tool.ghostty", func(d map[string]any) { d["artifacts"] = append(artifacts(d), find(d, "uninstall")) }, true},
		"zap script":           {"tool.ghostty", func(d map[string]any) { find(d, "zap")["zap"] = []any{map[string]any{"script": "zap.sh"}} }, true},
		"ambiguous artifact":   {"tool.ghostty", func(d map[string]any) { find(d, "app")["binary"] = []string{"x"} }, true},
		"wrapper of another": {"app.zen", func(d map[string]any) {
			find(d, "command_wrapper")["command_wrapper"] = []any{"zen", map[string]any{"executable": "$APPDIR/Zen.app/Contents/MacOS/other"}}
		}, true},
	} {
		spec := specs[test.id]
		document := caskTestMetadata(t, spec.Token)
		test.change(document)
		for _, installed := range []bool{false, true} {
			err := ValidateCaskMetadata(caskTestJSON(t, document), spec, installed)
			if rejected := !installed || test.installed; (err != nil) != rejected {
				t.Errorf("%s (installed %v): %v", name, installed, err)
			}
		}
	}
	if err := ValidateCaskMetadata([]byte(strings.Repeat(" ", 1<<20+1)), specs["app.zen"], true); err == nil {
		t.Fatal("oversized metadata was accepted")
	}
}

func TestCaskIdentityUsesTheExactRequirementAndNeverAcceptsALink(t *testing.T) {
	root := macTestRoot(t)
	spec := macCasks(filepath.Join(root, "home"), filepath.Join(root, "homebrew"), filepath.Join(root, "Applications"))["tool.ghostty"]
	var signature error
	calls := [][]string{}
	query := func(_ context.Context, program string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{program}, args...))
		return nil, signature
	}
	if present, healthy, err := CaskIdentity(context.Background(), query, spec, spec.Application); present || healthy || err != nil || len(calls) != 0 {
		t.Fatal("an absent application", present, healthy, err)
	}
	if err := os.MkdirAll(spec.Application, 0755); err != nil {
		t.Fatal(err)
	}
	if present, healthy, err := CaskIdentity(context.Background(), query, spec, spec.Application); !present || !healthy || err != nil {
		t.Fatal(present, healthy, err)
	}
	want := []string{"/usr/bin/codesign", "--verify", "--deep", "--strict=symlinks", "-R", "=" + caskTestRequirements["tool.ghostty"].requirement, spec.Application}
	if !reflect.DeepEqual(calls, [][]string{want}) {
		t.Fatal("unexpected signature check", calls)
	}
	for _, code := range []int{1, 3} {
		signature = macTestExit(code)
		if present, healthy, err := CaskIdentity(context.Background(), query, spec, spec.Application); !present || healthy || err != nil {
			t.Fatal("a failed signature must be a present and unverified application", code, present, healthy, err)
		}
	}
	for _, failure := range []error{macTestExit(2), errors.New("codesign did not start")} {
		signature = failure
		if _, healthy, err := CaskIdentity(context.Background(), query, spec, spec.Application); healthy || err == nil {
			t.Fatal("an inspection failure proved an identity", failure)
		}
	}
	signature, calls = nil, nil
	link := filepath.Join(root, "home", "Applications", "Ghostty.app")
	if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(spec.Application, link); err != nil {
		t.Fatal(err)
	}
	if present, healthy, err := CaskIdentity(context.Background(), query, spec, link); !present || healthy || err != nil || len(calls) != 0 {
		t.Fatal("the application link of release v2 satisfied the cask", present, healthy, err)
	}
}

type caskFixture struct {
	*brewFixture
	r    catalog.Resource
	spec CaskSpec
}

func newCaskFixture(t *testing.T, id string) *caskFixture {
	t.Helper()
	f := &caskFixture{brewFixture: newBrewFixture(t)}
	f.spec = macCasks(f.d.Home, f.location.Prefix, f.d.appdir)[id]
	f.r = catalog.Resource{ID: id, Name: f.spec.Name, Action: "package", PlatformRequires: map[string][]string{"darwin": {"infra.homebrew"}},
		Bindings: map[string]catalog.Binding{"darwin": {Provider: "homebrew-cask", Package: f.spec.Token}}}
	document := map[string]any{}
	if err := json.Unmarshal(caskTestJSON(t, caskTestMetadata(t, f.spec.Token), `"/Applications/`, `"`+f.d.appdir+`/`), &document); err != nil {
		t.Fatal(err)
	}
	f.metadata[f.spec.Token] = document
	f.installed["outside"] = brewPackage{Name: "outside", Source: "outside", Version: "1", Healthy: true}
	f.onRun = func(args []string) error {
		f.publish()
		return nil
	}
	return f
}

func (f *caskFixture) publish() {
	f.t.Helper()
	if err := os.MkdirAll(f.spec.Application, 0755); err != nil {
		f.t.Fatal(err)
	}
	appdir := filepath.Dir(f.spec.Application)
	metadata := filepath.Join(f.spec.Caskroom(), ".metadata")
	macTestWrite(f.t, filepath.Join(metadata, "config.json"), string(caskTestJSON(f.t, map[string]any{"default": map[string]string{"appdir": appdir}, "env": map[string]string{}, "explicit": map[string]string{"appdir": appdir}})), 0644)
	macTestWrite(f.t, filepath.Join(metadata, "1", "timestamp", "Casks", f.spec.Token+".json"), string(caskTestJSON(f.t, f.metadata[f.spec.Token])), 0644)
	for _, artifact := range f.spec.Artifacts {
		if artifact.Active() {
			if err := os.MkdirAll(filepath.Dir(f.spec.target(artifact)), 0755); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Symlink(f.spec.source(artifact), f.spec.target(artifact)); err != nil && !errors.Is(err, os.ErrExist) {
				f.t.Fatal(err)
			}
		}
	}
	f.installed["cask/"+f.spec.Token] = brewPackage{Name: "cask/" + f.spec.Token, Source: f.spec.Token, Version: "1", Healthy: true}
}

func (f *caskFixture) record() {
	f.t.Helper()
	if err := f.rec.Update(func(s *state.State) error { s.AddSystem("cask", f.spec.Token); return nil }); err != nil {
		f.t.Fatal(err)
	}
}

func (f *caskFixture) asked(program string) bool {
	return slices.ContainsFunc(f.queries, func(query []string) bool { return query[0] == program })
}

func TestCaskInstallArgumentsMarkAndVerification(t *testing.T) {
	for id := range catalog.ReviewedCasks {
		t.Run(id, func(t *testing.T) {
			f := newCaskFixture(t, id)
			f.running = true
			want := engine.Want{Present: true, Update: true}
			missing := f.inspect(f.r, want)
			if missing.Status != engine.Missing || !missing.Pending || missing.Privileged || len(brewTestBlocks(missing, "setup")) != 1 || len(brewTestBlocks(missing, "you")) != 0 {
				t.Fatal(missing)
			}
			line := brewTestBlocks(missing, "setup")[0]
			if err := f.d.Apply(context.Background(), f.r, missing, f.rec); err != nil {
				t.Fatal(err)
			}
			if line != macTestLine(f.runner.commands[0]) {
				t.Fatal("the approved line differs from the command that ran", line, macTestLine(f.runner.commands[0]))
			}
			if !reflect.DeepEqual(f.mutations(), [][]string{{"install", "--cask", "--appdir=" + f.d.appdir, "--no-binaries", "--require-sha", f.spec.Token}}) {
				t.Fatal(f.mutations())
			}
			for _, control := range append([]string{"HOMEBREW_CASK_OPTS=", "HOMEBREW_NO_SUDO=1"}, brewProcessControls...) {
				if !slices.Contains(f.runner.commands[0].Env, control) {
					t.Fatal("the cask command lacks a process control", control)
				}
			}
			if !slices.Equal(f.states[0].Pending, []string{id + ":brew-install"}) || !f.states[0].HasSystem("cask", f.spec.Token) {
				t.Fatal("the mark and the provisional record must exist before brew starts", f.states[0].Pending, f.states[0].System)
			}
			if st := f.rec.State(); len(st.Pending) != 0 || !st.HasSystem("cask", f.spec.Token) {
				t.Fatal(st.Pending, st.System)
			}
			if f.asked("/usr/bin/osascript") {
				t.Fatal("a new install beside a running copy needs no running-application guard")
			}
			if !slices.ContainsFunc(f.queries, func(q []string) bool {
				return slices.Equal(q, []string{"brew", "info", "--json=v2", "--cask", f.spec.Token})
			}) {
				t.Fatal("the selected metadata was not checked before the change")
			}
			after := f.inspect(f.r, want)
			if after.Status != engine.Satisfied || after.Pending || len(after.Commands) != 0 || after.Version != "" {
				t.Fatal(after)
			}
			application, err := f.d.Application(context.Background(), f.r, f.rec)
			if err != nil || application != f.spec.Application {
				t.Fatal(application, err)
			}
			if spec, err := f.d.Cask(context.Background(), f.r); err != nil || !reflect.DeepEqual(spec, f.spec) {
				t.Fatal(spec, err)
			}
			updated := f.installed["cask/"+f.spec.Token]
			updated.Version = "2"
			f.installed["cask/"+f.spec.Token] = updated
			macTestWrite(t, filepath.Join(f.spec.Application, "Contents", "new-version"), "2", 0644)
			if later := f.inspect(f.r, want); later.Status != engine.Satisfied || later.Token != after.Token {
				t.Fatal("a self-update of the application changed the inspection", later)
			}
			if err := f.d.Apply(context.Background(), f.r, engine.Finding{Status: engine.Outdated}, f.rec); err == nil || len(f.runner.commands) != 1 {
				t.Fatal("setup updated an application that updates itself", err)
			}
		})
	}
}

func TestBrewNeverUsesAForbiddenFlagOrARemoval(t *testing.T) {
	commands := []platform.Command{}
	git := brewTestFormula("tool.git", "git")
	for _, status := range []engine.Status{engine.Missing, engine.Unhealthy, engine.Outdated} {
		f := newBrewFixture(t)
		if status != engine.Missing {
			f.installed["git"] = brewPackage{Name: "git", Source: "git", Version: "1", Healthy: status == engine.Outdated}
			if err := f.rec.Update(func(s *state.State) error { s.AddSystem("brew", "git"); return nil }); err != nil {
				t.Fatal(err)
			}
		}
		f.onRun = func([]string) error {
			f.installed["git"] = brewPackage{Name: "git", Source: "git", Version: "2", Healthy: true}
			return nil
		}
		if err := f.d.Apply(context.Background(), git, engine.Finding{Status: status}, f.rec); err != nil {
			t.Fatal(status, err)
		}
		commands = append(commands, f.runner.commands...)
	}
	for id := range catalog.ReviewedCasks {
		for _, reinstall := range []bool{false, true} {
			f := newCaskFixture(t, id)
			status := engine.Missing
			if reinstall {
				f.publish()
				f.record()
				if err := os.RemoveAll(f.spec.Application); err != nil {
					t.Fatal(err)
				}
				status = engine.Unhealthy
			}
			if err := f.d.Apply(context.Background(), f.r, engine.Finding{Status: status}, f.rec); err != nil {
				t.Fatal(id, reinstall, err)
			}
			commands = append(commands, f.runner.commands...)
		}
	}
	verbs := map[string]bool{}
	for _, c := range commands {
		verbs[c.Args[0]] = true
		if !slices.Contains(c.Env, "HOMEBREW_NO_AUTOREMOVE=1") {
			t.Fatal("a brew command can remove unused packages", c.Args)
		}
		for _, argument := range c.Args {
			if argument == "--force" || argument == "-f" || argument == "--ignore-dependencies" || argument == "--zap" || argument == "zap" || argument == "uninstall" || argument == "remove" || argument == "autoremove" || argument == "cleanup" || argument == "--binaries" || argument == "--adopt" {
				t.Fatal("forbidden brew argument", c.Args)
			}
		}
		if slices.Contains(c.Args, "--cask") && (!slices.Contains(c.Args, "--require-sha") || !slices.Contains(c.Args, "--no-binaries")) {
			t.Fatal("a cask command without its guards", c.Args)
		}
	}
	if !reflect.DeepEqual(verbs, map[string]bool{"install": true, "reinstall": true, "upgrade": true}) || len(commands) != 13 {
		t.Fatal("the test did not cover each brew command", verbs, len(commands))
	}
}

func TestCaskRunningApplicationGuardCoversAReinstall(t *testing.T) {
	f := newCaskFixture(t, "tool.ghostty")
	f.publish()
	f.record()
	if err := os.RemoveAll(f.spec.Application); err != nil {
		t.Fatal(err)
	}
	unhealthy := f.inspect(f.r, engine.Want{Present: true})
	if unhealthy.Status != engine.Unhealthy || !unhealthy.Pending || brewTestBlocks(unhealthy, "setup")[0] != f.line(true, "reinstall", "--cask", "--appdir="+f.d.appdir, "--no-binaries", "--require-sha", "ghostty") {
		t.Fatal(unhealthy)
	}
	f.running = true
	if err := f.d.Apply(context.Background(), f.r, unhealthy, f.rec); err == nil || !strings.Contains(err.Error(), "quit all copies of Ghostty") || len(f.runner.commands) != 0 || len(f.rec.State().Pending) != 0 {
		t.Fatal("a reinstall ran while the application was open", err)
	}
	if !slices.ContainsFunc(f.queries, func(q []string) bool {
		return q[0] == "/usr/bin/osascript" && q[len(q)-1] == "com.mitchellh.ghostty" && slices.Contains(q, macApplicationScript)
	}) {
		t.Fatal("the guard did not ask for the bundle identifier of the recipe")
	}
	f.running = false
	if err := f.d.Apply(context.Background(), f.r, unhealthy, f.rec); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.mutations(), [][]string{{"reinstall", "--cask", "--appdir=" + f.d.appdir, "--no-binaries", "--require-sha", "ghostty"}}) || !slices.Equal(f.states[0].Pending, []string{"tool.ghostty:brew-repair"}) {
		t.Fatal(f.mutations(), f.states[0].Pending)
	}
	if found := f.inspect(f.r, engine.Want{Present: true}); found.Status != engine.Satisfied {
		t.Fatal(found)
	}

	f = newCaskFixture(t, "tool.ghostty")
	f.publish()
	if err := os.RemoveAll(f.spec.Application); err != nil {
		t.Fatal(err)
	}
	found := f.inspect(f.r, engine.Want{Present: true})
	if found.Status != engine.Blocked || !strings.Contains(found.Reason, "has no app") || len(brewTestBlocks(found, "setup")) != 0 {
		t.Fatal("setup repaired a cask without a record", found)
	}
	wantBlocks := []engine.CommandBlock{
		{Title: "Restore Ghostty with Homebrew", By: "you", Lines: []string{f.line(true, "reinstall", "--cask", "--appdir="+f.d.appdir, "--no-binaries", "--require-sha", "ghostty")}},
		{Title: "Remove Ghostty with Homebrew", By: "you", Lines: []string{f.line(true, "uninstall", "--cask", "ghostty")}},
	}
	if !reflect.DeepEqual(found.Commands, wantBlocks) {
		t.Fatalf("the two commands exclude each other and need one block each: %+v", found.Commands)
	}
	for _, status := range []engine.Status{engine.Unhealthy, engine.Missing} {
		if err := f.d.Apply(context.Background(), f.r, engine.Finding{Status: status}, f.rec); err == nil || len(f.runner.commands) != 0 || len(f.rec.State().Pending) != 0 {
			t.Fatal("setup changed a cask that Homebrew holds without a record of setup", status, err)
		}
	}
}

func TestCaskKeepsExistingApplicationsCommandsAndFiles(t *testing.T) {
	want := engine.Want{Present: true}
	f := newCaskFixture(t, "tool.vscode")
	macTestWrite(t, filepath.Join(f.location.Prefix, "bin", "code"), "personal command", 0755)
	found := f.inspect(f.r, want)
	if found.Status != engine.Blocked || !strings.Contains(found.Reason, "preserve the existing VS Code command") {
		t.Fatal(found)
	}
	if err := f.d.Apply(context.Background(), f.r, engine.Finding{Status: engine.Missing}, f.rec); err == nil || len(f.runner.commands) != 0 || len(f.rec.State().Pending) != 0 {
		t.Fatal("an existing command did not stop the install", err)
	}

	f = newCaskFixture(t, "tool.ghostty")
	macTestWrite(t, filepath.Join(f.location.Prefix, "share", "man", "man1", "ghostty.1"), "personal manual", 0644)
	if found := f.inspect(f.r, want); found.Status != engine.Blocked || !strings.Contains(found.Reason, "ghostty.1") {
		t.Fatal("a destination that stays active with --no-binaries must be free", found)
	}

	f = newCaskFixture(t, "tool.ghostty")
	payload := filepath.Join(f.root, "v2", "Ghostty.app")
	if err := os.MkdirAll(payload, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(payload, f.spec.UserApplication); err != nil {
		t.Fatal(err)
	}
	if found := f.inspect(f.r, want); found.Status != engine.Missing {
		t.Fatal("the application link of release v2 must not satisfy or stop the cask", found)
	}
	if err := f.apply(f.r, want); err != nil {
		t.Fatal("the cask must install beside the link of release v2", err)
	}
	if target, err := os.Readlink(f.spec.UserApplication); err != nil || target != payload {
		t.Fatal("the link of release v2 changed", target, err)
	}

	f = newCaskFixture(t, "tool.openwhispr")
	if err := os.Symlink(f.root, f.spec.Application); err != nil {
		t.Fatal(err)
	}
	if found := f.inspect(f.r, want); found.Status != engine.Blocked || !strings.Contains(found.Reason, "occupied by an unverified app or link") {
		t.Fatal(found)
	}

	f = newCaskFixture(t, "tool.openwhispr")
	if err := os.MkdirAll(f.spec.Application, 0755); err != nil {
		t.Fatal(err)
	}
	f.signature = macTestExit(1)
	if found := f.inspect(f.r, want); found.Status != engine.Blocked {
		t.Fatal("an unsigned application was accepted or overwritten", found)
	}
	f.signature = errors.New("codesign is unavailable")
	if found := f.inspect(f.r, want); found.Status != engine.Unknown || !strings.Contains(found.Reason, "could not be inspected") {
		t.Fatal(found)
	}

	f = newCaskFixture(t, "tool.openwhispr")
	if err := os.MkdirAll(f.spec.UserApplication, 0755); err != nil {
		t.Fatal(err)
	}
	found = f.inspect(f.r, want)
	if found.Status != engine.Satisfied || len(found.Commands) != 0 || len(found.Notes) != 1 || !strings.Contains(found.Notes[0], f.spec.UserApplication) {
		t.Fatal("a real signed bundle of the operator must satisfy the cask", found)
	}
	if application, err := f.d.Application(context.Background(), f.r, f.rec); err != nil || application != f.spec.UserApplication {
		t.Fatal(application, err)
	}
	if err := f.d.Apply(context.Background(), f.r, engine.Finding{Status: engine.Missing}, f.rec); err == nil || len(f.runner.commands) != 0 || f.rec.State().HasSystem("cask", "openwhispr") {
		t.Fatal("setup installed over an application of the operator", err)
	}
}

func TestCaskChecksTheSelectedMetadataBeforeAndTheInstalledRecipeAfter(t *testing.T) {
	want := engine.Want{Present: true}
	f := newCaskFixture(t, "tool.vscode")
	f.metadata["visual-studio-code"].(map[string]any)["auto_updates"] = false
	if err := f.apply(f.r, want); err == nil || !strings.Contains(err.Error(), "updater") || len(f.runner.commands) != 0 || len(f.rec.State().Pending) != 0 || f.rec.State().HasSystem("cask", "visual-studio-code") {
		t.Fatal("changed metadata reached brew, a mark or a record", err)
	}

	f = newCaskFixture(t, "tool.vscode")
	f.onRun = func([]string) error {
		f.publish()
		recipe := filepath.Join(f.spec.Caskroom(), ".metadata", "1", "timestamp", "Casks", "visual-studio-code.json")
		changed := caskTestMetadata(t, "visual-studio-code")
		changed["artifacts"] = append(changed["artifacts"].([]any), map[string]any{"pkg": []string{"Other.pkg"}})
		macTestWrite(t, recipe, string(caskTestJSON(t, changed)), 0644)
		return nil
	}
	if err := f.apply(f.r, want); err == nil || !strings.Contains(err.Error(), "preserve the native app") || !slices.Equal(f.rec.State().Pending, []string{"tool.vscode:brew-install"}) {
		t.Fatal("an unreviewed installed recipe passed", err)
	}
	if found := f.inspect(f.r, want); found.Status != engine.Blocked || !strings.Contains(found.Reason, "preserve the native app") {
		t.Fatal("a recorded cask needs a valid installed recipe", found)
	}

	f = newCaskFixture(t, "tool.ghostty")
	f.publish()
	f.record()
	if found := f.inspect(f.r, want); found.Status != engine.Satisfied {
		t.Fatal(found)
	}
	metadata := filepath.Join(f.spec.Caskroom(), ".metadata")
	macTestWrite(t, filepath.Join(metadata, "1", "timestamp", "Casks", "ghostty.json"), "{}", 0644)
	receipt := map[string]any{"uninstall_flight_blocks": false, "source": map[string]string{"tap": "homebrew/cask"},
		"uninstall_artifacts": f.metadata["ghostty"].(map[string]any)["artifacts"]}
	expanded := func() string {
		return string(caskTestJSON(t, receipt, "$APPDIR", f.d.appdir, "$HOMEBREW_PREFIX", f.location.Prefix))
	}
	macTestWrite(t, filepath.Join(metadata, "INSTALL_RECEIPT.json"), expanded(), 0644)
	if found := f.inspect(f.r, want); found.Status != engine.Satisfied {
		t.Fatal("the receipt form of the installed recipe was refused", found)
	}
	for name, change := range map[string]func(){
		"flight blocks": func() { receipt["uninstall_flight_blocks"] = true },
		"other tap":     func() { receipt["source"] = map[string]string{"tap": "other/tap"} },
		"other artifact": func() {
			receipt["uninstall_artifacts"] = append(receipt["uninstall_artifacts"].([]any), map[string]any{"pkgutil": "com.other"})
		},
	} {
		saved := caskTestCopy(receipt)
		change()
		macTestWrite(t, filepath.Join(metadata, "INSTALL_RECEIPT.json"), expanded(), 0644)
		if found := f.inspect(f.r, want); found.Status != engine.Blocked {
			t.Fatal("an unreviewed receipt passed:", name, found)
		}
		receipt = saved
	}
	macTestWrite(t, filepath.Join(metadata, "config.json"), `{"default":{"appdir":"/elsewhere"}}`, 0644)
	if found := f.inspect(f.r, want); found.Status != engine.Blocked || !strings.Contains(found.Reason, "install directory changed") {
		t.Fatal(found)
	}
}

func caskTestCopy(source map[string]any) map[string]any {
	result := map[string]any{}
	for key, value := range source {
		result[key] = value
	}
	return result
}
