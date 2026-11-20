package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

type aptFixtureRecords struct{ st state.State }

func newAPTFixtureRecords() *aptFixtureRecords {
	return &aptFixtureRecords{state.State{Schema: 2, Target: "linux/amd64:/home/fixture", System: map[string][]string{}, Targets: map[string]state.Record{}}}
}

func (r *aptFixtureRecords) State() state.State { return r.st.Clone() }

func (r *aptFixtureRecords) Update(edit func(*state.State) error) error {
	next := r.st.Clone()
	if err := edit(&next); err != nil {
		return err
	}
	r.st = next
	return nil
}

func aptFixtureExit(code int) error {
	return &platform.CommandError{Message: fmt.Sprintf("native command failed: exit status %d", code), ExitCode: &code}
}

type aptFixture struct {
	t              *testing.T
	apt            *APT
	rec            *aptFixtureRecords
	installed      map[string]aptPackage
	candidates     map[string]string
	verify         map[string]string
	collisions     string
	commands       []platform.Command
	authentication int
	fail           func(platform.Command) error
	during         func(platform.Command)
	health         func(args []string) ([]byte, error)
}

func newAPTFixture(t *testing.T) *aptFixture {
	t.Helper()
	if os.PathSeparator != '/' {
		t.Skip("the APT provider uses POSIX paths")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &aptFixture{t: t, rec: newAPTFixtureRecords(), installed: map[string]aptPackage{}, candidates: map[string]string{}, verify: map[string]string{}}
	location := APTLocation{APTGet: "/fake/apt-get", APTCache: "/fake/apt-cache", Dpkg: "/fake/dpkg", DpkgQuery: "/fake/dpkg-query", APTMark: "/fake/apt-mark", Sudo: "/fake/sudo"}
	f.apt = NewAPT(location, catalog.Pins{}, "amd64", filepath.Join(root, "apt"))
	f.apt.Temporary = filepath.Join(root)
	f.apt.DpkgConfiguration = filepath.Join(root, "dpkg")
	f.apt.Query, f.apt.Runner = f.query, f
	f.apt.Authenticate = func(context.Context) error { f.authentication++; return nil }
	return f
}

func (f *aptFixture) query(ctx context.Context, program string, args ...string) ([]byte, error) {
	switch {
	case program == "/fake/dpkg-query":
		rows := ""
		for _, name := range slices.Sorted(func(yield func(string) bool) {
			for name := range f.installed {
				if !yield(name) {
					return
				}
			}
		}) {
			pkg := f.installed[name]
			want, status := "install", "installed"
			if pkg.Held {
				want = "hold"
			}
			if !pkg.Healthy {
				status = "half-configured"
			}
			rows += name + "\t" + pkg.Version + "\t" + want + "\t" + status + "\tok\n"
		}
		return []byte(rows), nil
	case program == "/fake/apt-mark":
		names := ""
		for name, pkg := range f.installed {
			if pkg.Automatic {
				base, _, _ := strings.Cut(name, ":")
				names += base + "\n"
			}
		}
		return []byte(names), nil
	case program == "/fake/apt-cache":
		pkg := f.installed[args[1]]
		candidate := pkg.Version
		if newer := f.candidates[args[1]]; newer != "" {
			candidate = newer
		}
		return []byte(args[1] + ":\n  Installed: " + pkg.Version + "\n  Candidate: " + candidate + "\n  Version table:\n"), nil
	case program == "/fake/dpkg" && args[0] == "--verify":
		return []byte(f.verify[args[1]]), nil
	case program == "/fake/dpkg" && args[0] == "--compare-versions":
		if args[1] > args[3] {
			return nil, nil
		}
		return nil, aptFixtureExit(1)
	case program == "/bin/sh" && args[1] == debCodeHealth:
		if f.health != nil {
			return f.health(args)
		}
		return nil, nil
	case program == "/bin/sh":
		return []byte(f.collisions), nil
	}
	return nil, nil
}

func (f *aptFixture) Run(ctx context.Context, c platform.Command) ([]byte, error) {
	f.commands = append(f.commands, c)
	if f.fail != nil {
		if err := f.fail(c); err != nil {
			return nil, err
		}
	}
	if f.during != nil {
		f.during(c)
	}
	index := slices.Index(c.Args, "install")
	if index < 0 {
		return nil, nil
	}
	input := c.Args[index+1]
	name, version := input, "2"
	if strings.HasSuffix(input, ".deb") {
		name = strings.TrimSuffix(filepath.Base(input), ".deb")
		if name == "code" {
			version = f.apt.Pins.LinuxCode["amd64"].Version
		} else {
			version = f.apt.Pins.LinuxPackages["tool."+name]["amd64"].Version
		}
	}
	identity := name + ":amd64"
	if newer := f.candidates[identity]; newer != "" {
		version = newer
		delete(f.candidates, identity)
	}
	f.installed[identity] = aptPackage{Name: identity, Version: version, Healthy: true, Automatic: f.installed[identity].Automatic}
	return nil, nil
}

func (f *aptFixture) installArguments() [][]string {
	result := [][]string{}
	for _, c := range f.commands {
		if slices.Contains(c.Args, "install") {
			result = append(result, c.Args)
		}
	}
	return result
}

func aptFixtureResource(name string) catalog.Resource {
	return catalog.Resource{ID: "tool." + name, Name: "Fixture " + name, Action: "apt", Bindings: map[string]catalog.Binding{"linux": {Provider: "apt", Package: name}}}
}

func aptFixtureInstalled(f *aptFixture, name, version string) {
	f.installed[name+":amd64"] = aptPackage{Name: name + ":amd64", Version: version, Healthy: true}
}

func TestAPTInstallArgumentsKeepTheGuardsAndNeverRemoveOrDowngrade(t *testing.T) {
	for _, c := range []struct {
		action    string
		automatic bool
		want      []string
		absent    []string
	}{
		{"install", false, []string{"--no-upgrade"}, []string{"--reinstall", "--only-upgrade", "--mark-auto"}},
		{"update", false, []string{"--only-upgrade"}, []string{"--reinstall", "--no-upgrade", "--mark-auto"}},
		{"update", true, []string{"--only-upgrade", "--mark-auto"}, []string{"--reinstall", "--no-upgrade"}},
		{"repair", false, []string{"--reinstall"}, []string{"--no-upgrade", "--mark-auto", "--only-upgrade"}},
		{"repair", true, []string{"--reinstall", "--mark-auto", "--only-upgrade"}, []string{"--no-upgrade"}},
	} {
		args := aptInstallArguments(c.action, c.automatic, "zsh")
		for _, word := range append([]string{"--yes", "--no-remove", "--no-install-recommends", "Dpkg::Options::=--force-confold", "DPkg::Lock::Timeout=30"}, c.want...) {
			if !slices.Contains(args, word) {
				t.Fatal(c.action, "lacks", word, args)
			}
		}
		for _, word := range append([]string{"--allow-downgrades", "--purge", "purge", "autoremove", "--autoremove", "--auto-remove", "remove", "--allow-remove-essential", "--force-yes"}, c.absent...) {
			if slices.Contains(args, word) {
				t.Fatal(c.action, "has", word, args)
			}
		}
		if !slices.Equal(args[len(args)-2:], []string{"install", "zsh"}) {
			t.Fatal("the package input must end the command", args)
		}
	}
	if got := aptRemovalArguments("zsh"); !slices.Equal(got, []string{"remove", "zsh"}) {
		t.Fatal("the removal command for the operator must be a plain remove", got)
	}
}

func TestAPTInventoryParsing(t *testing.T) {
	rows := "zsh:amd64\t5.9-6\tinstall\tinstalled\tok\n" +
		"held:amd64\t1\thold\tinstalled\tok\n" +
		"broken:amd64\t1\tinstall\thalf-configured\tok\n" +
		"reinst:amd64\t1\tinstall\tinstalled\treinstreq\n" +
		"residue:amd64\t1\tdeinstall\tconfig-files\tok\n" +
		"gone:amd64\t\tunknown\tnot-installed\tok\n" +
		"libatomic1:i386\t14\tinstall\tinstalled\tok\n" +
		"docs:all\t3\tinstall\tinstalled\tok\n"
	installed, err := parseAPTInventory([]byte(rows), []byte("held\nlibatomic1:i386\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]aptPackage{
		"zsh:amd64":       {Name: "zsh:amd64", Version: "5.9-6", Healthy: true},
		"held:amd64":      {Name: "held:amd64", Version: "1", Healthy: true, Held: true, Automatic: true},
		"broken:amd64":    {Name: "broken:amd64", Version: "1"},
		"reinst:amd64":    {Name: "reinst:amd64", Version: "1"},
		"libatomic1:i386": {Name: "libatomic1:i386", Version: "14", Healthy: true, Automatic: true},
		"docs:all":        {Name: "docs:all", Version: "3", Healthy: true},
	}
	if len(installed) != len(want) {
		t.Fatal("residual configuration or absent rows count as installed", installed)
	}
	for name, pkg := range want {
		if installed[name] != pkg {
			t.Fatal(name, installed[name], pkg)
		}
	}
	for name, identity := range map[string]string{"zsh": "zsh:amd64", "docs": "docs:all", "libatomic1": "", "libatomic1:i386": "libatomic1:i386", "residue": ""} {
		if got := aptIdentity(name, "amd64", installed); got != identity {
			t.Fatal(name, got, identity)
		}
	}
	for _, invalid := range []string{
		"zsh\t1\tinstall\tinstalled\tok\n",
		"zsh:amd64\t1\tinstall\tinstalled\n",
		"zsh:amd64\t\tinstall\tinstalled\tok\n",
		"zsh:amd64\t1\tinstall\tinstalled\tok\nzsh:amd64\t1\tinstall\tinstalled\tok\n",
		"Zsh:amd64\t1\tinstall\tinstalled\tok\n",
		"zsh:amd64\t1\tinstall\tinstalled\tok\x00\n",
	} {
		if _, err := parseAPTInventory([]byte(invalid), nil); err == nil {
			t.Fatalf("accepted the invalid inventory %q", invalid)
		}
	}
	if _, err := parseAPTInventory(nil, []byte("zsh\nzsh\n")); err == nil {
		t.Fatal("accepted a duplicate automatic package")
	}
}

func TestAPTCandidateParsing(t *testing.T) {
	for output, want := range map[string]string{
		"zsh:\n  Installed: 5.9-6\n  Candidate: 5.9-7\n  Version table:\n": "5.9-7",
		"zsh:\n  Installed: (none)\n  Candidate: (none)\n":                 "",
	} {
		if got, err := parseAPTCandidate([]byte(output)); err != nil || got != want {
			t.Fatal(output, got, err)
		}
	}
	for _, invalid := range []string{"", "zsh:\n  Installed: 1\n", "zsh:\n  Candidate: 1 2\n"} {
		if _, err := parseAPTCandidate([]byte(invalid)); err == nil {
			t.Fatalf("accepted the invalid policy %q", invalid)
		}
	}
}

func TestAPTPendingMarkLifecycle(t *testing.T) {
	ctx := context.Background()
	r := aptFixtureResource("zsh")
	isInstall := func(c platform.Command) bool { return slices.Contains(c.Args, "install") }

	t.Run("did not start", func(t *testing.T) {
		f := newAPTFixture(t)
		f.fail = func(c platform.Command) error {
			if isInstall(c) {
				return errors.New("fork/exec /fake/sudo: no such file or directory")
			}
			return nil
		}
		found, err := f.apt.Inspect(ctx, r, engine.Want{Present: true}, f.rec)
		if err != nil || found.Status != engine.Missing || !found.Pending || !found.Privileged {
			t.Fatal(found, err)
		}
		if err := f.apt.Apply(ctx, r, found, f.rec); err == nil || !strings.Contains(err.Error(), "did not start") {
			t.Fatal(err)
		}
		if st := f.rec.State(); len(st.Pending) != 0 || st.HasSystem("apt", "zsh") {
			t.Fatal("a command that did not start left its mark or its provisional record", st.Pending, st.System)
		}
	})

	t.Run("started", func(t *testing.T) {
		f := newAPTFixture(t)
		seen := false
		f.fail = func(c platform.Command) error {
			if !isInstall(c) {
				if st := f.rec.State(); len(st.Pending) != 0 || st.HasSystem("apt", "zsh") {
					t.Fatal("the refresh of the package lists must not need a mark", st.Pending, st.System)
				}
				return nil
			}
			st := f.rec.State()
			seen = slices.Equal(st.Pending, []string{"tool.zsh:apt-install"}) && st.HasSystem("apt", "zsh")
			return aptFixtureExit(100)
		}
		found, _ := f.apt.Inspect(ctx, r, engine.Want{Present: true}, f.rec)
		if err := f.apt.Apply(ctx, r, found, f.rec); err == nil || !strings.Contains(err.Error(), "confirm that the command exited") {
			t.Fatal(err)
		}
		if !seen {
			t.Fatal("the mark and the provisional record must exist before apt-get starts")
		}
		if st := f.rec.State(); !slices.Equal(st.Pending, []string{"tool.zsh:apt-install"}) || !st.HasSystem("apt", "zsh") {
			t.Fatal("a command that started and failed must keep its mark and its record", st.Pending, st.System)
		}
		if err := f.rec.Update(func(s *state.State) error { s.Clear("tool.zsh:apt-install"); return nil }); err != nil {
			t.Fatal(err)
		}
		if found, err := f.apt.Inspect(ctx, r, engine.Want{}, f.rec); err != nil || found.Status != engine.Absent {
			t.Fatal(found, err)
		}
		if err := f.apt.Remove(ctx, r, engine.Finding{}, f.rec); err != nil || f.rec.State().HasSystem("apt", "zsh") {
			t.Fatal("the record of an absent package must go after verified absence", err)
		}
	})

	t.Run("verified", func(t *testing.T) {
		f := newAPTFixture(t)
		f.during = func(c platform.Command) {
			if f.authentication != 1 {
				t.Fatal("the foreground authentication must come one time before the first privileged command", f.authentication)
			}
			if c.Path != "/fake/sudo" || !slices.Equal(c.Args[:4], []string{"-n", "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive", "/fake/apt-get"}) || len(c.Env) != 0 {
				t.Fatal("APT must run through the non-interactive sudo line", c)
			}
		}
		found, _ := f.apt.Inspect(ctx, r, engine.Want{Present: true}, f.rec)
		if len(found.Commands) != 1 || found.Commands[0].By != "setup" || !slices.Equal(found.Commands[0].Lines, []string{
			"sudo apt-get update",
			"sudo apt-get --yes --no-remove --no-install-recommends -o Dpkg::Use-Pty=0 -o DPkg::Lock::Timeout=30 -o Dpkg::Options::=--force-confold --no-upgrade install zsh",
		}) || !strings.Contains(strings.Join(found.Notes, " "), "depends on") {
			t.Fatal("the preview must show the exact commands and explain the dependencies", found.Commands, found.Notes)
		}
		if err := f.apt.Apply(ctx, r, found, f.rec); err != nil {
			t.Fatal(err)
		}
		if len(f.commands) != 2 || !slices.Equal(f.commands[0].Args[4:], []string{"update"}) || !slices.Equal(f.commands[1].Args[4:], aptInstallArguments("install", false, "zsh")) {
			t.Fatal(f.commands)
		}
		if st := f.rec.State(); len(st.Pending) != 0 || !st.HasSystem("apt", "zsh") {
			t.Fatal("a verified install must clear the mark and keep the record", st.Pending, st.System)
		}
		if found, err := f.apt.Inspect(ctx, r, engine.Want{Present: true}, f.rec); err != nil || found.Status != engine.Satisfied || found.Version != "2" || len(found.Commands) != 0 {
			t.Fatal(found, err)
		}
	})

	t.Run("root", func(t *testing.T) {
		f := newAPTFixture(t)
		f.apt.Location.Root, f.apt.Location.Sudo = true, ""
		found, _ := f.apt.Inspect(ctx, r, engine.Want{Present: true}, f.rec)
		if err := f.apt.Apply(ctx, r, found, f.rec); err != nil {
			t.Fatal(err)
		}
		for _, c := range f.commands {
			if c.Path != "/fake/apt-get" || !slices.Equal(c.Env, []string{"LC_ALL=C", "DEBIAN_FRONTEND=noninteractive"}) || f.authentication != 0 {
				t.Fatal("root must run APT without sudo and without a password request", c)
			}
		}
	})
}

func TestAPTFailedRefreshLeavesNoMark(t *testing.T) {
	f := newAPTFixture(t)
	ctx := context.Background()
	r := aptFixtureResource("zsh")
	f.fail = func(platform.Command) error { return aptFixtureExit(100) }
	found, _ := f.apt.Inspect(ctx, r, engine.Want{Present: true}, f.rec)
	err := f.apt.Apply(ctx, r, found, f.rec)
	if err == nil || !strings.Contains(err.Error(), "could not refresh its package lists") || len(f.commands) != 1 {
		t.Fatal(err, f.commands)
	}
	if st := f.rec.State(); len(st.Pending) != 0 || st.HasSystem("apt", "zsh") {
		t.Fatal(st.Pending, st.System)
	}
}

func TestAPTChangesAPresentPackageOnlyWithARecord(t *testing.T) {
	ctx := context.Background()
	r := aptFixtureResource("zsh")
	want := engine.Want{Present: true, Update: true}

	f := newAPTFixture(t)
	aptFixtureInstalled(f, "zsh", "1")
	f.candidates["zsh:amd64"] = "3"
	found, err := f.apt.Inspect(ctx, r, want, f.rec)
	if err != nil || found.Status != engine.Satisfied || len(found.Commands) != 0 || found.Pending {
		t.Fatal("a healthy package without a record must be reused as it is", found, err)
	}
	if err := f.apt.Apply(ctx, r, found, f.rec); err != nil || len(f.commands) != 0 || f.rec.State().HasSystem("apt", "zsh") {
		t.Fatal("setup changed or recorded a package that it did not install", err, f.commands)
	}
	pkg := f.installed["zsh:amd64"]
	pkg.Healthy = false
	f.installed["zsh:amd64"] = pkg
	found, err = f.apt.Inspect(ctx, r, want, f.rec)
	if err != nil || found.Status != engine.Blocked || len(found.Commands) != 1 || found.Commands[0].By != "you" || !slices.Equal(found.Commands[0].Lines, []string{"sudo apt-get install --reinstall zsh"}) {
		t.Fatal("an unhealthy package without a record needs the operator", found, err)
	}
	if err := f.apt.Apply(ctx, r, found, f.rec); err == nil || len(f.commands) != 0 {
		t.Fatal("setup repaired a package that it did not install", err, f.commands)
	}

	f = newAPTFixture(t)
	aptFixtureInstalled(f, "zsh", "1")
	f.rec.st.AddSystem("apt", "zsh")
	found, err = f.apt.Inspect(ctx, r, want, f.rec)
	if err != nil || found.Status != engine.Satisfied {
		t.Fatal(found, err)
	}
	f.candidates["zsh:amd64"] = "3"
	found, err = f.apt.Inspect(ctx, r, want, f.rec)
	if err != nil || found.Status != engine.Outdated || !strings.Contains(found.Reason, "version 3") {
		t.Fatal("a recorded package with a newer candidate must be outdated", found, err)
	}
	if err := f.apt.Apply(ctx, r, found, f.rec); err != nil {
		t.Fatal(err)
	}
	if got := f.installArguments(); len(got) != 1 || !slices.Contains(got[0], "--only-upgrade") || slices.Contains(got[0], "--allow-downgrades") || !slices.Contains(got[0], "--no-remove") {
		t.Fatal(got)
	}
	if found, err := f.apt.Inspect(ctx, r, want, f.rec); err != nil || found.Status != engine.Satisfied || found.Version != "3" || len(f.rec.State().Pending) != 0 {
		t.Fatal(found, err)
	}

	pkg = f.installed["zsh:amd64"]
	pkg.Healthy, pkg.Automatic = false, true
	f.installed["zsh:amd64"] = pkg
	f.commands = nil
	found, err = f.apt.Inspect(ctx, r, want, f.rec)
	if err != nil || found.Status != engine.Unhealthy {
		t.Fatal(found, err)
	}
	if err := f.apt.Apply(ctx, r, found, f.rec); err != nil {
		t.Fatal(err)
	}
	if got := f.installArguments(); len(got) != 1 || !slices.Contains(got[0], "--reinstall") || !slices.Contains(got[0], "--mark-auto") {
		t.Fatal("a repair of a recorded automatic package must keep its automatic mark", got)
	}

	pkg = f.installed["zsh:amd64"]
	pkg.Held = true
	f.installed["zsh:amd64"] = pkg
	f.candidates["zsh:amd64"] = "9"
	if found, err := f.apt.Inspect(ctx, r, want, f.rec); err != nil || found.Status != engine.Satisfied || found.Pending {
		t.Fatal("setup must keep a hold", found, err)
	}
}

func TestAPTRemovalCommandOnlyForARecordedPackage(t *testing.T) {
	ctx := context.Background()
	r := aptFixtureResource("zsh")
	f := newAPTFixture(t)
	aptFixtureInstalled(f, "zsh", "1")
	found, err := f.apt.Inspect(ctx, r, engine.Want{}, f.rec)
	if err != nil || found.Status != engine.Satisfied || len(found.Commands) != 0 {
		t.Fatal("a package without a record gets no removal command", found, err)
	}
	f.rec.st.AddSystem("apt", "zsh")
	found, err = f.apt.Inspect(ctx, r, engine.Want{}, f.rec)
	if err != nil || found.Status != engine.Satisfied || len(found.Commands) != 1 || found.Commands[0].By != "you" ||
		!slices.Equal(found.Commands[0].Lines, []string{"sudo apt-get remove zsh"}) || !strings.Contains(strings.Join(found.Notes, " "), "Examine the packages that APT proposes to remove") {
		t.Fatal(found, err)
	}
	for _, word := range []string{"purge", "autoremove", "auto-remove", "--yes", "-y "} {
		if strings.Contains(found.Commands[0].Lines[0]+" ", word) {
			t.Fatal("the removal command must not purge, autoremove or confirm for the operator", found.Commands[0].Lines)
		}
	}
	if err := f.apt.Remove(ctx, r, found, f.rec); err == nil || len(f.commands) != 0 || !f.rec.State().HasSystem("apt", "zsh") {
		t.Fatal("setup must never remove a package, and the record of a present package must stay", err, f.commands)
	}
	delete(f.installed, "zsh:amd64")
	f.installed["zsh:i386"] = aptPackage{Name: "zsh:i386", Version: "1", Healthy: true}
	found, err = f.apt.Inspect(ctx, r, engine.Want{}, f.rec)
	if err != nil || found.Status != engine.Absent || len(found.Commands) != 0 {
		t.Fatal(found, err)
	}
	if err := f.apt.Remove(ctx, r, found, f.rec); err != nil || len(f.commands) != 0 || f.rec.State().HasSystem("apt", "zsh") {
		t.Fatal("Remove must delete the record after verified absence, and run no command", err, f.commands)
	}
}

func TestAPTLibraryHealthRejectsChangedFilesDespiteSuccessfulDpkgExit(t *testing.T) {
	ctx := context.Background()
	r := catalog.Resource{ID: "library.first", Name: "Fixture library", Action: "apt-library", Bindings: map[string]catalog.Binding{"linux": {Provider: "apt", Package: "first"}}}
	for _, diagnostic := range []string{"", "??5?????? /usr/lib/libexample.so\n", "missing /usr/lib/libexample.so\n"} {
		f := newAPTFixture(t)
		aptFixtureInstalled(f, "first", "1")
		f.rec.st.AddSystem("apt", "first")
		f.verify["first:amd64"] = diagnostic
		found, err := f.apt.Inspect(ctx, r, engine.Want{Present: true}, f.rec)
		if err != nil || (found.Status == engine.Satisfied) != (diagnostic == "") || diagnostic != "" && (found.Status != engine.Unhealthy || !strings.Contains(found.Reason, "changed package files")) {
			t.Fatal("package database registration hid changed runtime files", found, err)
		}
	}
}

func TestAPTUnqualifiedNameResolvesNativeArchitectureAmongMultiArchInstances(t *testing.T) {
	ctx := context.Background()
	r := catalog.Resource{ID: "library.libatomic1", Name: "libatomic1", Action: "apt-library", Bindings: map[string]catalog.Binding{"linux": {Provider: "apt", Package: "libatomic1"}}}
	f := newAPTFixture(t)
	f.installed["libatomic1:amd64"] = aptPackage{Name: "libatomic1:amd64", Version: "14.2.0", Healthy: true}
	f.installed["libatomic1:i386"] = aptPackage{Name: "libatomic1:i386", Version: "14.2.0"}
	f.verify["libatomic1"] = "ambiguous package name with more than one installed instance\n"
	if found, err := f.apt.Inspect(ctx, r, engine.Want{Present: true}, f.rec); err != nil || found.Status != engine.Satisfied {
		t.Fatal("unqualified catalog package did not resolve to the native architecture", found, err)
	}
	delete(f.installed, "libatomic1:amd64")
	f.installed["libatomic1:arm64"] = aptPackage{Name: "libatomic1:arm64", Version: "14.2.0", Healthy: true}
	found, err := f.apt.Inspect(ctx, r, engine.Want{Present: true}, f.rec)
	if err != nil || found.Status != engine.Missing {
		t.Fatal("foreign architectures alone resolved an unqualified catalog package", found, err)
	}
	if err := f.apt.Apply(ctx, r, found, f.rec); err != nil || f.installed["libatomic1:amd64"].Name == "" || f.installed["libatomic1:arm64"].Version != "14.2.0" {
		t.Fatal("a foreign architecture instance blocked the native installation", err, f.installed)
	}
}

func TestAPTTokenFollowsTheObservation(t *testing.T) {
	ctx := context.Background()
	r := aptFixtureResource("zsh")
	f := newAPTFixture(t)
	first, _ := f.apt.Inspect(ctx, r, engine.Want{Present: true}, f.rec)
	again, _ := f.apt.Inspect(ctx, r, engine.Want{Present: true}, f.rec)
	aptFixtureInstalled(f, "zsh", "1")
	changed, _ := f.apt.Inspect(ctx, r, engine.Want{Present: true}, f.rec)
	if first.Token == "" || first.Token != again.Token || first.Token == changed.Token {
		t.Fatal("the token must be stable for one observation and differ after a change", first.Token, again.Token, changed.Token)
	}
}

func TestAPTRefusesAResourceWithoutItsBindingOrHealthCheck(t *testing.T) {
	ctx := context.Background()
	f := newAPTFixture(t)
	if _, err := f.apt.Inspect(ctx, catalog.Resource{ID: "tool.other", Bindings: map[string]catalog.Binding{"linux": {Provider: "archive", Package: "other"}}}, engine.Want{Present: true}, f.rec); err == nil {
		t.Fatal("accepted a resource without the apt binding")
	}
	aptFixtureInstalled(f, "unknown", "1")
	found, err := f.apt.Inspect(ctx, aptFixtureResource("unknown"), engine.Want{Present: true}, f.rec)
	if err != nil || found.Status != engine.Blocked || !strings.Contains(found.Reason, "lacks its APT command health check") {
		t.Fatal(found, err)
	}
}

func TestAPTHealthIgnoresOnlyMissingFilesThatDpkgExcludes(t *testing.T) {
	ctx := context.Background()
	r := catalog.Resource{ID: "library.first", Name: "Fixture library", Action: "apt-library", Bindings: map[string]catalog.Binding{"linux": {Provider: "apt", Package: "first"}}}
	for name, c := range map[string]struct {
		verify  string
		healthy bool
	}{
		"excluded doc and locale files":   {"missing     /usr/share/doc/first/NEWS.gz\nmissing     /usr/share/locale/ab/LC_MESSAGES/first.mo\nmissing     /usr/share/man/man1/first.1.gz\n", true},
		"excluded file in the short form": {"missing /usr/share/doc/first/NEWS.gz\n", true},
		"re-included file is missing":     {"missing     /usr/share/doc/first/copyright\n", false},
		"last matching option wins":       {"missing     /usr/share/locale/en/LC_MESSAGES/first.mo\n", false},
		"excluded file changed":           {"??5??????   /usr/share/doc/first/NEWS.gz\n", false},
		"missing file outside the globs":  {"missing     /usr/lib/x86_64-linux-gnu/libfirst.so.1\n", false},
		"missing file that is unreadable": {"missing     /usr/share/doc/first/NEWS.gz (Permission denied)\n", false},
		"excluded and other missing file": {"missing     /usr/share/doc/first/NEWS.gz\nmissing     /usr/lib/libfirst.so\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAPTFixture(t)
			if err := os.MkdirAll(filepath.Join(f.apt.DpkgConfiguration, "dpkg.cfg.d"), 0755); err != nil {
				t.Fatal(err)
			}
			for file, text := range map[string]string{
				"dpkg.cfg.d/docker":      "# minimized image\npath-exclude /usr/share/doc/*\npath-include=/usr/share/doc/*/copyright\npath-exclude=/usr/share/man/*\n",
				"dpkg.cfg.d/excludes":    "path-exclude=/usr/share/locale/*\npath-include=/usr/share/locale/en*\n",
				"dpkg.cfg.d/ignored.bak": "path-exclude=/usr/lib/*\n",
				"dpkg.cfg":               "no-debsig\nlog /var/log/dpkg.log\n",
			} {
				if err := os.WriteFile(filepath.Join(f.apt.DpkgConfiguration, file), []byte(text), 0644); err != nil {
					t.Fatal(err)
				}
			}
			aptFixtureInstalled(f, "first", "1")
			f.rec.st.AddSystem("apt", "first")
			f.verify["first:amd64"] = c.verify
			found, err := f.apt.Inspect(ctx, r, engine.Want{Present: true}, f.rec)
			if err != nil || (found.Status == engine.Satisfied) != c.healthy || !c.healthy && (found.Status != engine.Unhealthy || !strings.Contains(found.Reason, "changed package files")) {
				t.Fatal(found, err)
			}
		})
	}
	f := newAPTFixture(t)
	aptFixtureInstalled(f, "first", "1")
	f.verify["first:amd64"] = "missing     /usr/share/doc/first/NEWS.gz\n"
	if found, err := f.apt.Inspect(ctx, r, engine.Want{Present: true}, f.rec); err != nil || found.Status == engine.Satisfied {
		t.Fatal("a missing file without a dpkg exclusion must fail", found, err)
	}
}

func TestAPTPathFilterGlobsFollowDpkg(t *testing.T) {
	filters := parseAPTPathFilters([]byte("path-exclude=/usr/share/*\n--path-include \"/usr/share/doc/?[a-c]*\"\npath-exclude=/opt/[!x]\\*\npath-exclude=/broken/[\n"))
	for path, excluded := range map[string]bool{
		"/usr/share/a/b/c":    true,
		"/usr/share/doc/xb/y": false,
		"/usr/share/doc/xd/y": true,
		"/usr/lib/a":          false,
		"/opt/a*":             true,
		"/opt/x*":             false,
		"/opt/ab":             false,
		"/broken/[":           false,
	} {
		if got := aptPathExcluded(filters, path); got != excluded {
			t.Fatal(path, got)
		}
	}
}
