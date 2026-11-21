package builds

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

func pipConfigNullDevice() string { return strings.ToLower(os.DevNull) }

type privatePythonRequirement struct {
	Name, File, Hash string
	Source           bool
}

func privatePythonConsolePaths(pin catalog.ArchivePin, name string) (string, string, error) {
	target := pin.Commands[name]
	source := ""
	switch target {
	case "bin/" + name, "commands/" + name:
		source = "bin/" + name
	case "Scripts/" + name + ".exe", "commands/" + name + ".exe":
		source = "Scripts/" + name + ".exe"
	default:
		return "", "", fmt.Errorf("%s requires its fixed private console command", name)
	}
	if len(pin.Commands) != 1 || !slices.Equal(pin.BinDirs, []string{path.Dir(target)}) {
		return "", "", fmt.Errorf("%s has an unexpected command layout", name)
	}
	return source, target, nil
}

func (p *Provider) basePython(uses []string) (string, error) {
	pin, ok := p.Pins[pythonResource]
	if !ok || len(uses) != 1 {
		return "", errors.New("a Python tool requires the private Python build; select Python and run setup again")
	}
	command := pin.Commands["python3"]
	if runtime.GOOS == "windows" || command == "" {
		command = pin.Commands["python"]
	}
	if command == "" {
		return "", errors.New("the Python pin declares no interpreter command")
	}
	return filepath.Join(uses[0], filepath.FromSlash(command)), nil
}

func (p *Provider) preparePrivatePython(ctx context.Context, pin catalog.ArchivePin, payload string, uses []string, name string, requirements []privatePythonRequirement, probe string, probeArguments ...string) error {
	base, err := p.basePython(uses)
	if err != nil {
		return err
	}
	if strings.ContainsAny(base, "\x00\r\n") || p.Runner == nil {
		return fmt.Errorf("%s requires the private Python command and a command runner", name)
	}
	home := filepath.Join(payload, ".build-home")
	temporary := os.TempDir()
	if err := os.Mkdir(home, 0700); err != nil {
		return err
	}
	environment := []string{"HOME=" + home, "USERPROFILE=" + home, "APPDATA=" + home, "LOCALAPPDATA=" + home, "TMPDIR=" + temporary, "TEMP=" + temporary, "TMP=" + temporary, "PIP_CONFIG_FILE=" + pipConfigNullDevice(), "PYTHONPATH=", "PYTHONHOME=", "PYTHONDONTWRITEBYTECODE=1"}
	phase := 0
	run := func(program string, args ...string) error {
		phase++
		if err := p.run(ctx, program, environment, args...); err != nil {
			return fmt.Errorf("%s preparation phase %d: %w", name, phase, err)
		}
		return nil
	}
	target := payload
	if runtime.GOOS == "windows" {
		base, target = platform.WindowsExtendedPath(base), platform.WindowsExtendedPath(target)
	}
	if err := run(base, "-I", "-B", "-m", "venv", target); err != nil {
		return err
	}
	python := filepath.Join(payload, "bin", "python")
	if runtime.GOOS == "windows" {
		python = platform.WindowsExtendedPath(filepath.Join(payload, "Scripts", "python.exe"))
	}
	for _, requirement := range requirements {
		if !resourceID.MatchString(requirement.Name) || !filepath.IsAbs(requirement.File) || !sha256Text.MatchString(requirement.Hash) {
			return errors.New("invalid fixed private Python requirement")
		}
		file := requirement.File
		if runtime.GOOS == "windows" {
			file = platform.WindowsExtendedPath(file)
		}
		file = filepath.ToSlash(file)
		if !strings.HasPrefix(file, "/") {
			file = "/" + file
		}
		local := &url.URL{Scheme: "file", Path: file}
		if runtime.GOOS == "windows" {
			local.RawPath = strings.ReplaceAll(local.EscapedPath(), ":", "%3A")
		}
		requirementFile := filepath.Join(payload, "."+requirement.Name+"-requirements.txt")
		if err := os.WriteFile(requirementFile, []byte(fmt.Sprintf("%s @ %s --hash=sha256:%s\n", requirement.Name, local.String(), requirement.Hash)), 0600); err != nil {
			return err
		}
		if runtime.GOOS == "windows" {
			requirementFile = platform.WindowsExtendedPath(requirementFile)
		}
		args := []string{"-I", "-B", "-m", "pip", "--isolated", "install", "--disable-pip-version-check", "--no-cache-dir", "--no-index", "--require-hashes", "--no-deps"}
		if requirement.Source {
			args = append(args, "--no-binary="+requirement.Name, "--no-build-isolation")
		} else {
			args = append(args, "--only-binary=:all:")
		}
		args = append(args, "-r", requirementFile)
		if err := run(python, args...); err != nil {
			return err
		}
	}
	if name == "latex2text" {
		if err := run(python, "-I", "-B", "-c", latex2textConsole); err != nil {
			return err
		}
	}
	source, console, err := privatePythonConsolePaths(pin, name)
	if err != nil {
		return err
	}
	if source != console {
		if err := os.Mkdir(filepath.Join(payload, "commands"), 0700); err != nil {
			return err
		}
		target := filepath.Join(payload, filepath.FromSlash(console))
		if err := platform.CopyTree(filepath.Join(payload, filepath.FromSlash(source)), target); err != nil {
			return err
		}
		if err := os.Chmod(target, 0755); err != nil {
			return err
		}
	}
	if err := run(python, pythonCompileArguments(payload, payload)...); err != nil {
		return err
	}
	return run(python, append([]string{"-I", "-B", "-c", probe}, probeArguments...)...)
}
