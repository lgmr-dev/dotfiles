package builds_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/builds"
	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

const python = "tool.python"

func posix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fixture programs are POSIX shell scripts")
	}
}

func debDecoder(t *testing.T) string {
	t.Helper()
	posix(t)
	path := filepath.Join(t.TempDir(), "dpkg-deb")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n[ \"$1\" = --fsys-tarfile ] && [ \"$2\" = - ]\nexec cat\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func rustFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	target := "x86_64-pc-windows-msvc"
	pin := f.add(tarball(t, []entry{{Name: "rust/LICENSE", Text: "retained provenance"}, {Name: "rust/install.sh", Text: "never execute this installer"}, {Name: "rust/rustc/manifest.in", Text: "file:bin/rustc.exe"}, {Name: "rust/rustc/bin/rustc.exe", Text: "rust compiler", Mode: 0755}, {Name: "rust/rustc/bin/rustdoc.exe", Text: "rust documentation", Mode: 0755}}), "tar.gz")
	pin.StripComponents, pin.RustTarget = 1, target
	if err := os.MkdirAll(f.provider.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	f.provider.Launcher = filepath.Join(f.provider.StateDir, "installer-launcher.exe")
	launcher := []byte("trusted fixture launcher")
	if err := os.WriteFile(f.provider.Launcher, launcher, 0755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(launcher)
	f.provider.LauncherSHA256 = hex.EncodeToString(sum[:])
	pin.WindowsRustLauncher = &catalog.WindowsRustLauncherPin{Revision: 1}
	pin.Commands = map[string]string{}
	for _, name := range []string{"rustc", "cargo", "rustfmt", "cargo-fmt", "cargo-clippy", "clippy-driver"} {
		pin.Commands[name] = "bin/" + name + ".exe"
	}
	pin.BinDirs = []string{"bin"}
	pin.RequiredFiles = []string{"lib/rustlib/src/rust/library/core/src/lib.rs"}
	names := catalog.RustComponentNames(target)
	for index, path := range []string{"bin/cargo.exe", "lib/rustlib/x86_64-pc-windows-msvc/lib/libstd-hash.rlib", "bin/rustfmt.exe", "bin/cargo-clippy.exe", "lib/rustlib/src/rust/library/core/src/lib.rs"} {
		entries := []entry{{Name: "rust/" + names[index] + "/manifest.in", Text: "file:" + path}, {Name: "rust/" + names[index] + "/" + path, Text: path, Mode: 0755}}
		if index == 1 {
			entries = append(entries, entry{Name: "rust/" + names[index] + "/lib/rustlib/" + target + "/lib/libcore-hash.rlib", Text: "core"})
		}
		if index == 2 || index == 3 {
			name := "cargo-fmt"
			if index == 3 {
				name = "clippy-driver"
			}
			entries = append(entries, entry{Name: "rust/" + names[index] + "/bin/" + name + ".exe", Text: name, Mode: 0755})
		}
		component := f.add(tarball(t, entries), "tar.gz")
		component.StripComponents = 1
		component.RequiredFiles = []string{names[index] + "/manifest.in"}
		pin.RustComponents = append(pin.RustComponents, component)
	}
	f.provider.Pins[shared] = pin
	return f
}

func TestRustBuildMergesItsComponentsAndBindsTheLauncher(t *testing.T) {
	f := rustFixture(t)
	build := f.mustApply(shared)
	if read(t, filepath.Join(build, "LICENSE")) != "retained provenance" || read(t, filepath.Join(build, "bin", "cargo.exe")) != "bin/cargo.exe" || read(t, filepath.Join(build, "lib", "rustlib", "src", "rust", "library", "core", "src", "lib.rs")) == "" {
		t.Fatal("the build lost a component or the upstream metadata")
	}
	for name, original := range map[string]string{"rustc.exe": "rust compiler", "rustdoc.exe": "rust documentation", "clippy-driver.exe": "clippy-driver"} {
		if read(t, filepath.Join(build, "bin", name)) != "trusted fixture launcher" || read(t, filepath.Join(build, "bin", "dotfiles-original-"+name)) != original {
			t.Fatal("the launcher or the original command differs", name)
		}
	}
	requests := f.requests.Load()
	rebuilt := []byte("unrelated installer rebuild")
	if err := os.WriteFile(f.provider.Launcher, rebuilt, 0755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rebuilt)
	f.provider.LauncherSHA256 = hex.EncodeToString(sum[:])
	if finding := f.inspect(shared, true); finding.Status != engine.Satisfied {
		t.Fatal("a new installer build made the Rust build", finding.Status)
	}
	if again := f.mustApply(shared); again != build || f.requests.Load() != requests || read(t, filepath.Join(build, "bin", "rustc.exe")) != "trusted fixture launcher" {
		t.Fatal("a new installer build replaced the verified Rust build")
	}
	write(t, filepath.Join(build, "bin", "rustc.exe"), "changed installed launcher")
	if finding := f.inspect(shared, true); finding.Status != engine.Unhealthy {
		t.Fatal("a changed launcher is", finding.Status)
	}
}

func TestRustLauncherMustBeTheObservedInstallerBeforeTheDownload(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*fixture)
	}{
		{"changed executable", func(f *fixture) {
			if err := os.WriteFile(f.provider.Launcher, []byte("other bytes"), 0755); err != nil {
				t.Fatal(err)
			}
		}},
		{"no hash", func(f *fixture) { f.provider.LauncherSHA256 = "" }},
		{"no executable", func(f *fixture) { f.provider.Launcher = "" }},
		{"no launcher recipe", func(f *fixture) {
			pin := f.provider.Pins[shared]
			pin.WindowsRustLauncher = nil
			f.provider.Pins[shared] = pin
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := rustFixture(t)
			test.change(f)
			if err := f.apply(shared); err == nil || f.requests.Load() != 0 {
				t.Fatal("the preparation started without the observed launcher", err, f.requests.Load())
			}
			entries, err := os.ReadDir(builds.ResourceDir(f.provider.StateDir, shared))
			if err != nil || len(entries) != 0 {
				t.Fatal("the refused preparation left an entry", entries, err)
			}
		})
	}
}

func TestPythonBuildCompilesItsStandardLibraryBeforeTheManifest(t *testing.T) {
	posix(t)
	f := newFixture(t)
	script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$PWD\" \"$@\" > compile-arguments\n"
	pin := f.add(tarball(t, []entry{{Name: "python/bin/python3", Text: script, Mode: 0755}, {Name: "python/lib/python3/os.py", Text: "pass\n"}}), "tar.gz")
	pin.StripComponents, pin.PythonStdlib = 1, "lib/python3"
	pin.Commands, pin.BinDirs = map[string]string{"python3": "bin/python3"}, []string{"bin"}
	f.provider.Pins[python] = pin
	build := f.mustApply(python)
	arguments := strings.Split(strings.TrimSpace(read(t, filepath.Join(build, "compile-arguments"))), "\n")
	want := []string{build, "-I", "-B", "-m", "compileall", "-q", "-f", "-j", "1", "--invalidation-mode", "checked-hash", "-o", "0", "-o", "1", "-o", "2", "lib/python3"}
	if !slices.Equal(arguments, want) {
		t.Fatal("unexpected bytecode preparation", arguments)
	}
}

func pythonTool(f *fixture, version string) catalog.ArchivePin {
	pin := f.add(tarball(f.t, []entry{{Name: "bin/python3", Text: "python " + version, Mode: 0755}}), "tar.gz")
	pin.Version = version
	pin.Commands, pin.BinDirs = map[string]string{"python3": "bin/python3", "python": "bin/python3"}, []string{"bin"}
	return pin
}

func venvHook(binary string) func(platform.Command) error {
	return func(c platform.Command) error {
		if !slices.Contains(c.Args, "venv") {
			return nil
		}
		payload := c.Args[len(c.Args)-1]
		interpreter := "bin/python"
		if runtime.GOOS == "windows" {
			interpreter = "Scripts/python.exe"
		}
		for _, name := range []string{interpreter, binary} {
			path := filepath.Join(payload, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte("fixture program"), 0755); err != nil {
				return err
			}
		}
		return nil
	}
}

func consoleCommand(name string) (source, public string, revision int) {
	if runtime.GOOS == "windows" {
		return "Scripts/" + name + ".exe", "commands/" + name + ".exe", 1
	}
	return "bin/" + name, "commands/" + name, 0
}

func yamllintFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.provider.Pins[python] = pythonTool(f, "3.13.1")
	f.mustApply(python)
	pin := f.add([]byte("yamllint wheel"), "file")
	pin.Version, pin.File = "1.38.0", "yamllint-1.38.0-py3-none-any.whl"
	source, public, revision := consoleCommand("yamllint")
	pin.WindowsVenvRevision = revision
	pin.Commands, pin.BinDirs = map[string]string{"yamllint": public}, []string{"commands"}
	pathspec := f.add([]byte("pathspec wheel"), "file")
	pathspec.Version, pathspec.File = "1.1.1", "pathspec-1.1.1-py3-none-any.whl"
	pathspec.RequiredFiles = []string{pathspec.File}
	pyyaml := f.add([]byte("PyYAML wheel"), "file")
	pyyaml.Version, pyyaml.File = "6.0.3", "pyyaml-6.0.3-cp313-cp313-macosx_11_0_arm64.whl"
	pyyaml.RequiredFiles = []string{pyyaml.File}
	pin.Yamllint = &catalog.YamllintPin{Pathspec: pathspec, PyYAML: pyyaml, PythonPinID: strings.Repeat("a", 64)}
	f.provider.Pins[shared] = pin
	f.runner.hook = venvHook(source)
	return f
}

func TestYamllintBuildUsesOnlyPinnedOfflineWheelsAndTheExactPythonBuild(t *testing.T) {
	f := yamllintFixture(t)
	base := f.build(python)
	build := f.mustApply(shared)
	commands := f.runner.commands
	if len(commands) != 6 {
		t.Fatal("unexpected preparation commands", len(commands))
	}
	interpreter := filepath.Join(base, "bin", "python3")
	if runtime.GOOS == "windows" {
		interpreter = platform.WindowsExtendedPath(interpreter)
	}
	if commands[0].Path != interpreter || strings.Contains(commands[0].Path, "current") {
		t.Fatal("the environment does not use the exact Python build", commands[0].Path)
	}
	for _, command := range commands[1:4] {
		for _, flag := range []string{"-I", "-B", "--isolated", "--no-index", "--require-hashes", "--no-deps", "--only-binary=:all:", "--no-cache-dir"} {
			if !slices.Contains(command.Args, flag) {
				t.Fatal("lost offline wheel control", flag)
			}
		}
		fields := strings.Fields(read(t, command.Args[len(command.Args)-1]))
		if len(fields) != 4 || fields[1] != "@" || !strings.HasPrefix(fields[3], "--hash=sha256:") {
			t.Fatal("unpinned requirement", fields)
		}
		local, err := url.Parse(fields[2])
		if err != nil || local.Scheme != "file" || local.Host != "" {
			t.Fatal("online requirement", fields, err)
		}
	}
	for _, command := range commands {
		if !slices.Contains(command.Env, "PIP_CONFIG_FILE="+strings.ToLower(os.DevNull)) || !slices.Contains(command.Env, "PYTHONPATH=") || !slices.Contains(command.Env, "HOME="+filepath.Join(build, ".build-home")) {
			t.Fatal("the preparation inherited Python or pip configuration", command.Env)
		}
		for _, name := range []string{"TMP=", "TEMP=", "TMPDIR="} {
			if !slices.Contains(command.Env, name+os.TempDir()) {
				t.Fatal("the temporary folder of the build is below the build", command.Env)
			}
		}
	}
	if !slices.Contains(commands[4].Args, "checked-hash") || !slices.Contains(commands[5].Args, "-B") {
		t.Fatal("ordinary imports would change the build")
	}
	pin := f.provider.Pins[shared]
	if read(t, builds.CommandPath(f.provider.StateDir, shared, "yamllint", pin)) != "fixture program" {
		t.Fatal("the public console is not a copy of the generated console")
	}
	var manifest builds.Manifest
	if err := platform.Decode([]byte(read(t, build+".manifest.json")), &manifest); err != nil || !slices.Equal(manifest.Uses, []string{base}) || manifest.Digest != builds.Digest(pin, []string{base}) {
		t.Fatal("the manifest does not record the Python build", manifest, err)
	}
	if f.inspect(shared, true).Status != engine.Satisfied || len(f.runner.commands) != 6 {
		t.Fatal("an inspection ran a command")
	}
}

func TestDependentBuildIsRebuiltWhenItsBaseBuildChanges(t *testing.T) {
	f := yamllintFixture(t)
	oldBase := f.build(python)
	old := f.mustApply(shared)
	before := f.inspect(shared, true)
	f.provider.Pins[python] = pythonTool(f, "3.13.2")
	if finding := f.inspect(python, true); finding.Status != engine.Outdated {
		t.Fatal("the base is", finding.Status)
	}
	planned := f.inspect(shared, true)
	if planned.Status != engine.Outdated || planned.Token != before.Token {
		t.Fatal("the dependent build does not follow the planned base change", planned.Status, planned.Reason)
	}
	newBase := f.mustApply(python)
	if removed, left := f.sweep(); len(removed) != 0 || len(left) != 0 {
		t.Fatal("the sweep removed a base build that a current build uses", removed, left)
	}
	if read(t, filepath.Join(oldBase, "bin", "python3")) != "python 3.13.1" {
		t.Fatal("the old base build changed")
	}
	fresh := f.inspect(shared, true)
	if fresh.Status != engine.Outdated || fresh.Token != planned.Token {
		t.Fatal("a base change altered the token of the dependent resource", fresh.Status)
	}
	f.runner.commands = nil
	build := f.mustApply(shared)
	interpreter := filepath.Join(newBase, "bin", "python3")
	if runtime.GOOS == "windows" {
		interpreter = platform.WindowsExtendedPath(interpreter)
	}
	if build == old || f.runner.commands[0].Path != interpreter {
		t.Fatal("the dependent build was not rebuilt with the new base", build, f.runner.commands[0].Path)
	}
	var manifest builds.Manifest
	if err := platform.Decode([]byte(read(t, build+".manifest.json")), &manifest); err != nil || !slices.Equal(manifest.Uses, []string{newBase}) {
		t.Fatal(manifest, err)
	}
	removed, left := f.sweep()
	want := []string{oldBase, old}
	slices.Sort(want)
	if !slices.Equal(removed, want) || len(left) != 0 {
		t.Fatal("the sweep did not remove the old base and its dependent build", removed, left)
	}
}

func TestDependentBuildWaitsForItsBase(t *testing.T) {
	f := yamllintFixture(t)
	if err := f.remove(python); err != nil {
		t.Fatal(err)
	}
	if finding := f.inspect(shared, true); finding.Status != engine.Missing {
		t.Fatal(finding)
	}
	if err := f.apply(shared); err == nil || !strings.Contains(err.Error(), python) || len(f.runner.commands) != 0 {
		t.Fatal("a dependent build started without its base", err)
	}
}

func TestYamllintRequiresEachDependencyChecksumBeforeACommand(t *testing.T) {
	f := yamllintFixture(t)
	pin := f.provider.Pins[shared]
	pin.Yamllint.PyYAML.SHA256 = strings.Repeat("c", 64)
	if err := f.apply(shared); err == nil || len(f.runner.commands) != 0 {
		t.Fatal("an unchecked dependency reached Python", err, len(f.runner.commands))
	}
}

func TestInterruptedPythonToolBuildIsNotReused(t *testing.T) {
	f := yamllintFixture(t)
	hook, failed := f.runner.hook, false
	f.runner.hook = func(c platform.Command) error {
		if !failed && slices.Contains(c.Args, "pip") {
			failed = true
			return errors.New("interrupted offline pip")
		}
		return hook(c)
	}
	if err := f.apply(shared); err == nil {
		t.Fatal("an interrupted preparation published")
	}
	absent(t, builds.CurrentDir(f.provider.StateDir, shared))
	build := f.mustApply(shared)
	if serial(build) != "2" || len(f.runner.commands) != 8 {
		t.Fatal("the next run reused the interrupted build", build, len(f.runner.commands))
	}
}

func TestLatex2textBuildKeepsItsIsolatedSourceBuild(t *testing.T) {
	f := newFixture(t)
	f.provider.Pins[python] = pythonTool(f, "3.13.1")
	f.mustApply(python)
	pin := f.add([]byte("pinned source archive"), "file")
	pin.Version, pin.File = "2.11", "pylatexenc-2.11.tar.gz"
	source, public, revision := consoleCommand("latex2text")
	pin.WindowsVenvRevision = revision
	pin.Commands, pin.BinDirs = map[string]string{"latex2text": public}, []string{"commands"}
	backend := f.add([]byte("pinned build backend"), "file")
	backend.Version, backend.File = "84.0.0", "setuptools-84.0.0-py3-none-any.whl"
	backend.RequiredFiles = []string{backend.File}
	pin.Latex2text = &catalog.Latex2textPin{Backend: backend, PythonPinID: strings.Repeat("c", 64), ConsoleRevision: 1}
	f.provider.Pins[shared] = pin
	f.runner.hook = venvHook(source)
	f.mustApply(shared)
	commands := f.runner.commands
	if len(commands) != 6 {
		t.Fatal("unexpected preparation commands", len(commands))
	}
	for _, index := range []int{1, 2} {
		for _, flag := range []string{"--isolated", "--no-index", "--require-hashes", "--no-deps", "--no-cache-dir"} {
			if !slices.Contains(commands[index].Args, flag) {
				t.Fatal("lost isolated pinned install", commands[index].Args)
			}
		}
	}
	if !slices.Contains(commands[2].Args, "--no-binary=pylatexenc") || !slices.Contains(commands[2].Args, "--no-build-isolation") {
		t.Fatal("the source build changed")
	}
	if !strings.Contains(commands[3].Args[len(commands[3].Args)-1], "stream.reconfigure(encoding='utf-8')") || !slices.Contains(commands[5].Args, "-B") {
		t.Fatal("the console is not the UTF-8 console, or the probe can write bytecode")
	}
	old := pin
	recipe := *pin.Latex2text
	recipe.ConsoleRevision = 0
	old.Latex2text = &recipe
	f.provider.Pins[shared] = old
	if err := f.apply(shared); err == nil || len(f.runner.commands) != 6 {
		t.Fatal("an old console recipe was prepared", err)
	}
}

func TestAzureCLIBuildCompilesItsPackagesAndStartsFromItsOwnPython(t *testing.T) {
	posix(t)
	f := newFixture(t)
	packages := "libexec/lib/python3.14/site-packages"
	interpreter := `#!/bin/sh
set -eu
if [ "$1" = -B ]; then
	printf '%s\n' "$PYTHONPATH" "$AZ_INSTALLER" "$@"
	exit 0
fi
for last; do :; done
case " $* " in
*" -b "*) /usr/bin/find "$last" -name '*.py' | while read -r file; do printf compiled > "${file}c"; done ;;
esac
`
	runtimePin := f.add(tarball(t, []entry{{Name: "python/bin/python3.14", Text: interpreter, Mode: 0755}, {Name: "python/lib/python3.14/os.py", Text: "pass\n"}}), "tar.gz")
	runtimePin.Version, runtimePin.StripComponents = "3.14.2", 1
	runtimePin.Commands, runtimePin.BinDirs, runtimePin.RequiredFiles = map[string]string{}, []string{}, []string{"bin/python3.14"}
	pin := f.add(tarball(t, []entry{{Name: packages + "/azure/cli/__main__.py", Text: "main\n"}, {Name: packages + "/azure/cli/core.py", Text: "core\n"}, {Name: packages + "/azure/data.json", Text: "{}"}}), "tar.gz")
	pin.Commands, pin.BinDirs = map[string]string{"az": "commands/az"}, []string{"commands"}
	pin.RequiredFiles = []string{packages + "/azure/cli/__main__.pyc"}
	pin.AzureCLI = &catalog.AzureCLIPin{Python: runtimePin}
	f.provider.Pins[shared] = pin
	build := f.mustApply(shared)
	for _, name := range []string{"azure/cli/__main__", "azure/cli/core"} {
		absent(t, filepath.Join(build, filepath.FromSlash(packages), filepath.FromSlash(name)+".py"))
		if read(t, filepath.Join(build, filepath.FromSlash(packages), filepath.FromSlash(name)+".pyc")) != "compiled" {
			t.Fatal("a module has no bytecode", name)
		}
	}
	if read(t, filepath.Join(build, filepath.FromSlash(packages), "azure", "data.json")) != "{}" {
		t.Fatal("the preparation removed a data file")
	}
	output, err := exec.Command(builds.CommandPath(f.provider.StateDir, shared, "az", pin), "version", "a b").Output()
	want := filepath.Join(build, filepath.FromSlash(packages)) + "\nTARBALL\n-B\n-s\n-P\n-m\nazure.cli\nversion\na b\n"
	if err != nil || string(output) != want {
		t.Fatal("the launcher does not start the private interpreter", string(output), err)
	}
	if f.inspect(shared, true).Status != engine.Satisfied {
		t.Fatal("a run of the command changed the build")
	}
}

func TestZenBuildDisablesItsUpdaterAndKeepsItsLockStable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the Zen archive is for Linux only")
	}
	f := newFixture(t)
	program := "#!/bin/sh\nprintf '%s\\n' \"$MOZ_LEGACY_PROFILES\" \"$@\"\n"
	packer := exec.Command("/usr/bin/xz", "--compress", "--stdout")
	packer.Stdin = bytes.NewReader(plainTar(t, []entry{{Name: "zen/zen", Text: program, Mode: 0755}, {Name: "zen/zen-bin", Text: program, Mode: 0755}}))
	data, err := packer.Output()
	if err != nil {
		t.Fatal("the Linux extraction fixture requires xz", err)
	}
	pin := f.add(data, "tar.xz")
	pin.StripComponents, pin.ZenProfile, pin.ZenProfileRevision = 1, true, 1
	pin.Commands, pin.BinDirs, pin.RequiredFiles = map[string]string{"zen": "zen"}, []string{"."}, []string{"zen-bin"}
	f.provider.Pins[shared] = pin
	build := f.mustApply(shared)
	if read(t, filepath.Join(build, "distribution", "policies.json")) != "{\"policies\":{\"DisableAppUpdate\":true}}\n" {
		t.Fatal("the updater policy is missing")
	}
	output, err := exec.Command(builds.CommandPath(f.provider.StateDir, shared, "zen", pin), "https://example.com/a b?c=$d").CombinedOutput()
	if err != nil || string(output) != "1\nhttps://example.com/a b?c=$d\n" {
		t.Fatal(string(output), err)
	}
	lock, err := os.OpenFile(filepath.Join(build, ".parentlock"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if f.inspect(shared, true).Status != engine.Satisfied {
		t.Fatal("a normal start changed the build")
	}
	write(t, filepath.Join(build, ".parentlock"), "personal change")
	if f.inspect(shared, true).Status != engine.Unhealthy {
		t.Fatal("a change of the lock file is not found")
	}
}

func ghosttyPin(f *fixture) catalog.ArchivePin {
	entries := []entry{{Name: "usr/bin/ghostty", Text: "#!/bin/sh\nprintf '%s\\n' \"$LD_LIBRARY_PATH\" \"$@\"\n", Mode: 0755}}
	required := []string{"usr/bin/ghostty-bin", "usr/lib/libgtk4-layer-shell.so", "usr/share/terminfo/x/xterm-ghostty", "usr/share/ghostty/themes/Rose Pine"}
	for _, name := range required[1:] {
		entries = append(entries, entry{Name: name, Text: "private runtime"})
	}
	pin := f.add(plainTar(f.t, entries), "deb")
	pin.Version, pin.GhosttyLibraries = "1.3.1", true
	pin.Commands, pin.BinDirs, pin.RequiredFiles = map[string]string{"ghostty": "usr/bin/ghostty"}, []string{"usr/bin"}, required
	return pin
}

func TestGhosttyBuildStartsItsBinaryWithThePrivateLibraries(t *testing.T) {
	f := newFixture(t)
	f.provider.DpkgDeb = debDecoder(t)
	pin := ghosttyPin(f)
	f.provider.Pins[shared] = pin
	build := f.mustApply(shared)
	command := exec.Command(builds.CommandPath(f.provider.StateDir, shared, "ghostty", pin), "argument with spaces", "$literal")
	command.Env = []string{"PATH=/usr/bin:/bin", "LD_LIBRARY_PATH=/prior-library-path"}
	output, err := command.CombinedOutput()
	if err != nil || string(output) != filepath.Join(build, "usr", "bin")+"/../lib:/prior-library-path\nargument with spaces\n$literal\n" {
		t.Fatal("the wrapper lost the library path or a literal argument", string(output), err)
	}
	if f.inspect(shared, true).Status != engine.Satisfied {
		t.Fatal("a run of Ghostty changed the build")
	}
}

func TestGhosttyBuildRefusesAPackageThatHasThePreparedBinary(t *testing.T) {
	f := newFixture(t)
	f.provider.DpkgDeb = debDecoder(t)
	pin := ghosttyPin(f)
	entries := []entry{{Name: "usr/bin/ghostty", Text: "ghostty", Mode: 0755}, {Name: "usr/bin/ghostty-bin", Text: "ghostty-bin", Mode: 0755}}
	for _, name := range pin.RequiredFiles[1:] {
		entries = append(entries, entry{Name: name, Text: "private runtime"})
	}
	conflict := f.add(plainTar(t, entries), "deb")
	pin.URL, pin.SHA256 = conflict.URL, conflict.SHA256
	f.provider.Pins[shared] = pin
	if err := f.apply(shared); err == nil {
		t.Fatal("the preparation replaced an existing binary")
	}
	absent(t, builds.CurrentDir(f.provider.StateDir, shared))
}

func TestT3CodeBuildStartsTheAppWithoutTheSandboxAndTheUpdater(t *testing.T) {
	f := newFixture(t)
	f.provider.DpkgDeb = debDecoder(t)
	binary := "opt/T3 Code (Beta)/t3code"
	pin := f.add(plainTar(t, []entry{{Name: binary, Text: "#!/bin/sh\nprintf '%s\\n' \"$T3CODE_DISABLE_AUTO_UPDATE\" \"$T3CODE_DESKTOP_LAUNCHER_PATH\" \"$@\"\n", Mode: 0755}}), "deb")
	pin.T3CodeLauncher, pin.RequiredFiles = true, []string{"t3-code", binary}
	f.provider.Pins[shared] = pin
	f.mustApply(shared)
	launcher := builds.RequiredFilePath(f.provider.StateDir, shared, "t3-code", pin)
	output, err := exec.Command(launcher, "t3code://open?path=a b").Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
	if want := []string{"1", launcher, "--no-sandbox", "t3code://open?path=a b"}; !slices.Equal(got, want) {
		t.Fatalf("launcher started %q, want %q", got, want)
	}
}

var portableGitFiles = []string{
	"cmd/git.exe", "cmd/git-gui.exe", "cmd/gitk.exe", "bin/bash.exe", "bin/sh.exe",
	"git-bash.exe", "git-cmd.exe", "usr/bin/bash.exe", "usr/bin/ssh.exe", "usr/bin/ssh-keygen.exe",
	"usr/bin/msys-2.0.dll", "usr/bin/msys-crypto-3.dll", "usr/bin/msys-z.dll",
	"usr/bin/msys-iconv-2.dll", "usr/bin/msys-intl-8.dll", "usr/bin/msys-readline8.dll", "usr/bin/msys-ncursesw6.dll",
	"ucrt64/bin/git.exe", "ucrt64/bin/git-lfs.exe", "ucrt64/bin/git-credential-manager.exe",
	"ucrt64/bin/libcurl-4.dll", "ucrt64/bin/libpcre2-8-0.dll", "ucrt64/bin/libiconv-2.dll", "ucrt64/bin/libintl-8.dll",
	"ucrt64/bin/libcrypto-3-x64.dll", "ucrt64/bin/libssl-3-x64.dll", "ucrt64/bin/zlib1.dll",
	"ucrt64/bin/tcl86.dll", "ucrt64/bin/tk86.dll", "ucrt64/libexec/git-core/git-gui",
	"etc/gitconfig", "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", "README.portable",
}

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func portableGitFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	data := []byte("verified fixture self-extractor")
	sum := sha256.Sum256(data)
	pin := catalog.ArchivePin{Version: "2.56.0.windows.1", URL: "https://github.com/git-for-windows/git/releases/download/v2.56.0.windows.1/PortableGit-2.56.0-64-bit.7z.exe", SHA256: hex.EncodeToString(sum[:]),
		Format: "file", File: "portable-git.exe", PortableGit: true, PortableGitRevision: 1,
		Commands: map[string]string{"git": "cmd/git.exe", "bash": "bin/bash.exe", "sh": "bin/sh.exe"}, BinDirs: []string{"cmd"}, RequiredFiles: portableGitFiles}
	f.provider.Pins[shared] = pin
	f.provider.Client = &http.Client{Transport: transport(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != pin.URL {
			t.Error("unexpected download URL")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header), Request: request}, nil
	})}
	f.runner.hook = func(c platform.Command) error {
		if filepath.Base(c.Path) != pin.File {
			return nil
		}
		payload := filepath.Dir(c.Path)
		output := payload
		if runtime.GOOS == "windows" {
			output = platform.WindowsExtendedPath(payload)
		}
		if !slices.Equal(c.Args, []string{"-y", "-o" + output}) {
			t.Error("unreviewed self-extractor arguments", c.Args)
		}
		for _, file := range pin.RequiredFiles {
			write(t, filepath.Join(payload, filepath.FromSlash(file)), "runtime fixture")
		}
		return nil
	}
	return f
}

func TestPortableGitBuildRunsTheFixedPreparationInAPrivateEnvironment(t *testing.T) {
	f := portableGitFixture(t)
	build := f.mustApply(shared)
	commands := f.runner.commands
	if len(commands) != 4 {
		t.Fatal("the extraction and the runtime probes did not complete", len(commands))
	}
	for _, command := range commands {
		if !slices.Contains(command.Env, "HOME="+filepath.Join(build, ".prepare-home")) || !slices.Contains(command.Env, "GIT_CONFIG_GLOBAL=/dev/null") || !slices.Contains(command.Env, "MSYS=winsymlinks:sys") {
			t.Fatal("the preparation can use personal configuration or an unsupported link format")
		}
	}
	if want := []string{"-c", "core.longpaths=true", "config", "--file", filepath.Join(build, "etc", "gitconfig"), "core.longpaths", "true"}; commands[1].Path != filepath.Join(build, "cmd", "git.exe") || !slices.Equal(commands[1].Args, want) {
		t.Fatal("the private long-path configuration changed", commands[1])
	}
	if commands[2].Path != filepath.Join(build, "bin", "bash.exe") || commands[2].Args[len(commands[2].Args)-1] != "2.56.0.windows.1" || !slices.Equal(commands[3].Args, []string{"--version"}) {
		t.Fatal("the runtime probes changed")
	}
	absent(t, filepath.Join(build, "portable-git.exe"))
	absent(t, filepath.Join(build, ".prepare-post-install.bat"))
	if f.inspect(shared, true).Status != engine.Satisfied || len(f.runner.commands) != 4 {
		t.Fatal("an inspection ran a command")
	}
}

func TestPortableGitCompletesTheUpstreamPostInstallAfterExtraction(t *testing.T) {
	for _, outcome := range []string{"complete", "failed", "failed-after-cleanup", "unfinished", "changed-copy", "occupied-copy"} {
		t.Run(outcome, func(t *testing.T) {
			f := portableGitFixture(t)
			extract, calls := f.runner.hook, 0
			f.runner.hook = func(c platform.Command) error {
				err := extract(c)
				payload := filepath.Dir(c.Path)
				switch filepath.Base(c.Path) {
				case "portable-git.exe":
					write(t, filepath.Join(payload, "post-install.bat"), "upstream fixture")
					if err := os.MkdirAll(filepath.Join(payload, "etc", "post-install"), 0700); err != nil {
						t.Fatal(err)
					}
					if outcome == "occupied-copy" {
						write(t, filepath.Join(payload, ".prepare-post-install.bat"), "preserve occupied path")
					}
				case "git-bash.exe":
					calls++
					if !slices.Equal(c.Args, []string{"--no-needs-console", "--hide", "--cd=" + payload, "--command=.prepare-post-install.bat"}) {
						t.Fatal("post-install must run an unchanged sibling copy in its ordinary payload folder", c.Args)
					}
					if outcome == "failed" {
						return errors.New("upstream post-install failed")
					}
					if read(t, filepath.Join(payload, ".prepare-post-install.bat")) != read(t, filepath.Join(payload, "post-install.bat")) {
						t.Fatal("the interpreter input is not an exact copy of the verified upstream script")
					}
					if outcome == "complete" || outcome == "failed-after-cleanup" || outcome == "changed-copy" {
						for _, name := range []string{"post-install.bat", "etc/post-install"} {
							if err := os.Remove(filepath.Join(payload, filepath.FromSlash(name))); err != nil {
								t.Fatal(err)
							}
						}
					}
					if outcome == "changed-copy" {
						write(t, filepath.Join(payload, ".prepare-post-install.bat"), "changed input")
					}
					if outcome == "failed-after-cleanup" {
						return errors.New("post-install failed after deleting its artifacts")
					}
				}
				return err
			}
			err := f.apply(shared)
			wantCalls, wantCommands := 1, 2
			if outcome == "occupied-copy" {
				wantCalls, wantCommands = 0, 1
			}
			if outcome == "complete" || outcome == "changed-copy" {
				wantCommands = 5
			}
			if calls != wantCalls || (err == nil) != (outcome == "complete") || len(f.runner.commands) != wantCommands {
				t.Fatalf("post-install calls=%d commands=%d error=%v", calls, len(f.runner.commands), err)
			}
			if outcome == "complete" {
				absent(t, filepath.Join(f.build(shared), ".prepare-post-install.bat"))
				return
			}
			absent(t, builds.CurrentDir(f.provider.StateDir, shared))
			entries, err := os.ReadDir(builds.ResourceDir(f.provider.StateDir, shared))
			if err != nil || len(entries) != 1 {
				t.Fatal("the failed preparation did not leave exactly its unfinished build", entries, err)
			}
			copied := read(t, filepath.Join(builds.ResourceDir(f.provider.StateDir, shared), entries[0].Name(), ".prepare-post-install.bat"))
			if outcome == "occupied-copy" && copied != "preserve occupied path" || outcome == "changed-copy" && copied != "changed input" {
				t.Fatal("the preparation replaced or removed an input that it did not write", copied)
			}
		})
	}
}

func TestPortableGitRefusesAnOldRecipe(t *testing.T) {
	f := portableGitFixture(t)
	pin := f.provider.Pins[shared]
	pin.PortableGitRevision = 0
	f.provider.Pins[shared] = pin
	if err := f.apply(shared); err == nil || len(f.runner.commands) != 0 {
		t.Fatal("an old PortableGit recipe ran a command", err)
	}
}
