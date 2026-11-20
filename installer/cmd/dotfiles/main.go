package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/luisgui1757/dotfiles/installer/builds"
	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/system"
	"github.com/luisgui1757/dotfiles/installer/terminal"
	"github.com/luisgui1757/dotfiles/installer/tools"
)

const usage = "run setup without arguments for the menu; automation uses plan, apply, update, check or restore-v2 with --yes, --select=<ids>, --remove-shared=<ids> and --replace=<ids>"

const (
	exitDone      = 0
	exitFailed    = 1
	exitRunAgain  = 3
	exitInterrupt = 130
)

type command struct {
	name         string
	yes          bool
	selected     []string
	selectSet    bool
	removeShared []string
	replace      []string
}

type setup struct {
	command      command
	checkout     string
	executable   string
	source       string
	stateDir     string
	target       string
	host         platform.Host
	folders      platform.Folders
	catalog      *catalog.Catalog
	authenticate func(context.Context) error
	output       *os.File

	sudo              string
	apt               *system.APTLocation
	aptIssue          string
	clipboard         system.LinuxClipboard
	dpkgDeb           string
	powershell        string
	buildTools        string
	windowsIssue      string
	homebrew          *system.HomebrewLocation
	homebrewDirectory func() string
	sentinel          hooks.SentinelLocations
}

func main() {
	if handled, code := builds.RunWindowsRustLauncher(); handled {
		os.Exit(code)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	err := run(ctx, os.Args[1:])
	code := exitCode(ctx, err)
	if code == exitInterrupt {
		fmt.Fprintln(os.Stderr, "Dotfiles: interrupted")
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "Dotfiles:", err)
	}
	os.Exit(code)
}

func exitCode(ctx context.Context, err error) int {
	switch {
	case err == nil:
		return exitDone
	case ctx.Err() != nil && errors.Is(err, context.Canceled):
		return exitInterrupt
	case errors.Is(err, engine.ErrReplan), errors.Is(err, engine.ErrPlanChanged):
		return exitRunAgain
	}
	return exitFailed
}

func parseArguments(args []string) (command, error) {
	if len(args) == 0 {
		return command{}, nil
	}
	flags, known := map[string][]string{
		"plan":       {"--select", "--remove-shared", "--replace"},
		"apply":      {"--yes", "--select", "--remove-shared", "--replace"},
		"update":     {"--yes", "--replace"},
		"check":      {},
		"restore-v2": {"--yes"},
	}[args[0]]
	if !known {
		return command{}, fmt.Errorf("unknown command %q; %s", args[0], usage)
	}
	c := command{name: args[0]}
	seen := map[string]bool{}
	for _, argument := range args[1:] {
		name, value, hasValue := strings.Cut(argument, "=")
		if !slices.Contains(flags, name) {
			return command{}, fmt.Errorf("%s does not accept %q; %s", c.name, argument, usage)
		}
		if seen[name] {
			return command{}, fmt.Errorf("%s is given two times", name)
		}
		seen[name] = true
		if name == "--yes" {
			if hasValue {
				return command{}, errors.New("--yes takes no value")
			}
			c.yes = true
			continue
		}
		if !hasValue {
			return command{}, fmt.Errorf("%s needs a list: %s=<id>,<id>", name, name)
		}
		ids := []string{}
		if value != "" {
			ids = strings.Split(value, ",")
		}
		if slices.ContainsFunc(ids, func(id string) bool { return id == "" || strings.TrimSpace(id) != id }) {
			return command{}, fmt.Errorf("%s has an empty or padded ID; separate the IDs with one comma", name)
		}
		switch {
		case name == "--select":
			c.selected, c.selectSet = ids, true
		case len(ids) == 0:
			return command{}, fmt.Errorf("%s needs one ID or more", name)
		case name == "--remove-shared":
			c.removeShared = ids
		default:
			c.replace = ids
		}
	}
	return c, nil
}

func (c command) request() engine.Request {
	mode := c.name
	if mode == "plan" {
		mode = "apply"
	}
	return engine.Request{Mode: mode, Selected: c.selected, SelectSet: c.selectSet, RemoveShared: c.removeShared, Replace: c.replace}
}

func executableHash() (string, string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	file, err := os.Open(executable)
	if err != nil {
		return "", "", err
	}
	hash := sha256.New()
	count, readErr := io.Copy(hash, io.LimitReader(file, 64<<20+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return "", "", err
	}
	if count > 64<<20 {
		return "", "", errors.New("installer executable exceeds its identity bound")
	}
	return executable, hex.EncodeToString(hash.Sum(nil)), nil
}

func run(ctx context.Context, args []string) error {
	c, err := parseArguments(args)
	if err != nil {
		return err
	}
	if entrypoint := os.Getenv("DOTFILES_ENTRYPOINT"); entrypoint != "" && entrypoint != "setup" {
		return errors.New("unknown installer entrypoint; use setup from the checkout")
	}
	repository := os.Getenv("DOTFILES_CHECKOUT")
	if repository == "" {
		return errors.New("launch through the checkout's installer entrypoint; DOTFILES_CHECKOUT is missing")
	}
	checkout, err := filepath.Abs(repository)
	if err != nil {
		return err
	}
	if checkout, err = platform.ResolvePath(checkout); err != nil {
		return fmt.Errorf("DOTFILES_CHECKOUT does not name a readable checkout: %w", err)
	}
	executable, source, err := executableHash()
	if err != nil {
		return err
	}
	host, err := platform.DiscoverHost()
	if err != nil {
		return err
	}
	folders, err := platform.DiscoverFolders()
	if err != nil {
		return err
	}
	stateDir, err := platform.DiscoverStateDirectory(host.Context, folders)
	if err != nil {
		return err
	}
	bound, err := platform.BindDestination(filepath.Join(stateDir, state.File))
	if err != nil {
		return err
	}
	stateDir = filepath.Dir(bound)
	resources, err := catalog.Load(tools.FS)
	if err != nil {
		return err
	}
	s := setup{command: c, checkout: checkout, executable: executable, source: source, stateDir: stateDir, target: host.OS + "/" + host.Arch + ":" + folders.Home,
		host: host, folders: folders, catalog: resources, output: os.Stdout}
	if err := discover(ctx, &s); err != nil {
		return err
	}
	if c.name == "restore-v2" {
		return restoreV2(ctx, s)
	}
	if st, err := state.Peek(stateDir, s.target); errors.Is(err, state.ErrSchema1) || err == nil && st.V2Restore == "switching" {
		return importV2(ctx, s)
	}
	e := newEngine(s)
	if c.name != "" {
		return execute(ctx, e, c, s.output)
	}
	return menu(ctx, e, os.Stdin, s.output)
}

func menu(ctx context.Context, e *engine.Engine, input, output *os.File) (resultErr error) {
	ui := &terminal.Workflow{Input: input, Output: output}
	defer func() { resultErr = errors.Join(resultErr, ui.Close()) }()
	e.UI = ui
	return finish(e.Run(ctx))
}

func execute(ctx context.Context, e *engine.Engine, c command, output *os.File) error {
	if c.name == "check" {
		result, err := e.Check(ctx)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprint(output, terminal.ReportText(result)); err != nil {
			return err
		}
		return finish(result, nil)
	}
	request := c.request()
	plan, err := e.Preview(ctx, request)
	if err != nil {
		return err
	}
	preview := strings.Join(terminal.PlanText(e.Catalog, plan), "\n") + "\n"
	if notice := engine.FallbackNotice(plan); notice != "" {
		preview += notice + "\n"
	}
	if !c.yes && c.name != "plan" {
		preview += "Nothing was changed. To apply this plan, run the same command with --yes.\n"
	}
	if _, err := fmt.Fprint(output, preview); err != nil || !c.yes {
		return err
	}
	ui := &terminal.Workflow{Output: output}
	applying, err := ui.BeginApply(ctx)
	if err != nil {
		return err
	}
	result, err := e.Apply(applying, request, plan)
	if err != nil && !errors.Is(err, engine.ErrReplan) && !errors.Is(err, engine.ErrPending) {
		result.Status, result.Message = "failed", strings.TrimSpace(err.Error()+"\n"+result.Message)
	}
	if reportErr := ui.Report(result); reportErr != nil {
		return errors.Join(err, reportErr)
	}
	if err != nil {
		err = &engine.ReportedError{Err: err}
	}
	return finish(result, err)
}

func finish(result engine.Result, err error) error {
	var reported *engine.ReportedError
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, engine.ErrReplan), errors.Is(err, engine.ErrPlanChanged):
		return fmt.Errorf("setup stopped before the end of the plan; run the same command again: %w", err)
	case errors.As(err, &reported):
		return errors.New("setup did not finish; the report above shows the cause and its log")
	case err != nil:
		return err
	case result.Status == "failed" || result.Status == "needs-action":
		return errors.New("setup did not finish; the report above lists what needs your action")
	}
	return nil
}

const sudoPrompt = "Dotfiles needs administrator access for the approved changes. [sudo] password for %u: "

func discover(ctx context.Context, s *setup) error {
	s.sentinel = hooks.SentinelLocations{CodexHome: os.Getenv("CODEX_HOME"), ConfigHome: os.Getenv("XDG_CONFIG_HOME"), ClaudeConfigDir: os.Getenv("CLAUDE_CONFIG_DIR"),
		KiroHome: os.Getenv("KIRO_HOME"), CopilotHome: os.Getenv("COPILOT_HOME"), Installed: hooks.DiscoverSentinelAgents(s.host.Context, s.folders.Home)}
	s.homebrewDirectory = func() string { return "" }
	failure := "administrator authentication failed"
	switch s.host.OS {
	case "darwin":
		directory := &homebrewDirectory{}
		s.homebrew, s.homebrewDirectory = directory.discover(ctx), directory.get
		if os.Geteuid() != 0 {
			s.sudo = "/usr/bin/sudo"
		}
	case "linux":
		var err error
		s.clipboard = system.DiscoverLinuxClipboard()
		if s.apt, err = system.DiscoverLinuxAPT(ctx); err != nil {
			s.apt, s.aptIssue = nil, err.Error()
		} else {
			s.sudo, failure = s.apt.Sudo, "APT administrator authentication failed"
		}
		if s.dpkgDeb, err = exec.LookPath("dpkg-deb"); err != nil {
			return fmt.Errorf("Ubuntu/Debian requires its native archive decoder dpkg-deb: %w", err)
		}
	case "windows":
		var err error
		if s.powershell, err = system.DiscoverWindowsPowerShell(); err != nil {
			s.powershell, s.windowsIssue = "", err.Error()
		}
		if s.buildTools, err = system.DiscoverWindowsBuildToolsDirectory(); err != nil {
			s.buildTools, s.windowsIssue = "", err.Error()
		}
	}
	if s.sudo != "" {
		s.authenticate = foregroundSudo(s.sudo, failure)
	}
	return ctx.Err()
}

type homebrewDirectory struct {
	mu        sync.Mutex
	directory string
}

func (h *homebrewDirectory) discover(ctx context.Context) *system.HomebrewLocation {
	h.mu.Lock()
	defer h.mu.Unlock()
	location, err := system.DiscoverHomebrew(ctx)
	if err != nil || location == nil || location.Validate() != nil {
		return nil
	}
	if system.NixHomebrewIssue(*location) == "" {
		h.directory = location.CommandDirectory()
	}
	return location
}

func (h *homebrewDirectory) get() string {
	h.mu.Lock()
	directory := h.directory
	h.mu.Unlock()
	if directory == "" {
		h.discover(context.Background())
		h.mu.Lock()
		directory = h.directory
		h.mu.Unlock()
	}
	return directory
}

func foregroundSudo(program, failure string) func(context.Context) error {
	return func(ctx context.Context) error {
		command := exec.CommandContext(ctx, program, "-v", "-p", sudoPrompt)
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("%s: %w", failure, err)
		}
		return nil
	}
}

func sudoSession(program string, authenticate func(context.Context) error) func(context.Context) (func() error, error) {
	return func(ctx context.Context) (func() error, error) {
		return platform.KeepSudoAlive(ctx, program, authenticate)
	}
}
