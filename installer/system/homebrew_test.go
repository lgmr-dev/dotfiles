package system

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

func homebrewProcessFixture(t *testing.T) HomebrewLocation {
	t.Helper()
	root := filepath.Join(macTestRoot(t), "native packages with 'quotes'")
	location := HomebrewLocation{Program: filepath.Join(root, "bin", "brew"), Prefix: root, Cellar: filepath.Join(root, "Cellar")}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	macTestWrite(t, location.Program, "#!/bin/sh\nset -eu\n"+`case "$*" in
--prefix) printf '%s\n' `+quote(root)+`;;
--cellar) printf '%s\n' `+quote(location.Cellar)+`;;
--version) printf 'Homebrew fixture\n';;
environment) printf '%s|%s|%s|%s\n' "${HOMEBREW_NO_AUTO_UPDATE-unset}" "${HOMEBREW_NO_AUTOREMOVE-unset}" "${HOMEBREW_CASK_OPTS-unset}" "${HOMEBREW_NO_INSTALL_CLEANUP-unset}";;
'linkage --test git') test "${HOMEBREW_DEV_CMD_RUN-}" = 1; test "${HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK-}" = '';;
'outdated --json=v2 --formula git') printf '{"formulae":[{"name":"git","current_version":"2.0"}],"casks":[]}\n'; exit 1;;
*) printf 'unexpected fixture command\n' >&2; exit 2;;
esac
`, 0755)
	return location
}

func TestHomebrewDiscoveryAndInspectionUseActualProcessPaths(t *testing.T) {
	location := homebrewProcessFixture(t)
	got, err := inspectHomebrew(context.Background(), location.Program)
	if err != nil || *got != location || got.CommandDirectory() != filepath.Join(location.Prefix, "bin") {
		t.Fatal("provider paths were inferred or lost quoting", got, err)
	}
	t.Setenv("HOMEBREW_DEV_CMD_RUN", "parent-value")
	t.Setenv("HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK", "1")
	t.Setenv("HOMEBREW_CASK_OPTS", "--appdir=/elsewhere --force")
	if _, err := location.Query(context.Background(), "brew", "linkage", "--test", "git"); err != nil {
		t.Fatal("native inspection did not apply child-only maintenance controls", err)
	}
	if os.Getenv("HOMEBREW_DEV_CMD_RUN") != "parent-value" || os.Getenv("HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK") != "1" {
		t.Fatal("inspection changed its caller's environment")
	}
	output, err := location.Query(context.Background(), "brew", "environment")
	if err != nil || string(output) != "1|1||1\n" {
		t.Fatal("the inspection lacks a process control or inherited the cask options", string(output), err)
	}
	if _, err := location.Query(context.Background(), "brew", "unsupported"); err == nil || !strings.Contains(err.Error(), "unexpected fixture command") {
		t.Fatal("a failed inspection lost its diagnostic", err)
	} else if code, exited := platform.ExitCode(err); !exited || code != 2 {
		t.Fatal("a failed inspection lost its exit code", err)
	}
	current, outdated, err := brewOutdated(context.Background(), location.Query, "git")
	if err != nil || !outdated || current != "2.0" {
		t.Fatal("exit 1 with a list of outdated formulae is a result, not a failure", current, outdated, err)
	}
	if _, err := location.Query(context.Background(), "relative"); err == nil {
		t.Fatal("a relative program was accepted")
	}
}

func TestHomebrewDiscoveryUsesPathThenTheBootstrapLocation(t *testing.T) {
	location := homebrewProcessFixture(t)
	t.Setenv("PATH", t.TempDir())
	if got, err := discoverHomebrew(context.Background(), filepath.Join(t.TempDir(), "brew")); err != nil || got != nil {
		t.Fatal("an absent Homebrew was reported", got, err)
	}
	got, err := discoverHomebrew(context.Background(), location.Program)
	if err != nil || got == nil || *got != location {
		t.Fatal("the bootstrap location was not used before PATH has it", got, err)
	}
	t.Setenv("PATH", filepath.Dir(location.Program))
	got, err = discoverHomebrew(context.Background(), filepath.Join(t.TempDir(), "brew"))
	if err != nil || got == nil || *got != location {
		t.Fatal("Homebrew on PATH was not found", got, err)
	}
	for _, invalid := range []HomebrewLocation{{}, {Program: "brew", Prefix: "/p", Cellar: "/p/Cellar"}, {Program: "/p/bin/brew", Prefix: "/p/../q", Cellar: "/p/Cellar"}, {Program: "/p/bin/brew", Prefix: "/p:q", Cellar: "/p/Cellar"}} {
		if invalid.Validate() == nil {
			t.Fatal("invalid Homebrew location accepted", invalid)
		}
	}
}

func TestNixHomebrewIssueKeepsAManagedPrefixOutOfSetup(t *testing.T) {
	location := homebrewProcessFixture(t)
	if issue := NixHomebrewIssue(location); issue != "" {
		t.Fatal("an ordinary prefix was refused", issue)
	}
	marker := filepath.Join(location.Prefix, ".managed_by_nix_darwin")
	macTestWrite(t, marker, "", 0644)
	if issue := NixHomebrewIssue(location); !strings.Contains(issue, "managed by nix-homebrew") {
		t.Fatal(issue)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(location.Prefix, "Library"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nix/store/fixture-brew/Library/Homebrew", filepath.Join(location.Prefix, "Library", "Homebrew")); err != nil {
		t.Fatal(err)
	}
	if issue := NixHomebrewIssue(location); !strings.Contains(issue, "managed by nix-homebrew") {
		t.Fatal(issue)
	}
	d := NewBrew(location.Prefix)
	d.Discover = func(context.Context) (*HomebrewLocation, error) { return &location, nil }
	found, err := d.Inspect(context.Background(), catalog.Resource{ID: "tool.git", Bindings: map[string]catalog.Binding{"darwin": {Provider: "homebrew-formula", Package: "git"}}}, engine.Want{Present: true}, newMacTestRecords())
	if err != nil || found.Status != engine.Unknown || !strings.Contains(found.Reason, "nix-homebrew") {
		t.Fatal("a package used a prefix that nix-homebrew manages", found, err)
	}
}

type homebrewBootstrapFixture struct {
	t         *testing.T
	d         *HomebrewBootstrap
	r         catalog.Resource
	rec       *macTestRecords
	runner    *macTestRunner
	payload   string
	requests  int
	source    string
	inventory string
	fail      string
	version   string
	pending   [][]string
}

type homebrewRoundTripper func(*http.Request) (*http.Response, error)

func (f homebrewRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newHomebrewBootstrapFixture(t *testing.T) *homebrewBootstrapFixture {
	t.Helper()
	root := macTestRoot(t)
	f := &homebrewBootstrapFixture{t: t, d: NewHomebrewBootstrap(filepath.Join(root, "home")), r: catalog.Resource{ID: "infra.homebrew", Name: "Homebrew", Retain: true, ReplanAfter: true},
		rec: newMacTestRecords(), runner: &macTestRunner{}, payload: "#!/bin/bash\necho fixture installer\n", source: "https://github.com/Homebrew/brew",
		inventory: `{"formulae":[],"casks":[]}`, version: "Homebrew 4.fixture\n"}
	if f.d.prefix != "/opt/homebrew" || f.d.url != homebrewBootstrapURL || f.d.sha256 != homebrewBootstrapSHA256 || !homebrewBootstrapSource.MatchString(f.d.url) || !strings.Contains(f.d.url, homebrewBootstrapCommit) {
		t.Fatal("the provider lost its reviewed prefix or installer pin")
	}
	f.d.prefix = filepath.Join(root, "homebrew")
	sum := sha256.Sum256([]byte(f.payload))
	f.d.sha256 = hex.EncodeToString(sum[:])
	f.d.HTTPClient = &http.Client{Transport: homebrewRoundTripper(func(r *http.Request) (*http.Response, error) {
		f.requests++
		if r.URL.String() != homebrewBootstrapURL {
			t.Fatal("unexpected download", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(f.payload)), Header: http.Header{}}, nil
	})}
	location := HomebrewLocation{Program: filepath.Join(f.d.prefix, "bin", "brew"), Prefix: f.d.prefix, Cellar: filepath.Join(f.d.prefix, "Cellar")}
	f.d.Discover = func(context.Context) (*HomebrewLocation, error) {
		if _, err := os.Lstat(location.Program); err != nil {
			return nil, nil
		}
		return &location, nil
	}
	f.d.Query = func(_ context.Context, program string, args ...string) ([]byte, error) {
		if program == "/usr/bin/env" && slices.Contains(args, "remote.origin.url") {
			return []byte(f.source + "\n"), nil
		}
		if program != "brew" {
			t.Fatal("unexpected query", program, args)
		}
		switch strings.Join(args, " ") {
		case "--version":
			return []byte(f.version), nil
		case "--prefix":
			return []byte(location.Prefix + "\n"), nil
		case "--cellar":
			return []byte(location.Cellar + "\n"), nil
		case "info --json=v2 --installed":
			return []byte(f.inventory), nil
		}
		t.Fatal("unexpected brew query", args)
		return nil, nil
	}
	f.d.Runner = f.runner
	f.runner.run = func(c platform.Command, input string) ([]byte, error) {
		kind := "install"
		if c.Path == "/usr/bin/sudo" {
			kind = "reserve"
		}
		f.pending = append(f.pending, f.rec.State().Pending)
		if f.fail == kind+"-not-started" {
			return nil, errors.New("fork failed")
		}
		if f.fail == kind {
			return nil, macTestExit(1)
		}
		if kind == "reserve" {
			if err := os.Mkdir(f.d.prefix, 0755); err != nil || f.fail != "reserve-then-content" {
				return nil, err
			}
			macTestWrite(t, filepath.Join(f.d.prefix, "other"), "content of another process", 0644)
			return nil, nil
		}
		macTestWrite(t, location.Program, "fixture manager; never executed", 0755)
		return nil, nil
	}
	return f
}

func (f *homebrewBootstrapFixture) inspect() engine.Finding {
	f.t.Helper()
	found, err := f.d.Inspect(context.Background(), f.r, engine.Want{Present: true}, f.rec)
	if err != nil {
		f.t.Fatal(err)
	}
	return found
}

func (f *homebrewBootstrapFixture) apply() error {
	return f.d.Apply(context.Background(), f.r, f.inspect(), f.rec)
}

func TestHomebrewBootstrapFreshLifecycleUsesThePendingMarkAndReplans(t *testing.T) {
	f := newHomebrewBootstrapFixture(t)
	missing := f.inspect()
	if missing.Status != engine.Missing || !missing.Replan || !missing.Pending || !missing.Privileged || len(missing.Commands) != 1 || missing.Commands[0].By != "setup" {
		t.Fatal(missing)
	}
	if err := f.d.Apply(context.Background(), f.r, missing, f.rec); err != nil {
		t.Fatal(err)
	}
	if len(f.runner.commands) != 2 || f.requests != 1 {
		t.Fatal("unexpected bootstrap commands", f.runner.commands)
	}
	reserve, install := f.runner.commands[0], f.runner.commands[1]
	if reserve.Path != "/usr/bin/sudo" || !reserve.Elevated || !slices.Equal(reserve.Args, []string{"-n", "/bin/mkdir", f.d.prefix}) {
		t.Fatal("unsafe prefix reservation", reserve)
	}
	if install.Path != "/usr/bin/env" || len(install.Env) != 0 || f.runner.inputs[1] != f.payload ||
		!slices.Equal(install.Args[len(install.Args)-6:], []string{"/bin/bash", "--noprofile", "--norc", "-s", "--", "--path=" + f.d.prefix}) {
		t.Fatal("unsafe native bootstrap command", install.Path, install.Args)
	}
	for _, required := range append([]string{"-i", "HOME=" + f.d.Home, "NONINTERACTIVE=1", "SUDO_ASKPASS=/usr/bin/false", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "HOMEBREW_BREW_GIT_REMOTE=https://github.com/Homebrew/brew"}, brewProcessControls...) {
		if !slices.Contains(install.Args, required) {
			t.Fatal("missing bootstrap environment boundary", required)
		}
	}
	if !slices.Equal(f.pending[0], []string{"infra.homebrew:install"}) || !slices.Equal(f.pending[1], []string{"infra.homebrew:install"}) || len(f.rec.State().Pending) != 0 {
		t.Fatal("the pending mark must cover the prefix reservation and the installer", f.pending, f.rec.State().Pending)
	}
	if shown := missing.Commands[0].Lines; !slices.Equal(shown, []string{macTestLine(reserve), macTestLine(install) + " < install.sh"}) {
		t.Fatal("the approved lines differ from the commands that ran", shown)
	}
	if len(f.rec.State().System) != 0 {
		t.Fatal("retained Homebrew got a system record", f.rec.State().System)
	}
	after := f.inspect()
	if after.Status != engine.Satisfied || after.Version != "4.fixture" || after.Pending || len(after.Commands) != 0 {
		t.Fatal(after)
	}
	if err := f.d.Apply(context.Background(), f.r, after, f.rec); err == nil || len(f.runner.commands) != 2 {
		t.Fatal("an existing Homebrew was changed", err)
	}
	if err := f.d.Remove(context.Background(), f.r, after, f.rec); err == nil {
		t.Fatal("Homebrew was removed")
	}
}

func TestHomebrewBootstrapVerifiesThePayloadBeforeAnyCommand(t *testing.T) {
	refused := func(name string, f *homebrewBootstrapFixture) {
		t.Helper()
		if err := f.apply(); err == nil {
			t.Fatal("an unverified installer was accepted:", name)
		}
		if len(f.runner.commands) != 0 || len(f.rec.State().Pending) != 0 {
			t.Fatal("a bad payload reached a command or a pending mark:", name)
		}
	}
	f := newHomebrewBootstrapFixture(t)
	f.d.sha256 = strings.Repeat("0", 64)
	refused("wrong checksum", f)
	f = newHomebrewBootstrapFixture(t)
	f.d.url = "https://example.com/install.sh"
	refused("unpinned source", f)
	if f.requests != 0 {
		t.Fatal("an unpinned source was downloaded")
	}
	f = newHomebrewBootstrapFixture(t)
	f.payload = strings.Repeat("x", homebrewBootstrapLimit+1)
	sum := sha256.Sum256([]byte(f.payload))
	f.d.sha256 = hex.EncodeToString(sum[:])
	refused("oversized installer", f)
	f = newHomebrewBootstrapFixture(t)
	f.d.HTTPClient = &http.Client{Transport: homebrewRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "raw.githubusercontent.com" {
			t.Fatal("the download followed a redirect", r.URL)
		}
		return &http.Response{StatusCode: http.StatusFound, Body: io.NopCloser(bytes.NewReader(nil)), Header: http.Header{"Location": {"https://example.com/install.sh"}}, Request: r}, nil
	})}
	refused("redirect", f)
}

func TestHomebrewBootstrapMarkLifecycleOnFailure(t *testing.T) {
	f := newHomebrewBootstrapFixture(t)
	f.fail = "reserve"
	if err := f.apply(); err == nil || len(f.runner.commands) != 1 || !slices.Equal(f.pending[0], []string{"infra.homebrew:install"}) || !slices.Equal(f.rec.State().Pending, []string{"infra.homebrew:install"}) {
		t.Fatal("a failed reservation reached the installer or ran without its mark", err, f.pending, f.rec.State().Pending)
	}
	f = newHomebrewBootstrapFixture(t)
	f.fail = "reserve-not-started"
	if err := f.apply(); err == nil || len(f.runner.commands) != 1 || !slices.Equal(f.pending[0], []string{"infra.homebrew:install"}) || len(f.rec.State().Pending) != 0 {
		t.Fatal("a reservation that did not start kept its mark", err, f.pending, f.rec.State().Pending)
	}
	f = newHomebrewBootstrapFixture(t)
	f.fail = "install-not-started"
	if err := f.apply(); err == nil || !strings.Contains(err.Error(), "did not start") || len(f.rec.State().Pending) != 0 {
		t.Fatal("an installer that did not start kept its mark", err, f.rec.State().Pending)
	}
	if found := f.inspect(); found.Status != engine.Missing || len(found.Commands[0].Lines) != 1 || strings.Contains(found.Commands[0].Lines[0], "mkdir") {
		t.Fatal("an empty reserved prefix must permit the installer without a second reservation", found)
	}
	f.fail = ""
	if err := f.apply(); err != nil || len(f.runner.commands) != 3 || f.runner.commands[2].Path != "/usr/bin/env" {
		t.Fatal("the retry into the empty prefix failed", err)
	}
	f = newHomebrewBootstrapFixture(t)
	f.fail = "install"
	if err := f.apply(); err == nil || !strings.Contains(err.Error(), "confirm that the command exited") || !slices.Equal(f.rec.State().Pending, []string{"infra.homebrew:install"}) {
		t.Fatal("a failed installer lost its pending mark", err, f.rec.State().Pending)
	}
}

func TestHomebrewBootstrapNamesTheFolderWhenTheReservedPrefixChanged(t *testing.T) {
	f := newHomebrewBootstrapFixture(t)
	f.fail = "reserve-then-content"
	err := f.apply()
	if err == nil || len(f.runner.commands) != 1 || len(f.rec.State().Pending) != 0 {
		t.Fatal("the installer ran in a prefix with content, or the mark stayed without a command", err, f.rec.State().Pending)
	}
	for _, part := range []string{f.d.prefix, "Examine", "remove the empty folder that setup reserved", "run setup again"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("the error lacks %q: %v", part, err)
		}
	}
	found := f.inspect()
	if found.Status != engine.Unknown || !strings.Contains(found.Reason, f.d.prefix) || !strings.Contains(found.Reason, "run setup again") {
		t.Fatal("the next inspection gives no hint", found)
	}
	if err := os.Remove(filepath.Join(f.d.prefix, "other")); err != nil {
		t.Fatal(err)
	}
	f.fail = ""
	if found := f.inspect(); found.Status != engine.Missing || len(found.Commands[0].Lines) != 1 {
		t.Fatal("an empty reserved prefix must permit a retry without a second reservation", found)
	}
	if err := f.apply(); err != nil || len(f.runner.commands) != 2 || f.runner.commands[1].Path != "/usr/bin/env" || len(f.rec.State().Pending) != 0 {
		t.Fatal("the retry into the empty reserved prefix failed", err)
	}
	if found := f.inspect(); found.Status != engine.Satisfied {
		t.Fatal(found)
	}
}

func TestHomebrewBootstrapKeepsUnknownBrokenAndChangedInstallations(t *testing.T) {
	f := newHomebrewBootstrapFixture(t)
	macTestWrite(t, filepath.Join(f.d.prefix, "personal"), "data", 0644)
	found := f.inspect()
	if found.Status != engine.Unknown || !strings.Contains(found.Reason, "without a working manager") || found.Replan {
		t.Fatal(found)
	}
	if err := f.d.Apply(context.Background(), f.r, engine.Finding{Status: engine.Missing}, f.rec); err == nil || len(f.runner.commands) != 0 {
		t.Fatal("an occupied prefix was overwritten", err)
	}
	f = newHomebrewBootstrapFixture(t)
	macTestWrite(t, filepath.Join(f.d.prefix, "bin", "brew"), "manager", 0755)
	f.version = "not a manager\n"
	if found := f.inspect(); found.Status != engine.Blocked || !strings.Contains(found.Reason, "needs native repair") {
		t.Fatal(found)
	}
	for _, change := range []string{"packages", "source"} {
		f = newHomebrewBootstrapFixture(t)
		if change == "packages" {
			f.inventory = `{"formulae":[{"name":"git","full_name":"git","linked_keg":"1.0","installed":[{"version":"1.0","installed_on_request":true,"runtime_dependencies":[]}]}],"casks":[]}`
		} else {
			f.source = "https://example.com/brew"
		}
		if err := f.apply(); err == nil || !slices.Equal(f.rec.State().Pending, []string{"infra.homebrew:install"}) {
			t.Fatal("an unverified bootstrap cleared its mark", change, err)
		}
	}
	f = newHomebrewBootstrapFixture(t)
	f.d.Discover = func(context.Context) (*HomebrewLocation, error) { return nil, errors.New("brew --prefix failed") }
	if found := f.inspect(); found.Status != engine.Unknown || found.Reason != "brew --prefix failed" {
		t.Fatal("a discovery failure must stay local to Homebrew", found)
	}
	if _, err := f.d.Inspect(context.Background(), catalog.Resource{ID: "tool.git"}, engine.Want{Present: true}, f.rec); err == nil {
		t.Fatal("another resource was accepted")
	}
}
