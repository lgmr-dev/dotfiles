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
	"regexp"
	"time"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

var homebrewBootstrapSource = regexp.MustCompile(`^https://raw\.githubusercontent\.com/Homebrew/install/[a-f0-9]{40}/install\.sh$`)

var homebrewSHA256 = regexp.MustCompile(`^[a-f0-9]{64}$`)

const homebrewBootstrapLimit = 512 << 10

func (d *HomebrewBootstrap) environment() []string {
	values := []string{"-i", "HOME=" + d.Home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "SHELL=/bin/zsh",
		"NONINTERACTIVE=1", "SUDO_ASKPASS=/usr/bin/false", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"HOMEBREW_BREW_GIT_REMOTE=https://github.com/Homebrew/brew", "HOMEBREW_CORE_GIT_REMOTE=https://github.com/Homebrew/homebrew-core"}
	return append(values, brewProcessControls...)
}

func (d *HomebrewBootstrap) reserveCommand(prefix string) platform.Command {
	return platform.Command{Path: "/usr/bin/sudo", Args: []string{"-n", "/bin/mkdir", prefix}, Elevated: true}
}

func (d *HomebrewBootstrap) installCommand(prefix string, payload []byte) platform.Command {
	return platform.Command{Path: "/usr/bin/env", Stdin: bytes.NewReader(payload),
		Args: append(d.environment(), "/bin/bash", "--noprofile", "--norc", "-s", "--", "--path="+prefix)}
}

func (d *HomebrewBootstrap) query(location HomebrewLocation) MacQuery {
	if d.Query != nil {
		return d.Query
	}
	return func(ctx context.Context, program string, args ...string) ([]byte, error) {
		if program == "brew" {
			args = append(append(d.environment(), location.Program), args...)
			program = "/usr/bin/env"
		}
		return location.Query(ctx, program, args...)
	}
}

func (d *HomebrewBootstrap) payload(ctx context.Context) ([]byte, error) {
	if !homebrewBootstrapSource.MatchString(d.url) || !homebrewSHA256.MatchString(d.sha256) {
		return nil, errors.New("Homebrew bootstrap source is not a pinned official installer")
	}
	client := d.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	safe := *client
	safe.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("Homebrew bootstrap does not accept source redirects")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url, nil)
	if err != nil {
		return nil, err
	}
	response, err := safe.Do(request)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, homebrewBootstrapLimit+1))
	if err := errors.Join(readErr, response.Body.Close()); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if response.StatusCode != http.StatusOK || len(data) == 0 || len(data) > homebrewBootstrapLimit || hex.EncodeToString(sum[:]) != d.sha256 {
		return nil, errors.New("Homebrew bootstrap download failed its status, size or pinned checksum check")
	}
	return data, nil
}

func homebrewPrefixIdentity(prefix string) (identity string, result error) {
	before, err := os.Lstat(prefix)
	if err != nil {
		return "", err
	}
	if !before.IsDir() {
		return "", errors.New("the Homebrew prefix is not a real folder; preserve it")
	}
	file, err := os.Open(prefix)
	if err != nil {
		return "", err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", errors.Join(errors.New("the Homebrew prefix changed while opening"), err)
	}
	return homebrewDirectoryIdentity(file)
}
