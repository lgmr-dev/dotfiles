package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

// HomebrewLocation holds the Homebrew executable and the prefix and Cellar
// that it reports. Discovery observes them at the process boundary and never
// derives them from the architecture or the home folder.
type HomebrewLocation struct {
	Program, Prefix, Cellar string
}

// Validate accepts only clean absolute POSIX paths.
func (location HomebrewLocation) Validate() error {
	for _, path := range []string{location.Program, location.Prefix, location.Cellar} {
		if !macosCleanPath(path) || strings.Contains(path, ":") {
			return errors.New("Homebrew paths must be canonical absolute POSIX paths")
		}
	}
	return nil
}

// CommandDirectory returns the folder of the commands that Homebrew links.
func (location HomebrewLocation) CommandDirectory() string {
	return filepath.Join(location.Prefix, "bin")
}

// DiscoverHomebrew finds Homebrew on PATH, or at /opt/homebrew when the
// shell initialization did not add it to PATH yet, and reads its prefix and
// Cellar. It returns nil when there is no Homebrew.
func DiscoverHomebrew(ctx context.Context) (*HomebrewLocation, error) {
	return discoverHomebrew(ctx, "/opt/homebrew/bin/brew")
}

func discoverHomebrew(ctx context.Context, bootstrapProgram string) (*HomebrewLocation, error) {
	program, err := exec.LookPath("brew")
	if errors.Is(err, exec.ErrNotFound) {
		if _, err := os.Lstat(bootstrapProgram); errors.Is(err, os.ErrNotExist) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		return inspectHomebrew(ctx, bootstrapProgram)
	}
	if err != nil {
		return nil, err
	}
	return inspectHomebrew(ctx, program)
}

func inspectHomebrew(ctx context.Context, program string) (*HomebrewLocation, error) {
	location := &HomebrewLocation{Program: program}
	for option, destination := range map[string]*string{"--prefix": &location.Prefix, "--cellar": &location.Cellar} {
		data, err := location.Query(ctx, "brew", option)
		if err != nil {
			return nil, fmt.Errorf("discover Homebrew %s: %w", option, err)
		}
		*destination = strings.TrimSuffix(string(data), "\n")
	}
	if err := location.Validate(); err != nil {
		return nil, err
	}
	return location, nil
}

// Query is the MacQuery of a Homebrew installation. The program "brew" is
// the Homebrew executable. Each command gets the Homebrew process controls,
// and brew gets an empty HOMEBREW_CASK_OPTS. The limit is 30 seconds, and 5
// minutes for `brew linkage`.
func (location HomebrewLocation) Query(ctx context.Context, program string, args ...string) ([]byte, error) {
	brew := program == "brew"
	if brew {
		program = location.Program
	}
	limit := 30 * time.Second
	environment := append(os.Environ(), brewProcessControls...)
	if brew {
		environment = append(environment, "HOMEBREW_CASK_OPTS=")
	}
	if brew && len(args) > 0 && args[0] == "linkage" {
		limit = 5 * time.Minute
		environment = append(environment, "HOMEBREW_DEV_CMD_RUN=1")
	}
	stdout, stderr, err := macosCapture(ctx, limit, 8<<20, environment, program, args...)
	if err != nil {
		return stdout, fmt.Errorf("native inspection failed: %w%s", err, platform.Diagnostic(stderr))
	}
	return stdout, nil
}

// NixHomebrewIssue returns a reason when nix-homebrew manages the Homebrew
// prefix, or when setup cannot inspect that. Setup never uses such a prefix
// as its package infrastructure. It returns "" for an ordinary prefix.
func NixHomebrewIssue(location HomebrewLocation) string {
	issue := "Homebrew at " + location.Prefix + " is managed by nix-homebrew; run migrate from a v1.0.1 checkout to convert it before setup uses Homebrew"
	for _, path := range []string{filepath.Join(location.Prefix, ".managed_by_nix_darwin"), filepath.Join(location.Prefix, "Library", ".homebrew-is-managed-by-nix")} {
		if _, err := os.Lstat(path); err == nil {
			return issue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "Homebrew ownership could not be inspected: " + err.Error()
		}
	}
	for _, path := range []string{location.Program, filepath.Join(location.Prefix, "Library", "Homebrew")} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "Homebrew ownership could not be inspected: " + err.Error()
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(path)
		if err != nil {
			return "Homebrew ownership could not be inspected: " + err.Error()
		}
		if strings.HasPrefix(target, "/nix/store/") {
			return issue
		}
	}
	return ""
}

func homebrewLocate(ctx context.Context, discover func(context.Context) (*HomebrewLocation, error)) (*HomebrewLocation, string, error) {
	location, err := discover(ctx)
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	if err != nil {
		return nil, err.Error(), nil
	}
	if location == nil {
		return nil, "", nil
	}
	if err := location.Validate(); err != nil {
		return nil, err.Error(), nil
	}
	return location, NixHomebrewIssue(*location), nil
}
