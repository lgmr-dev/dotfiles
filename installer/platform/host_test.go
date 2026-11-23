package platform_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

func TestUnsupportedTargetIsRefused(t *testing.T) {
	for _, target := range []platform.Context{{OS: "darwin", Arch: "amd64"}, {OS: "windows", Arch: "arm64"}, {OS: "linux", Arch: "386"}, {OS: "plan9", Arch: "amd64"}} {
		if err := target.Validate(); err == nil {
			t.Error("accepted an unsupported target", target)
		}
	}
	for _, target := range []platform.Context{{OS: "darwin", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "windows", Arch: "amd64"}} {
		if err := target.Validate(); err != nil {
			t.Error("refused a supported target", target, err)
		}
	}
}

func TestHostDiscovery(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "token-fixture")
	host, err := platform.DiscoverHost()
	if err != nil {
		t.Fatal(err)
	}
	if host.OS != runtime.GOOS || host.Arch != runtime.GOARCH || host.GitHubToken != "token-fixture" || len(host.SystemCommandDirectories) == 0 {
		t.Fatal("native host differs", host)
	}
	if host.OS == "linux" && (host.Distro == "" || host.Libc == "") {
		t.Fatal("a native Linux host must have an observed distribution and libc", host)
	}
}

func TestLinuxSupportRejectsUnselectedDistributions(t *testing.T) {
	for _, data := range []string{
		"ID=fedora\n", "ID=arch\n", "ID=opensuse-tumbleweed\n", "ID=alpine\n",
		"ID=linuxmint\nID_LIKE=\"ubuntu debian\"\n", "ID=unknown\nID_LIKE=debian\n",
		"ID=ubuntu\nID=fedora\n", "NAME=Linux\n",
	} {
		if _, _, _, err := platform.LinuxDistribution([]byte(data)); err == nil {
			t.Errorf("unsupported distribution accepted: %q", data)
		}
	}
}

func TestLinuxDistributionUsesExactSupportedIdentity(t *testing.T) {
	for _, fixture := range []struct{ data, id, release, family string }{
		{"ID=ubuntu\nID_LIKE=debian\n", "ubuntu", "", "apt"},
		{"ID='debian'\n", "debian", "", "apt"},
		{"ID=fedora\nID=ubuntu\n", "ubuntu", "", "apt"},
		{"ID=ubuntu\nID_LIKE=\"fedora\"\n", "ubuntu", "", "apt"},
		{"ID=debian\nNAME=\"$(do not execute)\"\n", "debian", "", "apt"},
		{"ID=ubuntu\nVERSION_ID=\"24.04\"\n", "ubuntu", "24.04", "apt"},
		{"VERSION_ID=12\nID=debian\nVERSION_ID=13\n", "debian", "13", "apt"},
		{"ID=ubuntu\nVERSION_ID=24.04-lts\n", "ubuntu", "", "apt"},
		{"ID=ubuntu\nVERSION_ID=24.04\nVERSION_ID=\n", "ubuntu", "", "apt"},
	} {
		id, release, family, err := platform.LinuxDistribution([]byte(fixture.data))
		if err != nil || id != fixture.id || release != fixture.release || family != fixture.family {
			t.Fatal(fixture, id, release, family, err)
		}
	}
	for _, invalid := range []string{"ID=\"ubuntu", "ID=ubuntu debian", "ID=$(uname)", "ID=", "ID_LIKE=\"debian;touch /tmp/file\""} {
		if _, _, _, err := platform.LinuxDistribution([]byte(invalid)); err == nil {
			t.Fatal("accepted malformed policy input", invalid)
		}
	}
}

func TestInterpreterNamesTheLibc(t *testing.T) {
	for path, want := range map[string]string{"/lib/ld-musl-x86_64.so.1": "musl", "/lib64/ld-linux-x86-64.so.2": "glibc", "/lib/ld-linux-aarch64.so.1": "glibc", "/lib/other.so": ""} {
		if got := platform.InterpreterLibc(path); got != want {
			t.Error(path, got, want)
		}
	}
}

func TestStateDirectoryDiscovery(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("XDG_STATE_HOME", "")
	folders := platform.Folders{Home: home, LocalAppData: filepath.Join(home, "AppData", "Local")}
	directory, err := platform.DiscoverStateDirectory(platform.Context{OS: "linux", Arch: "amd64"}, folders)
	if err != nil || directory != filepath.Join(home, ".local", "state", "dotfiles") {
		t.Fatal(directory, err)
	}
	directory, err = platform.DiscoverStateDirectory(platform.Context{OS: "windows", Arch: "amd64"}, folders)
	if err != nil || directory != filepath.Join(folders.LocalAppData, "dotfiles") {
		t.Fatal(directory, err)
	}
	state := filepath.Join(home, "state")
	t.Setenv("XDG_STATE_HOME", state)
	directory, err = platform.DiscoverStateDirectory(platform.Context{OS: "darwin", Arch: "arm64"}, folders)
	if err != nil || directory != filepath.Join(state, "dotfiles") {
		t.Fatal(directory, err)
	}
	t.Setenv("XDG_STATE_HOME", "relative/state")
	if _, err := platform.DiscoverStateDirectory(platform.Context{OS: "linux", Arch: "amd64"}, folders); err == nil {
		t.Fatal("relative state folder accepted")
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("discovery created a folder", err)
	}
}
