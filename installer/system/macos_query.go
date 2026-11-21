package system

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

// MacQuery runs one read-only inspection command and returns its standard
// output. It never elevates and gives the command no input. A Homebrew
// provider accepts the program "brew" for the discovered Homebrew executable.
type MacQuery func(ctx context.Context, program string, args ...string) ([]byte, error)

type macosBoundedOutput struct {
	data  bytes.Buffer
	limit int
}

func (output *macosBoundedOutput) Write(data []byte) (int, error) {
	remaining := output.limit - output.data.Len()
	if len(data) > remaining {
		n, err := output.data.Write(data[:remaining])
		return n, errors.Join(fmt.Errorf("native command output exceeds %d bytes", output.limit), err)
	}
	return output.data.Write(data)
}

func macosCapture(ctx context.Context, limit time.Duration, outputLimit int, environment []string, program string, args ...string) (stdout, stderr []byte, err error) {
	if !filepath.IsAbs(program) || filepath.Clean(program) != program {
		return nil, nil, errors.New("native inspection requires an absolute executable")
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	command.Env = environment
	output, diagnostic := macosBoundedOutput{limit: outputLimit}, macosBoundedOutput{limit: 1 << 20}
	command.Stdout, command.Stderr = &output, &diagnostic
	if err := command.Run(); err != nil {
		return output.data.Bytes(), diagnostic.data.Bytes(), errors.Join(err, ctx.Err())
	}
	return output.data.Bytes(), diagnostic.data.Bytes(), nil
}

var errMacNoDeveloperSelection = errors.New("Apple developer directory is not selected")

// MacPrerequisiteQuery is the MacQuery of the Apple developer tools and the
// clipboard commands. It removes DEVELOPER_DIR, SDKROOT and TOOLCHAINS from
// the environment, so xcrun reads the directory that xcode-select names.
func MacPrerequisiteQuery(ctx context.Context, program string, args ...string) ([]byte, error) {
	environment := []string{}
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if key != "DEVELOPER_DIR" && key != "SDKROOT" && key != "TOOLCHAINS" && key != "LC_ALL" {
			environment = append(environment, value)
		}
	}
	stdout, stderr, err := macosCapture(ctx, 30*time.Second, 64<<10, append(environment, "LC_ALL=C"), program, args...)
	if err != nil {
		if program == "/usr/bin/xcode-select" && len(args) == 1 && args[0] == "--print-path" && appleCLTSelectionMissing(err, stdout, stderr) {
			return nil, fmt.Errorf("%w: %w", errMacNoDeveloperSelection, err)
		}
		return nil, fmt.Errorf("Apple prerequisite inspection failed: %w%s", err, platform.Diagnostic(stderr))
	}
	return stdout, nil
}

func macosDigest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func macosRun(ctx context.Context, runner platform.Runner, c platform.Command) (output []byte, started bool, err error) {
	if runner == nil {
		return nil, false, errors.New("the provider has no command runner")
	}
	output, err = runner.Run(ctx, c)
	if err == nil {
		return output, true, nil
	}
	_, started = platform.ExitCode(err)
	return output, started, err
}

var macosPlainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func macosShellWord(word string) string {
	if macosPlainWord.MatchString(word) {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'"'"'`) + "'"
}

func macosShellLine(c platform.Command) string {
	words := []string{}
	for _, assignment := range c.Env {
		key, value, _ := strings.Cut(assignment, "=")
		if value != "" {
			value = macosShellWord(value)
		}
		words = append(words, key+"="+value)
	}
	words = append(words, macosShellWord(c.Path))
	for _, argument := range c.Args {
		words = append(words, macosShellWord(argument))
	}
	return strings.Join(words, " ")
}

func macosClearMark(rec engine.Records, mark string) error {
	return rec.Update(func(s *state.State) error { s.Clear(mark); return nil })
}

func macosCleanPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n")
}
