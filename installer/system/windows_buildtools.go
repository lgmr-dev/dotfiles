package system

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/platform"
)

// BuildTools installs the Microsoft C++ Build Tools (tool.compiler on
// Windows) with the reviewed, signed bootstrapper into InstallDirectory. The
// bootstrapper resolves its own servicing packages. Directory holds the
// verified bootstrapper. Query is read-only PowerShell. Run starts the
// bootstrapper in the foreground, and Windows shows the elevation prompt.
// Issue is a discovery failure of the wiring: it makes each inspection
// unknown. The provider reuses a healthy compiler of each Visual Studio
// instance, changes only the recorded instance in InstallDirectory, declines
// a downgrade and never uninstalls.
type BuildTools struct {
	Directory, PowerShell, InstallDirectory string
	Pin                                     catalog.WindowsBuildToolsPin
	Query                                   VendorQuery
	Run                                     platform.Runner
	Fetch                                   VendorFetch
	Issue                                   string
}

// NewBuildTools returns the provider of tool.compiler on Windows. It only
// reads its arguments. The provider keeps its files in
// <stateDir>/windows-buildtools.
func NewBuildTools(stateDir, powershell, installDirectory string, pin catalog.WindowsBuildToolsPin, run platform.Runner, fetch VendorFetch) *BuildTools {
	return &BuildTools{Directory: filepath.Join(stateDir, "windows-buildtools"), PowerShell: powershell, InstallDirectory: installDirectory, Pin: pin, Query: QueryWindowsBuildTools, Run: run, Fetch: fetch}
}

const (
	windowsBuildToolsName    = "Microsoft C++ Build Tools"
	windowsBuildToolsProduct = "Microsoft.VisualStudio.Product.BuildTools"
)

var windowsBuildToolsInstanceID = regexp.MustCompile(`^[a-f0-9]{8}$`)

type windowsBuildToolsPackage struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	Chip      string `json:"chip"`
	Language  string `json:"language"`
	Branch    string `json:"branch"`
	Type      string `json:"type"`
	Extension bool   `json:"extension"`
	Count     int    `json:"count"`
}

func windowsBuildToolsComparePackages(a, b windowsBuildToolsPackage) int {
	left := [...]string{a.ID, a.Version, a.Chip, a.Language, a.Branch, a.Type}
	right := [...]string{b.ID, b.Version, b.Chip, b.Language, b.Branch, b.Type}
	for i, value := range left {
		if order := strings.Compare(value, right[i]); order != 0 {
			return order
		}
	}
	if a.Extension != b.Extension {
		if a.Extension {
			return 1
		}
		return -1
	}
	return 0
}

type windowsBuildToolsInstance struct {
	ID       string                     `json:"id"`
	Path     string                     `json:"path"`
	Version  string                     `json:"version"`
	Product  string                     `json:"product"`
	Complete bool                       `json:"complete"`
	Packages []windowsBuildToolsPackage `json:"packages"`
	Files    map[string]string          `json:"files"`
}

type windowsBuildToolsSnapshot struct {
	Instances   []windowsBuildToolsInstance `json:"instances"`
	Selected    string                      `json:"selected"`
	Healthy     bool                        `json:"healthy"`
	Occupied    bool                        `json:"occupied"`
	HealthIssue string                      `json:"health_issue"`
	Consumers   []string                    `json:"consumers"`
}

func (s windowsBuildToolsSnapshot) selected() windowsBuildToolsInstance {
	for _, instance := range s.Instances {
		if instance.ID == s.Selected {
			return instance
		}
	}
	return windowsBuildToolsInstance{}
}

func (p *BuildTools) validate(r catalog.Resource) error {
	if r.ID != "tool.compiler" || p.Query == nil {
		return errors.New("invalid Windows Build Tools resource boundary")
	}
	if !vendorCanonical(p.Directory, p.PowerShell, p.InstallDirectory) {
		return errors.New("Build Tools requires canonical absolute paths")
	}
	if err := vendorUnredirected(p.Directory, p.InstallDirectory); err != nil {
		return err
	}
	return p.Pin.Validate()
}

func (p *BuildTools) query(ctx context.Context) (windowsBuildToolsSnapshot, error) {
	var s windowsBuildToolsSnapshot
	input, err := json.Marshal(struct{ InstallDirectory string }{p.InstallDirectory})
	if err != nil {
		return s, err
	}
	data, err := p.Query(ctx, p.PowerShell, input, windowsVendorArguments(windowsBuildToolsObserveScript)...)
	if err != nil {
		return s, fmt.Errorf("inspect registered Microsoft Build Tools: %w", err)
	}
	if len(data) > 1<<20 {
		return s, errors.New("Build Tools inspection exceeded its bound")
	}
	if err := platform.Decode(data, &s); err != nil {
		return s, err
	}
	if len(s.Instances) > 128 || len(s.Consumers) > 1024 || len(s.HealthIssue) > 4096 {
		return s, errors.New("invalid Build Tools inspection")
	}
	seen := map[string]bool{}
	for index := range s.Instances {
		instance := &s.Instances[index]
		if !windowsBuildToolsInstanceID.MatchString(instance.ID) || seen[instance.ID] || !filepath.IsAbs(instance.Path) || filepath.Clean(instance.Path) != instance.Path || !windowsVendorVersion.MatchString(instance.Version) || !strings.HasPrefix(instance.Product, "Microsoft.VisualStudio.Product.") || len(instance.Packages) > 10000 || len(instance.Files) > 4 {
			return s, errors.New("invalid registered Visual Studio instance")
		}
		seen[instance.ID] = true
		slices.SortFunc(instance.Packages, windowsBuildToolsComparePackages)
		total := 0
		for i, entry := range instance.Packages {
			tokens := entry.ID + entry.Version + entry.Chip + entry.Language + entry.Branch + entry.Type
			if entry.ID == "" || len(tokens) > 512 || strings.ContainsAny(tokens, "\x00\r\n") || entry.Count < 1 || entry.Count > 10000 || i > 0 && windowsBuildToolsComparePackages(instance.Packages[i-1], entry) == 0 {
				return s, errors.New("invalid Visual Studio package inventory")
			}
			total += entry.Count
			if total > 10000 {
				return s, errors.New("Visual Studio package inventory exceeds its occurrence bound")
			}
		}
		for key, value := range instance.Files {
			if !slices.Contains([]string{"cl.exe", "link.exe", "VsDevCmd.bat", "Microsoft.VCToolsVersion.default.txt"}, key) || !windowsVendorSHA256.MatchString(value) {
				return s, errors.New("invalid Visual Studio tool identity")
			}
		}
	}
	if s.Selected != "" && !seen[s.Selected] || s.Healthy && (s.Selected == "" || len(s.selected().Files) != 4 || !s.selected().Complete) {
		return s, errors.New("invalid selected Visual Studio instance")
	}
	for _, consumer := range s.Consumers {
		if consumer == "" || len(consumer) > 512 || strings.ContainsAny(consumer, "\x00\r\n") {
			return s, errors.New("invalid Build Tools consumer")
		}
	}
	slices.Sort(s.Consumers)
	s.Consumers = slices.Compact(s.Consumers)
	return s, nil
}

func (p *BuildTools) dedicated(instance windowsBuildToolsInstance) bool {
	return instance.ID != "" && instance.Path == p.InstallDirectory && instance.Product == windowsBuildToolsProduct
}

func windowsBuildToolsInstallerArguments(action, installDirectory string, components []string) (string, error) {
	if strings.ContainsAny(installDirectory, "\"\x00\r\n") {
		return "", errors.New("invalid Build Tools installation directory")
	}
	arguments := []string{"--quiet", "--wait", "--norestart", "--installPath", `"` + installDirectory + `"`}
	switch action {
	case "install":
		for _, component := range components {
			arguments = append(arguments, "--add", component)
		}
	case "update", "repair":
		arguments = append([]string{action}, arguments...)
	default:
		return "", errors.New("unknown Build Tools action")
	}
	return strings.Join(arguments, " "), nil
}

func windowsBuildToolsAction(status engine.Status) string {
	return map[engine.Status]string{engine.Missing: "install", engine.Outdated: "update", engine.Unhealthy: "repair"}[status]
}

func (p *BuildTools) removalBlock(instance windowsBuildToolsInstance) engine.CommandBlock {
	block := engine.CommandBlock{Title: "Remove the " + windowsBuildToolsName + " " + instance.Version + " yourself; examine first which projects use this compiler", By: "you",
		Lines: []string{"Visual Studio Installer > " + instance.Path + " > More > Uninstall"}}
	file := vendorInstallerPath(p.Directory, p.Pin.SHA256)
	if p.dedicated(instance) && vendorInstallerReady(file, p.Pin.SHA256, p.Pin.Size) == nil {
		block.Lines = []string{"& " + vendorPowerShellLiteral(file) + " uninstall --wait --norestart --installPath " + vendorPowerShellLiteral(p.InstallDirectory)}
	}
	return block
}

// Inspect reads the registered Visual Studio instances and compiles and runs
// a probe with the selected compiler. It reports the installed and the pinned
// version. For a compiler that is not wanted, it returns the manual removal
// route only when the state has its record.
func (p *BuildTools) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	if p.Issue != "" {
		return engine.Finding{Status: engine.Unknown, Reason: p.Issue}, nil
	}
	if err := p.validate(r); err != nil {
		return engine.Finding{}, err
	}
	s, err := p.query(ctx)
	if err != nil {
		return vendorUnknown(ctx, "the "+windowsBuildToolsName, err)
	}
	instance := s.selected()
	recorded := rec.State().HasSystem(vendorSystemKey, vendorRecordName(r))
	f := engine.Finding{Version: instance.Version, Privileged: true, Pending: true}
	f.Token, err = vendorDigest(struct {
		Recorded, Healthy, Occupied bool
		Pin                         string
		Instance                    windowsBuildToolsInstance
	}{recorded, s.Healthy, s.Occupied, p.Pin.Version, instance})
	if err != nil {
		return f, err
	}
	present := s.Selected != ""
	versions := "installed " + instance.Version + ", pinned " + p.Pin.Version
	if !want.Present {
		if !present {
			f.Status = engine.Absent
			return f, nil
		}
		f.Status, f.Reason = engine.Satisfied, "setup never uninstalls the "+windowsBuildToolsName
		if recorded {
			f.Commands = []engine.CommandBlock{p.removalBlock(instance)}
			f.Notes = append([]string{"Manual and other-user projects can use this Microsoft C++ toolchain", "The Microsoft Visual Studio Installer and the shared SDK and runtime components of the vendor stay"}, s.Consumers...)
		}
		return f, nil
	}
	owned := recorded && p.dedicated(instance)
	order := vendorCompareVersions(instance.Version, p.Pin.Version)
	switch {
	case !present && s.Occupied:
		f.Status, f.Reason = engine.Blocked, "the dedicated Build Tools directory "+p.InstallDirectory+" already exists without a registered instance; preserve it and examine it"
		f.Preserved = []string{p.InstallDirectory}
	case !present:
		f.Status, f.Reason = engine.Missing, "install the "+windowsBuildToolsName+" "+p.Pin.Version+" into "+p.InstallDirectory
	case !s.Healthy && !owned:
		f.Status, f.Reason = engine.Blocked, "preserve the existing Microsoft C++ compiler in "+instance.Path+", which setup did not install: "+vendorOneLine(s.HealthIssue)+"; repair it with the Visual Studio Installer"
	case !s.Healthy:
		f.Status, f.Reason = engine.Unhealthy, vendorOneLine(s.HealthIssue)+" ("+versions+"); restart Windows first if its installer asked for a restart"
	case order < 0 && owned:
		f.Status, f.Reason = engine.Outdated, versions
	case order < 0:
		f.Status = engine.Satisfied
		f.Notes = []string{"The Microsoft C++ compiler in " + instance.Path + " is older than the reviewed pin (" + versions + "); setup did not install it and requests no change"}
	default:
		f.Status = engine.Satisfied
	}
	if action := windowsBuildToolsAction(f.Status); action != "" {
		arguments, err := windowsBuildToolsInstallerArguments(action, p.InstallDirectory, p.Pin.Components)
		if err != nil {
			return f, err
		}
		f.Commands = []engine.CommandBlock{{Title: windowsBuildToolsName + " " + p.Pin.Version + " (signed Microsoft installer, with elevation)", Lines: []string{"vs_BuildTools.exe " + arguments}, By: "setup"}}
	}
	return f, nil
}

// Apply installs, updates or repairs the dedicated Build Tools instance with
// the reviewed bootstrapper. It records the pending mark and the provisional
// system record before the bootstrapper starts, and clears the mark after the
// compiler probe passes. Exit code 3010 is complete only when the probe
// passes.
func (p *BuildTools) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	if p.Issue != "" {
		return errors.New(p.Issue)
	}
	if err := p.validate(r); err != nil {
		return err
	}
	if p.Run == nil {
		return errors.New("the " + windowsBuildToolsName + " installer needs the foreground command runner")
	}
	before, err := p.query(ctx)
	if err != nil {
		return err
	}
	action := windowsBuildToolsAction(approved.Status)
	recorded := rec.State().HasSystem(vendorSystemKey, vendorRecordName(r))
	if action == "" || action == "install" && (before.Selected != "" || before.Occupied) || action != "install" && (!recorded || !p.dedicated(before.selected())) {
		return errors.New("the Microsoft C++ compiler changed after the review, or setup did not install it; review a new plan")
	}
	arguments, err := windowsBuildToolsInstallerArguments(action, p.InstallDirectory, p.Pin.Components)
	if err != nil {
		return err
	}
	if err := vendorPrepareInstaller(ctx, p.Fetch, p.Directory, p.Pin.URL, p.Pin.SHA256, p.Pin.Size); err != nil {
		return err
	}
	input := struct {
		Action, File, SHA256, InstallDirectory, Arguments string
		Before                                            windowsBuildToolsSnapshot
	}{action, vendorInstallerPath(p.Directory, p.Pin.SHA256), p.Pin.SHA256, p.InstallDirectory, arguments, before}
	return vendorChange(rec, r, action, func() (int, bool, error) {
		return vendorRun(ctx, p.Run, p.PowerShell, windowsBuildToolsInstallScript, input, true)
	}, func(code int, cause error) error {
		if !vendorSucceeded(code) {
			return vendorFailure(windowsBuildToolsName, code, "", cause)
		}
		after, err := p.query(ctx)
		if err != nil {
			return err
		}
		instance := after.selected()
		if !p.dedicated(instance) || !instance.Complete {
			return errors.New("vendor setup did not establish a complete registered dedicated C++ instance")
		}
		for _, component := range p.Pin.Components {
			if !slices.ContainsFunc(instance.Packages, func(entry windowsBuildToolsPackage) bool { return entry.ID == component }) {
				return fmt.Errorf("Build Tools is missing the requested component %s", component)
			}
		}
		if action == "install" {
			if slices.ContainsFunc(before.Instances, func(old windowsBuildToolsInstance) bool { return old.ID == instance.ID }) {
				return errors.New("vendor setup reused a pre-existing Visual Studio instance")
			}
		} else if instance.ID != before.Selected {
			return errors.New("vendor setup replaced the instance identity")
		}
		if !after.Healthy {
			if code != 0 {
				return errors.New("the " + windowsBuildToolsName + " setup needs a restart of Windows; restart, then run setup again")
			}
			return errors.New("vendor setup did not leave a usable C++ compiler: " + vendorOneLine(after.HealthIssue))
		}
		if code != 0 {
			engine.ReportDetail(ctx, "The %s are usable. Their installer asked for a restart of Windows.", windowsBuildToolsName)
		}
		return nil
	})
}

// Remove never uninstalls a compiler. It deletes the system record only when
// no compiler is present.
func (p *BuildTools) Remove(ctx context.Context, r catalog.Resource, _ engine.Finding, rec engine.Records) error {
	if p.Issue != "" {
		return errors.New(p.Issue)
	}
	if err := p.validate(r); err != nil {
		return err
	}
	s, err := p.query(ctx)
	if err != nil {
		return err
	}
	return vendorForget(rec, r, s.Selected != "")
}

// CompilerEnvironment returns the SDK variables of the selected, usable
// compiler: PATH, INCLUDE, LIB, LIBPATH, VCINSTALLDIR, VCToolsInstallDir,
// WindowsSdkDir and WindowsSDKVersion. PATH holds only folders of the vendor;
// a caller puts it before the PATH of its command environment.
func (p *BuildTools) CompilerEnvironment(ctx context.Context) (map[string]string, error) {
	if p.Issue != "" {
		return nil, errors.New(p.Issue)
	}
	if err := p.validate(catalog.Resource{ID: "tool.compiler"}); err != nil {
		return nil, err
	}
	snapshot, err := p.query(ctx)
	if err != nil {
		return nil, err
	}
	if !snapshot.Healthy {
		return nil, errors.New("a usable Microsoft C++ toolchain is required before resolving its SDK environment")
	}
	input, err := json.Marshal(struct{ InstallDirectory, Instance, Probe string }{p.InstallDirectory, snapshot.Selected, windowsVendorArguments(windowsBuildToolsEnvironmentProbe)[4]})
	if err != nil {
		return nil, err
	}
	data, err := p.Query(ctx, p.PowerShell, input, windowsVendorArguments(windowsBuildToolsEnvironmentScript)...)
	if err != nil {
		return nil, fmt.Errorf("resolve Microsoft C++ environment: %w", err)
	}
	var environment map[string]string
	if len(data) > 256<<10 {
		return nil, errors.New("compiler environment exceeded its bound")
	}
	if err := platform.Decode(data, &environment); err != nil {
		return nil, err
	}
	allowed := []string{"PATH", "INCLUDE", "LIB", "LIBPATH", "VCINSTALLDIR", "VCToolsInstallDir", "WindowsSdkDir", "WindowsSDKVersion"}
	if len(environment) != len(allowed) {
		return nil, errors.New("compiler environment is incomplete")
	}
	for name, value := range environment {
		if !slices.Contains(allowed, name) || value == "" || len(value) > 32767 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("compiler environment contains an unsupported or malformed value")
		}
	}
	return environment, nil
}
