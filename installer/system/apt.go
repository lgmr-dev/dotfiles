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
	"strings"
	"sync"
	"time"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

// APTLocation holds the absolute paths of the APT programs. Sudo is empty
// and Root is true when the process runs as root.
type APTLocation struct {
	APTGet, APTCache, Dpkg, DpkgQuery, APTMark, Sudo string
	Root                                             bool
}

// DiscoverLinuxAPT finds the APT programs and sudo at the process boundary.
// It runs no package command.
func DiscoverLinuxAPT(ctx context.Context) (*APTLocation, error) {
	location := &APTLocation{Root: os.Geteuid() == 0}
	programs := map[string]*string{"apt-get": &location.APTGet, "apt-cache": &location.APTCache, "dpkg": &location.Dpkg, "dpkg-query": &location.DpkgQuery, "apt-mark": &location.APTMark}
	if !location.Root {
		programs["sudo"] = &location.Sudo
	}
	for name, target := range programs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path, err := exec.LookPath(name)
		if err != nil {
			if name == "sudo" {
				return nil, fmt.Errorf("APT needs administrator access: sudo is unavailable; an administrator must provide sudo or run the installer as root: %w", err)
			}
			return nil, fmt.Errorf("APT requires %s: %w", name, err)
		}
		*target = path
	}
	return location, location.Validate()
}

// Validate refuses a location with a path that is not absolute and clean.
func (location APTLocation) Validate() error {
	paths := []string{location.APTGet, location.APTCache, location.Dpkg, location.DpkgQuery, location.APTMark}
	if !location.Root {
		paths = append(paths, location.Sudo)
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return errors.New("APT requires canonical absolute native executable paths")
		}
	}
	return nil
}

type aptBoundedOutput struct {
	data  bytes.Buffer
	limit int
}

func (output *aptBoundedOutput) Write(data []byte) (int, error) {
	n := len(data)
	if remaining := output.limit - output.data.Len(); n > remaining {
		data = data[:max(remaining, 0)]
	}
	_, err := output.data.Write(data)
	return n, err
}

// QueryAPT runs one read-only inspection command without elevation and
// without input. It is the default Query of an APT provider.
func QueryAPT(ctx context.Context, program string, args ...string) ([]byte, error) {
	if !filepath.IsAbs(program) || filepath.Clean(program) != program {
		return nil, errors.New("APT inspection requires an absolute executable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, program, args...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, diagnostic := aptBoundedOutput{limit: 8 << 20}, aptBoundedOutput{limit: 1 << 20}
	command.Stdout, command.Stderr = &output, &diagnostic
	if err := command.Run(); err != nil {
		return output.data.Bytes(), fmt.Errorf("APT inspection failed: %w%s", err, platform.Diagnostic(diagnostic.data.Bytes()))
	}
	return output.data.Bytes(), nil
}

// APT is the provider of the resources with the apt binding, the libraries
// and the pinned DEB routes of Code and Tailscale included. It installs,
// updates and repairs through apt-get in the foreground. It never removes a
// package: for a recorded package that the selection no longer needs, Inspect
// returns the removal command for the operator.
//
// Directory holds the downloaded pinned DEB files. Temporary is the parent of
// the Code debconf input, which must be a path without a space. Query runs a
// read-only inspection command. Authenticate asks for the sudo password one
// time in the foreground before the first privileged command. Download
// fetches a URL to a file and verifies its SHA-256 and size.
//
// DpkgConfiguration is the folder of dpkg.cfg and dpkg.cfg.d, /etc/dpkg when
// empty. The provider reads its path-exclude and path-include options one
// time: a file that dpkg did not install because of them is not a changed
// package file.
type APT struct {
	Location          APTLocation
	Arch              string
	Directory         string
	Temporary         string
	DpkgConfiguration string
	Pins              catalog.Pins
	Query             func(ctx context.Context, program string, args ...string) ([]byte, error)
	Runner            platform.Runner
	Authenticate      func(context.Context) error
	Download          func(ctx context.Context, url, sha256 string, size int64, destination string) error

	pathFiltersOnce sync.Once
	pathFilters     []aptPathFilter
}

// NewAPT returns the provider for the discovered location. It is pure.
// Directory is a private folder in the state folder. The wiring sets
// Authenticate and Download.
func NewAPT(location APTLocation, pins catalog.Pins, arch, directory string) *APT {
	return &APT{Location: location, Arch: arch, Directory: directory, Temporary: "/var/tmp", Pins: pins, Query: QueryAPT, Runner: platform.Exec{}}
}

func (a *APT) validate(r catalog.Resource) (string, error) {
	binding := r.Bindings["linux"]
	if (a.Arch != "amd64" && a.Arch != "arm64") || binding.Provider != "apt" || !aptArgument.MatchString(binding.Package) || a.Query == nil {
		return "", errors.New("invalid APT resource boundary")
	}
	if err := a.Location.Validate(); err != nil {
		return "", err
	}
	if !filepath.IsAbs(a.Directory) || filepath.Clean(a.Directory) != a.Directory {
		return "", errors.New("APT requires a canonical folder for its package inputs")
	}
	return binding.Package, nil
}

func (a *APT) healthCommand(r catalog.Resource, name string) ([]string, error) {
	switch name {
	case "tailscale", "code":
		return []string{a.Location.Dpkg, "--verify", name}, nil
	case "xz-utils":
		return []string{"/usr/bin/xz", "--version"}, nil
	case "git":
		return []string{"/usr/bin/git", "--version"}, nil
	case "zsh":
		return []string{"/usr/bin/zsh", "--version"}, nil
	case "tmux":
		return []string{"/usr/bin/tmux", "-V"}, nil
	case "build-essential":
		return []string{"/usr/bin/cc", "--version"}, nil
	case "make":
		return []string{"/usr/bin/make", "--version"}, nil
	case "clangd":
		return []string{"/usr/bin/clangd", "--version"}, nil
	case "curl":
		return []string{"/usr/bin/curl", "--version"}, nil
	case "unzip":
		return []string{"/usr/bin/unzip", "-v"}, nil
	case "xclip":
		return []string{"/usr/bin/xclip", "-version"}, nil
	case "wl-clipboard":
		return []string{"/usr/bin/wl-copy", "--version"}, nil
	case "fontconfig":
		return []string{"/usr/bin/fc-cache", "--version"}, nil
	}
	if r.Action != "apt-library" {
		return nil, fmt.Errorf("%s lacks its APT command health check", r.ID)
	}
	if !strings.Contains(name, ":") {
		name += ":" + a.Arch
	}
	return []string{a.Location.Dpkg, "--verify", name}, nil
}

func (a *APT) health(ctx context.Context, r catalog.Resource, name, version string, pinned bool) error {
	command, err := a.healthCommand(r, name)
	if err != nil {
		return err
	}
	output, err := a.Query(ctx, command[0], command[1:]...)
	if err != nil {
		return fmt.Errorf("APT %s failed its command check: %w%s", r.Name, err, platform.Diagnostic(output))
	}
	vendorPackage := pinned && name != "code"
	changedFiles := false
	unreadable := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		switch {
		case line == "", a.excludedMissing(line):
		case vendorPackage && len(line) > 12 && line[9:12] == " c " && line[12] == '/':
		case name == "code" && strings.HasPrefix(line, "?????????   /"):
			unreadable = append(unreadable, line[12:])
		default:
			changedFiles = true
		}
	}
	if (r.Action == "apt-library" || pinned) && changedFiles {
		return fmt.Errorf("APT %s has changed package files%s", r.Name, platform.Diagnostic(output))
	}
	if name == "code" {
		modes := a.Pins.LinuxCodeDeclaredModes(version)
		arguments := append([]string{"-c", debCodeHealth, "sh", modes.Sandbox, modes.RootOnly}, unreadable...)
		if output, err := a.Query(ctx, "/bin/sh", arguments...); err != nil || len(output) != 0 {
			return errors.Join(fmt.Errorf("Code canonical native executable is missing or changed%s", platform.Diagnostic(output)), err)
		}
	}
	return nil
}

func (a *APT) versionGreater(ctx context.Context, left, right string) (bool, error) {
	for _, version := range []string{left, right} {
		if version == "" || len(version) > 256 || strings.ContainsAny(version, "\x00\r\n\t ") {
			return false, errors.New("invalid package version observation")
		}
	}
	output, err := a.Query(ctx, a.Location.Dpkg, "--compare-versions", left, "gt", right)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if len(output) != 0 {
		return false, errors.New("unexpected dpkg version comparison output")
	}
	if err == nil {
		return true, nil
	}
	if code, ok := platform.ExitCode(err); ok && code == 1 {
		return false, nil
	}
	return false, errors.Join(errors.New("cannot compare package versions"), err)
}

type aptObservation struct {
	Resource, Name, Identity string
	Present, Recorded        bool
	Package                  aptPackage
	Issue                    string
	Pin                      string
	Newer                    bool
	Candidate                string
	Collision                string
}

func (a *APT) observe(ctx context.Context, r catalog.Resource, st state.State) (aptObservation, debPin, error) {
	name, err := a.validate(r)
	if err != nil {
		return aptObservation{}, debPin{}, err
	}
	pin, pinned, err := a.debPin(r, name)
	if err != nil {
		return aptObservation{}, pin, err
	}
	installed, err := a.inventory(ctx)
	if err != nil {
		return aptObservation{}, pin, err
	}
	o := aptObservation{Resource: r.ID, Name: name, Identity: aptIdentity(name, a.Arch, installed), Recorded: st.HasSystem("apt", name), Pin: pin.Version}
	o.Package, o.Present = installed[o.Identity]
	if !o.Present {
		if err := a.debCollisions(ctx, name, true); err != nil {
			if ctx.Err() != nil {
				return o, pin, ctx.Err()
			}
			o.Collision = err.Error()
		}
		return o, pin, nil
	}
	if !o.Package.Healthy {
		o.Issue = "the package database reports an incomplete or pending state of " + o.Identity
	} else if err := a.health(ctx, r, name, o.Package.Version, pinned); err != nil {
		if ctx.Err() != nil {
			return o, pin, ctx.Err()
		}
		o.Issue = err.Error()
	}
	if pinned && o.Package.Version != pin.Version {
		if o.Newer, err = a.versionGreater(ctx, o.Package.Version, pin.Version); err != nil {
			return o, pin, err
		}
	}
	if !pinned && o.Recorded && o.Issue == "" && !o.Package.Held {
		output, err := a.Query(ctx, a.Location.APTCache, "policy", o.Identity)
		if err != nil {
			return o, pin, err
		}
		candidate, err := parseAPTCandidate(output)
		if err != nil {
			return o, pin, err
		}
		if candidate != "" && candidate != o.Package.Version {
			newer, err := a.versionGreater(ctx, candidate, o.Package.Version)
			if err != nil {
				return o, pin, err
			}
			if newer {
				o.Candidate = candidate
			}
		}
	}
	return o, pin, nil
}

func (o aptObservation) action() string {
	switch {
	case !o.Present:
		if o.Collision != "" {
			return ""
		}
		return "install"
	case o.Package.Held || !o.Recorded || o.Newer:
		return ""
	case o.Issue != "":
		return "repair"
	case o.Pin != "" && o.Package.Version != o.Pin, o.Candidate != "":
		return "update"
	}
	return ""
}

func aptDigest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func aptPinLabel(name string) string {
	if name == "code" {
		return "Code"
	}
	return name
}

func (a *APT) setupCommands(o aptObservation, pin debPin, action string) engine.CommandBlock {
	input := o.Name
	if o.Pin != "" {
		input = a.debFile(pin)
	}
	titles := map[string]string{"install": "Install ", "update": "Update ", "repair": "Repair "}
	return engine.CommandBlock{Title: titles[action] + o.Name + " with APT", By: "setup", Lines: []string{
		a.display(a.Location.APTGet, aptRefreshArguments()...),
		a.display(a.Location.APTGet, aptInstallArguments(action, o.Package.Automatic, input)...),
	}}
}

// Inspect reads the package database and the health of the package. It
// reuses a healthy package that is present. It proposes a change of a present
// package only when the state has a record of it.
func (a *APT) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	o, pin, err := a.observe(ctx, r, rec.State())
	if err != nil {
		return engine.Finding{}, err
	}
	token, err := aptDigest(struct {
		Observation aptObservation
		Present     bool
	}{o, want.Present})
	if err != nil {
		return engine.Finding{}, err
	}
	f := engine.Finding{Status: engine.Satisfied, Version: o.Package.Version, Token: token}
	if !want.Present {
		if !o.Present {
			f.Status, f.Reason = engine.Absent, "the package is not installed"
			return f, nil
		}
		if o.Recorded {
			f.Commands = []engine.CommandBlock{{Title: "Remove " + o.Name + " with APT (setup never removes a system package)", By: "you",
				Lines: []string{"sudo " + filepath.Base(a.Location.APTGet) + " " + strings.Join(aptRemovalArguments(o.Name), " ")}}}
			f.Notes = append(f.Notes, "Examine the packages that APT proposes to remove before you confirm. Setup deletes its record of "+o.Name+" after the package is absent.")
		}
		return f, nil
	}
	action := o.action()
	if action != "" {
		f.Privileged, f.Pending = true, true
		f.Commands = []engine.CommandBlock{a.setupCommands(o, pin, action)}
		f.Notes = append(f.Notes, "APT can install or upgrade the packages that "+o.Name+" depends on. The --no-remove guard stops a change that removes a package.")
	}
	switch {
	case action == "install":
		f.Status = engine.Missing
		if o.Pin != "" {
			f.Reason = "install reviewed " + aptPinLabel(o.Name) + " pin " + o.Pin
		}
	case action == "repair":
		f.Status, f.Reason = engine.Unhealthy, o.Issue
	case action == "update" && o.Pin != "":
		f.Status, f.Reason = engine.Outdated, fmt.Sprintf("%s %q -> reviewed pin %s", aptPinLabel(o.Name), o.Package.Version, o.Pin)
	case action == "update":
		f.Status, f.Reason = engine.Outdated, fmt.Sprintf("APT has version %s; version %s is installed", o.Candidate, o.Package.Version)
	case !o.Present:
		f.Status, f.Reason = engine.Blocked, o.Collision
	case o.Issue != "":
		f.Status, f.Reason = engine.Blocked, a.blockedRepair(o)
		if !o.Package.Held && !o.Newer {
			f.Commands = []engine.CommandBlock{{Title: "Repair " + o.Name + " with APT", By: "you",
				Lines: []string{"sudo " + filepath.Base(a.Location.APTGet) + " install --reinstall " + o.Name}}}
		}
	case o.Newer:
		f.Notes = append(f.Notes, fmt.Sprintf("Installed %s version %s is newer than the reviewed pin %s. Setup does not downgrade it.", aptPinLabel(o.Name), o.Package.Version, o.Pin))
	case o.Pin != "" && o.Package.Version != o.Pin && o.Package.Held:
		f.Notes = append(f.Notes, fmt.Sprintf("Installed %s version %s differs from the reviewed pin %s. The package is held, and setup keeps the hold.", aptPinLabel(o.Name), o.Package.Version, o.Pin))
	case o.Pin != "" && o.Package.Version != o.Pin:
		f.Notes = append(f.Notes, fmt.Sprintf("Installed %s version %s differs from the reviewed pin %s. Setup did not install this package and does not change it.", aptPinLabel(o.Name), o.Package.Version, o.Pin))
	}
	return f, nil
}

func (a *APT) blockedRepair(o aptObservation) string {
	switch {
	case o.Package.Held:
		return o.Issue + "; the package is held, and setup keeps the hold: repair it yourself"
	case o.Newer:
		return fmt.Sprintf("%s; installed version %s is newer than the reviewed pin %s, and setup does not downgrade: repair it yourself", o.Issue, o.Package.Version, o.Pin)
	}
	return o.Issue + "; setup did not install this package and does not change it: repair it yourself"
}

func aptMark(r catalog.Resource, action string) string { return r.ID + ":apt-" + action }

func (a *APT) run(ctx context.Context, c platform.Command) ([]byte, bool, error) {
	if a.Runner == nil {
		return nil, false, errors.New("APT has no command runner")
	}
	output, err := a.Runner.Run(ctx, c)
	if err == nil {
		return output, true, nil
	}
	_, started := platform.ExitCode(err)
	return output, started, err
}

// Apply installs a missing package, repairs a recorded package or updates a
// recorded package. A pinned DEB goes to the reviewed pin, and never to an
// older version. Apply writes the pending mark, and for a new install the
// provisional system record, before apt-get starts. It clears the mark after
// it verified the package.
func (a *APT) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) (result error) {
	o, pin, err := a.observe(ctx, r, rec.State())
	if err != nil {
		return err
	}
	action := o.action()
	if action == "" {
		if !o.Present {
			return errors.New(o.Collision)
		}
		if o.Issue != "" {
			return errors.New(a.blockedRepair(o))
		}
		return nil
	}
	input, environment := o.Name, []string{}
	if o.Pin != "" {
		if err := a.debCollisions(ctx, o.Name, !o.Present); err != nil {
			return err
		}
		engine.ReportDetail(ctx, "Preparing the reviewed %s package %s", aptPinLabel(o.Name), pin.Version)
		if err := a.debPrepare(ctx, pin); err != nil {
			return err
		}
		input = a.debFile(pin)
	}
	if o.Name == "code" {
		code, err := newDebCodeInput(a.Temporary)
		if err != nil {
			return err
		}
		defer func() {
			if err := code.cleanup(); err != nil {
				engine.ReportDetail(ctx, "Note: the Code temporary input %s stays for inspection: %v", code.Directory, err)
			}
		}()
		environment = []string{code.override()}
	}
	if !a.Location.Root && a.Authenticate != nil {
		if err := a.Authenticate(ctx); err != nil {
			return err
		}
	}
	if output, _, err := a.run(ctx, a.privileged(a.Location.APTGet, nil, aptRefreshArguments()...)); err != nil {
		if strings.Contains(string(output), "sudo:") {
			return fmt.Errorf("APT needs administrator authorization in the foreground; authorize sudo and run setup again: %w", err)
		}
		return fmt.Errorf("APT could not refresh its package lists; fix or disable the source named in the E: lines (`sudo apt-get update` lists it too), then run setup again: %w", err)
	}
	if o.Pin != "" {
		if err := a.debVerify(pin); err != nil {
			return err
		}
	}
	mark, provisional := aptMark(r, action), action == "install" && !o.Recorded
	if err := rec.Update(func(s *state.State) error {
		s.Mark(mark)
		if provisional {
			s.AddSystem("apt", o.Name)
		}
		return nil
	}); err != nil {
		return err
	}
	output, started, err := a.run(ctx, a.privileged(a.Location.APTGet, environment, aptInstallArguments(action, o.Package.Automatic, input)...))
	if err != nil && !started {
		return errors.Join(fmt.Errorf("APT did not start: %w", err), rec.Update(func(s *state.State) error {
			s.Clear(mark)
			if provisional {
				s.RemoveSystem("apt", o.Name)
			}
			return nil
		}))
	}
	if err != nil {
		if strings.Contains(string(output), "sudo:") {
			return fmt.Errorf("APT needs administrator authorization in the foreground; authorize sudo, run setup again and confirm that the command exited: %w", err)
		}
		return fmt.Errorf("APT native command failed; examine its output, run setup again and confirm that the command exited: %w", err)
	}
	after, _, err := a.observe(ctx, r, rec.State())
	if err != nil {
		return err
	}
	if !after.Present || after.Issue != "" {
		return fmt.Errorf("APT package %s has not reached its installed and healthy state: %s", o.Name, after.Issue)
	}
	if o.Pin != "" && (after.Package.Version != pin.Version || after.Identity != pin.Package+":"+pin.Architecture) {
		return fmt.Errorf("APT did not reach the reviewed %s version %s: version %q is installed", aptPinLabel(o.Name), pin.Version, after.Package.Version)
	}
	if err := rec.Update(func(s *state.State) error { s.Clear(mark); return nil }); err != nil {
		return err
	}
	if o.Pin != "" {
		if err := a.debCleanup(pin); err != nil {
			engine.ReportDetail(ctx, "Note: the package input %s stays for inspection: %v", filepath.Dir(a.debFile(pin)), err)
		}
	}
	return nil
}

// Remove never removes a package. It deletes the system record of the
// package only after it verified that the package is absent.
func (a *APT) Remove(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	name, err := a.validate(r)
	if err != nil {
		return err
	}
	installed, err := a.inventory(ctx)
	if err != nil {
		return err
	}
	if identity := aptIdentity(name, a.Arch, installed); identity != "" {
		return fmt.Errorf("%s is installed, and setup never removes a system package; remove it with `sudo apt-get remove %s` if nothing else needs it", identity, name)
	}
	return rec.Update(func(s *state.State) error { s.RemoveSystem("apt", name); return nil })
}
