//go:build darwin || linux

package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

const debFixtureCodeVersion = "1.2.3-4"

const debFixtureCodeCache = "/usr/share/code/resources/app/node-compile-cache"

var debFixtureCode = catalog.Resource{ID: "tool.vscode", Name: "Visual Studio Code", Action: "apt", Bindings: map[string]catalog.Binding{"linux": {Provider: "apt", Package: "code"}}}

var debFixtureTailscale = catalog.Resource{ID: "tool.tailscale", Name: "Tailscale", Action: "apt", Bindings: map[string]catalog.Binding{"linux": {Provider: "apt", Package: "tailscale"}}}

type debFixture struct {
	*aptFixture
	payload   []byte
	downloads int
}

func newDebFixture(t *testing.T) *debFixture {
	t.Helper()
	f := &debFixture{aptFixture: newAPTFixture(t), payload: []byte("reviewed package bytes")}
	sum := sha256.Sum256(f.payload)
	digest, size := hex.EncodeToString(sum[:]), int64(len(f.payload))
	f.apt.Pins = catalog.Pins{
		LinuxCode: map[string]catalog.LinuxCodePin{"amd64": {Version: debFixtureCodeVersion, Architecture: "amd64", SHA256: digest, Size: size,
			URL: "https://vscode.download.prss.microsoft.com/dbazure/download/stable/" + strings.Repeat("a", 40) + "/code_" + debFixtureCodeVersion + "_amd64.deb"}},
		LinuxCodeModes: map[string]catalog.LinuxCodeModes{debFixtureCodeVersion: {Sandbox: "4755", RootOnly: debFixtureCodeCache}},
		LinuxPackages: map[string]map[string]catalog.LinuxPackagePin{"tool.tailscale": {"amd64": {Package: "tailscale", Version: "1.2.3", Architecture: "amd64", SHA256: digest, Size: size,
			URL: "https://pkgs.tailscale.com/stable/tailscale_1.2.3_amd64.deb"}}},
	}
	f.apt.Download = func(ctx context.Context, url, sha256 string, size int64, destination string) error {
		f.downloads++
		if sha256 != digest || size != int64(len(f.payload)) || !strings.HasPrefix(url, "https://") {
			t.Fatal("the download must carry the reviewed pin", url, sha256, size)
		}
		return os.WriteFile(destination, f.payload, 0600)
	}
	return f
}

func TestCodePinnedInstallUsesTheVerifiedPayloadAndTheFixedDebconfInput(t *testing.T) {
	ctx := context.Background()
	f := newDebFixture(t)
	found, err := f.apt.Inspect(ctx, debFixtureCode, engine.Want{Present: true}, f.rec)
	if err != nil || found.Status != engine.Missing || found.Reason != "install reviewed Code pin "+debFixtureCodeVersion || f.downloads != 0 {
		t.Fatal(found, err)
	}
	payload := filepath.Join(f.apt.Directory, "payloads", f.apt.Pins.LinuxCode["amd64"].SHA256, "code.deb")
	input := ""
	f.during = func(c platform.Command) {
		if !slices.Contains(c.Args, "install") {
			return
		}
		override := c.Args[3]
		prefix := "DEBCONF_DB_OVERRIDE=File{filename:" + f.apt.Temporary + "/dotfiles-code-debconf-"
		suffix := "/answer.dat accept_name:^code/add-(copilot-command|microsoft-repo)$ mode:0644 backup:no}"
		if !strings.HasPrefix(override, prefix) || !strings.HasSuffix(override, suffix) || c.Args[4] != "/fake/apt-get" {
			t.Fatal("Code must install with its one-question debconf database", c.Args)
		}
		input = strings.TrimSuffix(strings.TrimPrefix(override, "DEBCONF_DB_OVERRIDE=File{filename:"), suffix)
		database, err := os.ReadFile(filepath.Join(input, "answer.dat"))
		if err != nil || string(database) != debCodeDatabase || strings.Count(string(database), "Value: false") != 2 {
			t.Fatal("the debconf input must decline the Microsoft repository and the Copilot command", string(database), err)
		}
		data, err := os.ReadFile(payload)
		if err != nil || string(data) != string(f.payload) || c.Args[len(c.Args)-1] != payload {
			t.Fatal("APT must receive the verified package file", c.Args, err)
		}
		if slices.Contains(c.Args, "--allow-downgrades") || !slices.Contains(c.Args, "--no-remove") {
			t.Fatal(c.Args)
		}
	}
	if err := f.apt.Apply(ctx, debFixtureCode, found, f.rec); err != nil {
		t.Fatal(err)
	}
	if input == "" || f.downloads != 1 {
		t.Fatal("the install command did not run with its input", input, f.downloads)
	}
	for _, path := range []string{input, filepath.Dir(payload)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("a verified install must delete its temporary input", path, err)
		}
	}
	st := f.rec.State()
	found, err = f.apt.Inspect(ctx, debFixtureCode, engine.Want{Present: true, Update: true}, f.rec)
	if err != nil || found.Status != engine.Satisfied || found.Version != debFixtureCodeVersion || !st.HasSystem("apt", "code") || len(st.Pending) != 0 {
		t.Fatal(found, st, err)
	}
}

func TestCodeCollisionBlocksBeforeAnyChange(t *testing.T) {
	ctx := context.Background()
	f := newDebFixture(t)
	f.collisions = "/usr/local/bin/code\n"
	found, err := f.apt.Inspect(ctx, debFixtureCode, engine.Want{Present: true}, f.rec)
	if err != nil || found.Status != engine.Blocked || !strings.Contains(found.Reason, "existing Code publication must be preserved") || !strings.Contains(found.Reason, "/usr/local/bin/code") {
		t.Fatal(found, err)
	}
	if err := f.apt.Apply(ctx, debFixtureCode, found, f.rec); err == nil || len(f.commands) != 0 || f.downloads != 0 || len(f.rec.State().Pending) != 0 {
		t.Fatal("a personal Code installation must stop the step before a download or a command", err, f.commands)
	}

	f = newDebFixture(t)
	aptFixtureInstalled(f.aptFixture, "code", "1.0.0-1")
	f.rec.st.AddSystem("apt", "code")
	found, err = f.apt.Inspect(ctx, debFixtureCode, engine.Want{Present: true, Update: true}, f.rec)
	if err != nil || found.Status != engine.Outdated {
		t.Fatal(found, err)
	}
	f.collisions = "/usr/local/bin/code\n"
	if err := f.apt.Apply(ctx, debFixtureCode, found, f.rec); err == nil || !strings.Contains(err.Error(), "existing Code publication") || len(f.commands) != 0 {
		t.Fatal("the legacy command that the package script deletes must stop an update", err, f.commands)
	}
}

func TestPinnedPackageGoesToThePinAndNeverToAnOlderVersion(t *testing.T) {
	ctx := context.Background()
	want := engine.Want{Present: true, Update: true}

	f := newDebFixture(t)
	aptFixtureInstalled(f.aptFixture, "code", "1.0.0-1")
	found, err := f.apt.Inspect(ctx, debFixtureCode, want, f.rec)
	if err != nil || found.Status != engine.Satisfied || !strings.Contains(strings.Join(found.Notes, " "), "1.0.0-1") || !strings.Contains(strings.Join(found.Notes, " "), debFixtureCodeVersion) {
		t.Fatal("an older package without a record must stay, with both versions in the report", found, err)
	}
	f.rec.st.AddSystem("apt", "code")
	found, err = f.apt.Inspect(ctx, debFixtureCode, want, f.rec)
	if err != nil || found.Status != engine.Outdated || found.Reason != `Code "1.0.0-1" -> reviewed pin `+debFixtureCodeVersion {
		t.Fatal(found, err)
	}
	if err := f.apt.Apply(ctx, debFixtureCode, found, f.rec); err != nil {
		t.Fatal(err)
	}
	if got := f.installArguments(); len(got) != 1 || !slices.Contains(got[0], "--only-upgrade") || slices.Contains(got[0], "--allow-downgrades") || !strings.HasSuffix(got[0][len(got[0])-1], "/code.deb") {
		t.Fatal(got)
	}
	if f.installed["code:amd64"].Version != debFixtureCodeVersion {
		t.Fatal(f.installed)
	}

	f = newDebFixture(t)
	aptFixtureInstalled(f.aptFixture, "code", "1.9.0-1")
	f.rec.st.AddSystem("apt", "code")
	found, err = f.apt.Inspect(ctx, debFixtureCode, want, f.rec)
	notes := strings.Join(found.Notes, " ")
	if err != nil || found.Status != engine.Satisfied || found.Pending || !strings.Contains(notes, "1.9.0-1") || !strings.Contains(notes, debFixtureCodeVersion) || !strings.Contains(notes, "does not downgrade") {
		t.Fatal("a newer installed version must stay, with the installed and the pinned version in the report", found, err)
	}
	if err := f.apt.Apply(ctx, debFixtureCode, found, f.rec); err != nil || len(f.commands) != 0 || f.downloads != 0 {
		t.Fatal("setup ran a command for a declined downgrade", err, f.commands)
	}
	f.verify["code"] = "??5??????   /usr/share/code/code\n"
	found, err = f.apt.Inspect(ctx, debFixtureCode, want, f.rec)
	if err != nil || found.Status != engine.Blocked || !strings.Contains(found.Reason, "does not downgrade") || len(found.Commands) != 0 {
		t.Fatal("a repair that needs a downgrade must be declined", found, err)
	}
	if err := f.apt.Apply(ctx, debFixtureCode, found, f.rec); err == nil || len(f.commands) != 0 {
		t.Fatal(err, f.commands)
	}
}

func TestCodeCheckAcceptsOnlyTheModesThatThePackageDeclares(t *testing.T) {
	const older = "1.0.0-1"
	const cache = debFixtureCodeCache + "/main/v24.22.1-x64-7034790f/0a1b2c3d"
	for name, c := range map[string]struct {
		version, verify, sandbox, found string
		healthy                         bool
	}{
		"pinned DEB as shipped":                {verify: "?????????   " + cache + "\n", sandbox: "4755", healthy: true},
		"pinned sandbox lost its setuid bit":   {sandbox: "755"},
		"pinned sandbox is group-writable":     {sandbox: "4775"},
		"pinned cache file became readable":    {sandbox: "4755", found: cache},
		"pinned unreadable file outside cache": {verify: "?????????   /usr/share/code/resources/app/out/main.js\n", sandbox: "4755"},
		"pinned sandbox digest changed":        {verify: "??5??????   /usr/share/code/chrome-sandbox\n", sandbox: "4755"},
		"older DEB as shipped":                 {version: older, sandbox: "755", healthy: true},
		"older sandbox gained a setuid bit":    {version: older, sandbox: "4755"},
		"older DEB with an unreadable file":    {version: older, verify: "?????????   " + cache + "\n", sandbox: "755"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDebFixture(t)
			version := debFixtureCodeVersion
			if c.version != "" {
				version = c.version
			}
			aptFixtureInstalled(f.aptFixture, "code", version)
			f.verify["code"] = c.verify
			bin := t.TempDir()
			for program, text := range map[string]string{
				"readlink": "#!/bin/sh\nprintf '/usr/share/code/bin/code\\n'\n",
				"stat":     "#!/bin/sh\ncase \"$2 $3\" in\n'%a /usr/share/code/chrome-sandbox') printf '%s\\n' \"$SANDBOX\";;\n'%u '*) printf '0\\n';;\n'%a '*) printf '755\\n';;\n*) exit 1;;\nesac\n",
				"find":     "#!/bin/sh\n[ \"$1\" = " + debFixtureCodeCache + " ] || exit 1\n[ -z \"$FOUND\" ] || printf '%s\\n' \"$FOUND\"\n",
			} {
				if err := os.WriteFile(filepath.Join(bin, program), []byte(text), 0755); err != nil {
					t.Fatal(err)
				}
			}
			f.health = func(args []string) ([]byte, error) {
				cmd := exec.Command("/bin/sh", args...)
				cmd.Env = []string{"PATH=" + bin, "SANDBOX=" + c.sandbox, "FOUND=" + c.found}
				return cmd.CombinedOutput()
			}
			found, err := f.apt.Inspect(context.Background(), debFixtureCode, engine.Want{Present: true}, f.rec)
			if err != nil || (found.Status == engine.Satisfied) != c.healthy || !c.healthy && found.Status != engine.Blocked {
				t.Fatal("Code health differs from the modes that the package declares", found, err)
			}
		})
	}
}

func TestCodeHealthReportsExactNativeMismatch(t *testing.T) {
	for _, mismatch := range []string{"mode", "parent", "owner", "link", "healthy"} {
		t.Run(mismatch, func(t *testing.T) {
			bin := t.TempDir()
			for name, text := range map[string]string{
				"readlink": "#!/bin/sh\nif [ \"$MISMATCH\" = link ]; then printf '/personal/code\\n'; else printf '/usr/share/code/bin/code\\n'; fi\n",
				"stat":     "#!/bin/sh\ncase \"$2\" in\n%u) if [ \"$MISMATCH:$3\" = owner:/usr/share/code ]; then printf '1001\\n'; else printf '0\\n'; fi;;\n%a) if [ \"$MISMATCH:$3\" = mode:/usr/share/code ]; then printf '775\\n'; elif [ \"$MISMATCH:$3\" = parent:/usr/share ]; then printf '777\\n'; else printf '755\\n'; fi;;\nesac\n",
			} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(text), 0755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("/bin/sh", "-c", debCodeHealth, "sh", "755", "")
			cmd.Env = []string{"PATH=" + bin, "MISMATCH=" + mismatch}
			out, err := cmd.CombinedOutput()
			if mismatch == "healthy" {
				if err != nil || len(out) != 0 {
					t.Fatal("healthy native query changed behavior", string(out), err)
				}
				return
			}
			want := map[string]string{"link": "/usr/bin/code target=/personal/code", "parent": "/usr/share owner=0 mode=777",
				"mode": "/usr/share/code owner=0 mode=775", "owner": "/usr/share/code owner=1001 mode=755"}[mismatch]
			if err == nil || !strings.Contains(string(out), want) || len(out) > 1024 {
				t.Fatal("native mismatch lost its bounded path/metadata diagnostic", string(out), err)
			}
		})
	}
}

func TestCodeTemporaryInputPreservesUnexpectedContents(t *testing.T) {
	input, err := newDebCodeInput(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(input.Directory); err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("the input folder must be private", info, err)
	}
	extra := filepath.Join(input.Directory, "personal")
	if err := os.WriteFile(extra, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := input.cleanup(); err == nil {
		t.Fatal("cleanup deleted a folder with an unexpected file")
	}
	if data, err := os.ReadFile(extra); err != nil || string(data) != "keep" {
		t.Fatal("the unexpected file was not preserved", err)
	}
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	answer := filepath.Join(input.Directory, "answer.dat")
	changed := strings.Replace(debCodeDatabase, "false", "true ", 1)
	if err := os.WriteFile(answer, []byte(changed), 0644); err != nil {
		t.Fatal(err)
	}
	if err := input.cleanup(); err == nil {
		t.Fatal("cleanup deleted a changed database")
	}
	if err := os.WriteFile(answer, []byte(debCodeDatabase), 0644); err != nil {
		t.Fatal(err)
	}
	if err := input.cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(input.Directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cleanup left the verified input", err)
	}
	if err := input.cleanup(); err != nil {
		t.Fatal("cleanup of an absent input must be a success", err)
	}
	if _, err := newDebCodeInput(filepath.Join(t.TempDir(), "with space")); err == nil {
		t.Fatal("debconf cannot name a database path with a space")
	}
}

func TestTailscalePinnedPackageChecks(t *testing.T) {
	ctx := context.Background()
	want := engine.Want{Present: true}

	f := newDebFixture(t)
	f.collisions = "/usr/sbin/tailscaled\n"
	found, err := f.apt.Inspect(ctx, debFixtureTailscale, want, f.rec)
	if err != nil || found.Status != engine.Blocked || !strings.Contains(found.Reason, "tailscale has a conflicting or changed native publication") {
		t.Fatal(found, err)
	}

	f = newDebFixture(t)
	found, err = f.apt.Inspect(ctx, debFixtureTailscale, want, f.rec)
	if err != nil || found.Status != engine.Missing || found.Reason != "install reviewed tailscale pin 1.2.3" {
		t.Fatal(found, err)
	}
	if err := f.apt.Apply(ctx, debFixtureTailscale, found, f.rec); err != nil {
		t.Fatal(err)
	}
	got := f.installArguments()
	if len(got) != 1 || !strings.HasSuffix(got[0][len(got[0])-1], "/tailscale.deb") || slices.Contains(got[0], "--allow-downgrades") || strings.Contains(strings.Join(got[0], " "), "DEBCONF") {
		t.Fatal(got)
	}
	if f.installed["tailscale:amd64"].Version != "1.2.3" || !f.rec.State().HasSystem("apt", "tailscale") {
		t.Fatal(f.installed, f.rec.State().System)
	}
	f.verify["tailscale"] = "??5?????? c /etc/default/tailscaled\n"
	if found, err := f.apt.Inspect(ctx, debFixtureTailscale, want, f.rec); err != nil || found.Status != engine.Satisfied {
		t.Fatal("a changed configuration file of the administrator is not a health issue", found, err)
	}
	f.verify["tailscale"] = "??5??????   /usr/sbin/tailscaled\n"
	if found, err := f.apt.Inspect(ctx, debFixtureTailscale, want, f.rec); err != nil || found.Status != engine.Unhealthy || !strings.Contains(found.Reason, "changed package files") {
		t.Fatal(found, err)
	}

	f = newDebFixture(t)
	f.payload = []byte("replaced package bytes")
	found, _ = f.apt.Inspect(ctx, debFixtureTailscale, want, f.rec)
	if err := f.apt.Apply(ctx, debFixtureTailscale, found, f.rec); err == nil || !strings.Contains(err.Error(), "SHA-256") && !strings.Contains(err.Error(), "damaged") || len(f.commands) != 0 || len(f.rec.State().Pending) != 0 {
		t.Fatal("a package file that differs from its pin reached APT", err, f.commands)
	}

	f = newDebFixture(t)
	pin := f.apt.Pins.LinuxPackages["tool.tailscale"]["amd64"]
	pin.URL = "https://example.com/tailscale_1.2.3_amd64.deb"
	f.apt.Pins.LinuxPackages["tool.tailscale"]["amd64"] = pin
	if _, err := f.apt.Inspect(ctx, debFixtureTailscale, want, f.rec); err == nil || !strings.Contains(err.Error(), "official release URL") {
		t.Fatal("accepted a pin outside the official release URL", err)
	}
	delete(f.apt.Pins.LinuxPackages, "tool.tailscale")
	if _, err := f.apt.Inspect(ctx, debFixtureTailscale, want, f.rec); err == nil || !strings.Contains(err.Error(), "lacks its reviewed native package") {
		t.Fatal("accepted Tailscale without its pin", err)
	}
	other := aptFixtureResource("code")
	if _, err := f.apt.Inspect(ctx, other, want, f.rec); err == nil || !strings.Contains(err.Error(), "unexpected resource") {
		t.Fatal("accepted the Code package on another resource", err)
	}
}

func TestCodeShownCommandsAreTheCommandsThatRunWithTheDebconfInput(t *testing.T) {
	ctx := context.Background()
	f := newDebFixture(t)
	found, err := f.apt.Inspect(ctx, debFixtureCode, engine.Want{Present: true}, f.rec)
	if err != nil || len(found.Commands) != 1 {
		t.Fatal(found, err)
	}
	shown := aptFixtureShellLines(found.Commands[0])
	if len(shown) != 2 || !strings.Contains(shown[1], "DEBCONF_DB_OVERRIDE='File{filename:") || !strings.Contains(shown[1], "DEBIAN_FRONTEND=noninteractive") {
		t.Fatal("the shown Code install must carry the debconf override that setup uses", shown)
	}
	answers := ""
	for _, line := range found.Commands[0].Lines {
		if strings.HasPrefix(line, "printf ") {
			answers = line
		}
	}
	database := ""
	f.during = func(c platform.Command) {
		if slices.Contains(c.Args, "install") {
			database = strings.TrimSuffix(strings.TrimPrefix(strings.Fields(c.Args[3])[0], "DEBCONF_DB_OVERRIDE=File{filename:"), "")
		}
	}
	if err := f.apt.Apply(ctx, debFixtureCode, found, f.rec); err != nil {
		t.Fatal(err)
	}
	ran := []string{}
	for _, c := range f.commands {
		ran = append(ran, aptFixtureLine(c))
	}
	if !slices.Equal(ran, shown) {
		t.Fatal("the shown lines differ from the commands that ran", ran, shown)
	}
	if database == "" || !strings.Contains(answers, database) || !strings.Contains(answers, "code/add-microsoft-repo") || !strings.Contains(answers, "code/add-copilot-command") || strings.Count(answers, "Value: false") != 2 {
		t.Fatal("the block must say where setup writes the debconf answers and what they are", answers, database)
	}
}

func TestShownEnvironmentAndPreparationLinesRunInAShell(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "dotfiles-code-debconf-1")
	override := debCodeOverride(directory)
	for name, c := range map[string]platform.Command{
		"root form": {Path: "/usr/bin/env", Env: []string{"LC_ALL=C", override}},
		"sudo form": {Path: "/usr/bin/env", Args: []string{"LC_ALL=C", override, "/usr/bin/env"}},
	} {
		output, err := exec.Command("/bin/sh", "-c", aptShellLine(c)).CombinedOutput()
		if err != nil || !slices.Contains(strings.Split(string(output), "\n"), override) {
			t.Fatal(name, "the pasted line did not deliver the debconf override", aptShellLine(c), string(output), err)
		}
	}

	f := newDebFixture(t)
	found, err := f.apt.Inspect(context.Background(), debFixtureCode, engine.Want{Present: true}, f.rec)
	if err != nil || len(found.Commands) != 1 {
		t.Fatal(found, err)
	}
	ran, curl, check := 0, "", ""
	for _, line := range found.Commands[0].Lines {
		switch {
		case strings.HasPrefix(line, "curl "):
			curl = line
		case strings.Contains(line, "sha256sum -c"):
			check = line
		case strings.HasPrefix(line, "mkdir ") || strings.HasPrefix(line, "printf "):
			if output, err := exec.Command("/bin/sh", "-c", line).CombinedOutput(); err != nil {
				t.Fatal("a shown preparation line failed in a shell", line, string(output), err)
			}
			ran++
		}
	}
	pin := f.apt.Pins.LinuxCode["amd64"]
	file := filepath.Join(f.apt.Directory, "payloads", pin.SHA256, "code.deb")
	if ran != 3 || curl != "curl -fLo "+file+" "+pin.URL || check != "echo '"+pin.SHA256+"  "+file+"' | sha256sum -c" {
		t.Fatal("the block must show executable preparation steps", found.Commands[0].Lines)
	}
	answers := debCodeAnswers(debCodeDirectory(f.apt.Temporary))
	if data, err := os.ReadFile(answers); err != nil || string(data) != debCodeDatabase {
		t.Fatal("the shown printf must write the debconf answers that setup uses", string(data), err)
	}
	if info, err := os.Stat(filepath.Dir(file)); err != nil || !info.IsDir() {
		t.Fatal("the shown mkdir must make the folder of the download", err)
	}
	if err := os.RemoveAll(filepath.Join(f.apt.Directory, "payloads")); err != nil {
		t.Fatal(err)
	}
	if err := f.apt.Apply(context.Background(), debFixtureCode, found, f.rec); err != nil {
		t.Fatal("setup must replace the answers folder that the pasted block wrote", err)
	}
}
