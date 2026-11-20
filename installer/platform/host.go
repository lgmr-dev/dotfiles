package platform

import (
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Host holds the observations of the native machine. It is not saved target
// identity. Provider locations and their issues stay with their providers.
type Host struct {
	Context
	Distro                   string
	DistroRelease            string
	Family                   string
	Libc                     string
	SystemCommandDirectories []string
	GitHubToken              string
}

// DiscoverHost observes the native machine. It runs no shell input and
// installs nothing. A Linux guest uses the same discovery as other Linux hosts.
func DiscoverHost() (Host, error) {
	host := Host{Context: Context{OS: runtime.GOOS, Arch: runtime.GOARCH}}
	if err := host.Context.Validate(); err != nil {
		return host, err
	}
	directories, err := systemCommandDirectories()
	if err != nil {
		return host, err
	}
	host.SystemCommandDirectories = directories
	host.GitHubToken = os.Getenv("GITHUB_TOKEN")
	if host.OS != "linux" {
		return host, nil
	}
	data, err := readPlatformFile("/etc/os-release")
	if errors.Is(err, os.ErrNotExist) {
		data, err = readPlatformFile("/usr/lib/os-release")
	}
	if err != nil {
		return host, fmt.Errorf("read Linux distribution identity: %w", err)
	}
	host.Distro, host.DistroRelease, host.Family, err = linuxDistribution(data)
	if err != nil {
		return host, err
	}
	host.Libc, err = linuxLibc("/bin/sh")
	if err != nil {
		return host, err
	}
	return host, nil
}

func readPlatformFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, 65537))
	if err := errors.Join(readErr, f.Close()); err != nil {
		return nil, err
	}
	if len(data) > 65536 || bytes.ContainsRune(data, 0) {
		return nil, fmt.Errorf("invalid or oversized platform identity at %s", path)
	}
	return data, nil
}

var distroID = regexp.MustCompile(`^[a-z0-9._-]+$`)

var distroRelease = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)

// https://www.freedesktop.org/software/systemd/man/latest/os-release.html
func linuxDistribution(data []byte) (string, string, string, error) {
	id, release := "linux", ""
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key != "ID" && key != "VERSION_ID" {
			continue
		}
		if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if key == "VERSION_ID" {
			release = value
			continue
		}
		if !distroID.MatchString(value) {
			return "", "", "", errors.New("invalid distribution ID")
		}
		id = value
	}
	if id != "ubuntu" && id != "debian" {
		return id, "", "", fmt.Errorf("unsupported Linux distribution %q; this release supports Ubuntu and Debian only", id)
	}
	if !distroRelease.MatchString(release) {
		release = ""
	}
	return id, release, "apt", nil
}

func linuxLibc(shell string) (libc string, result error) {
	f, err := elf.Open(shell)
	if err != nil {
		return "", fmt.Errorf("inspect system shell ELF loader: %w", err)
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	for _, program := range f.Progs {
		if program.Type != elf.PT_INTERP {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(program.Open(), 4097))
		if err != nil || len(data) > 4096 || len(data) < 2 || data[len(data)-1] != 0 {
			return "", errors.Join(errors.New("invalid system ELF interpreter"), err)
		}
		return interpreterLibc(string(data[:len(data)-1])), nil
	}
	return "", nil
}

func interpreterLibc(path string) string {
	name := filepath.Base(path)
	if strings.HasPrefix(name, "ld-musl-") {
		return "musl"
	}
	if strings.HasPrefix(name, "ld-linux-") {
		return "glibc"
	}
	return ""
}
