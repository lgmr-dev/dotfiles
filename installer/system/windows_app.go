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

// WindowsApps installs the applications of the windows-app binding with
// their reviewed, signed per-user installers. It never elevates. Directory
// holds the verified installers. Programs is the per-user Programs folder
// below the local application data folder. Query is read-only PowerShell. Run
// starts the installer in the foreground. Issue is a discovery failure of the
// wiring: it makes each inspection unknown. An application updates itself, so
// the provider compares no version after the installation. It reuses a
// healthy application, repairs only a recorded one and never uninstalls.
type WindowsApps struct {
	Directory, PowerShell, Programs string
	Pins                            map[string]catalog.WindowsAppPin
	Query                           VendorQuery
	Run                             platform.Runner
	Fetch                           VendorFetch
	Issue                           string
}

// NewWindowsApps returns the provider of the windows-app binding. It only
// reads its arguments. The provider keeps its files in
// <stateDir>/windows-app.
func NewWindowsApps(stateDir, powershell, localAppData string, pins map[string]catalog.WindowsAppPin, run platform.Runner, fetch VendorFetch) *WindowsApps {
	return &WindowsApps{Directory: filepath.Join(stateDir, "windows-app"), PowerShell: powershell, Programs: filepath.Join(localAppData, "Programs"), Pins: pins, Query: QueryWindowsVendor, Run: run, Fetch: fetch}
}

var windowsAppVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}$`)

const windowsAppInstallerArguments = "/S"

type windowsAppSnapshot struct {
	Present     bool   `json:"present"`
	Healthy     bool   `json:"healthy"`
	Version     string `json:"version"`
	Location    string `json:"location"`
	Scope       string `json:"scope"`
	Running     bool   `json:"running"`
	HealthIssue string `json:"health_issue"`
}

func windowsAppName(pin catalog.WindowsAppPin) string {
	return strings.TrimSuffix(pin.Executable, ".exe")
}

func (p *WindowsApps) validate(r catalog.Resource) (catalog.WindowsAppPin, error) {
	pin, found := p.Pins[r.ID]
	if !found || p.Query == nil || r.Bindings["windows"].Provider != "windows-app" || r.Bindings["windows"].Package == "" {
		return pin, errors.New("unsupported Windows application")
	}
	if !vendorCanonical(p.PowerShell, p.Directory, p.Programs) {
		return pin, errors.New("invalid Windows application provider boundary")
	}
	if err := vendorUnredirected(p.Directory); err != nil {
		return pin, err
	}
	return pin, pin.Validate()
}

func (p *WindowsApps) location(pin catalog.WindowsAppPin) string {
	return filepath.Join(p.Programs, pin.Folder)
}

func (p *WindowsApps) input(pin catalog.WindowsAppPin, fields map[string]string) ([]byte, error) {
	values := map[string]string{"ProductCode": pin.ProductCode, "Publisher": pin.Publisher, "Programs": p.Programs, "Folder": pin.Folder, "Executable": pin.Executable, "Uninstaller": pin.Uninstaller}
	for key, value := range fields {
		values[key] = value
	}
	return json.Marshal(values)
}

func (p *WindowsApps) query(ctx context.Context, pin catalog.WindowsAppPin) (windowsAppSnapshot, error) {
	var snapshot windowsAppSnapshot
	input, err := p.input(pin, nil)
	if err != nil {
		return snapshot, err
	}
	output, err := p.Query(ctx, p.PowerShell, input, windowsVendorArguments(windowsAppObserveScript)...)
	if err != nil {
		return snapshot, fmt.Errorf("inspect the registered %s application: %w", windowsAppName(pin), err)
	}
	if err := platform.Decode(output, &snapshot); err != nil {
		return snapshot, err
	}
	if !slices.Contains([]string{"", "user", "machine"}, snapshot.Scope) || snapshot.Version != "" && !windowsAppVersion.MatchString(snapshot.Version) || len(snapshot.HealthIssue) > 512 ||
		snapshot.Location != "" && (!filepath.IsAbs(snapshot.Location) || filepath.Clean(snapshot.Location) != snapshot.Location || strings.ContainsAny(snapshot.Location, "\x00\r\n")) ||
		snapshot.Healthy && (!snapshot.Present || snapshot.Scope == "" || snapshot.Location == "") {
		return snapshot, errors.New("invalid Windows application inspection")
	}
	return snapshot, nil
}

func (p *WindowsApps) managed(pin catalog.WindowsAppPin, snapshot windowsAppSnapshot) bool {
	return snapshot.Scope == "user" && strings.EqualFold(snapshot.Location, p.location(pin))
}

func (p *WindowsApps) removalBlock(pin catalog.WindowsAppPin, snapshot windowsAppSnapshot) engine.CommandBlock {
	name := windowsAppName(pin)
	block := engine.CommandBlock{Title: "Remove " + name + " yourself; close it first", By: "you", Lines: []string{"Settings > Apps > Installed apps > " + name + " > Uninstall"}}
	if snapshot.Healthy && p.managed(pin, snapshot) {
		block.Lines = []string{"& " + vendorPowerShellLiteral(filepath.Join(snapshot.Location, pin.Uninstaller)) + " /currentuser"}
	}
	return block
}

// Inspect reads the uninstall registration of the application and checks the
// publisher signature of its files. It reports the installed and the pinned
// version. For an application that is not wanted, it returns the manual
// removal route only when the state has its record.
func (p *WindowsApps) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	if p.Issue != "" {
		return engine.Finding{Status: engine.Unknown, Reason: p.Issue}, nil
	}
	pin, err := p.validate(r)
	if err != nil {
		return engine.Finding{}, err
	}
	name := windowsAppName(pin)
	snapshot, err := p.query(ctx, pin)
	if err != nil {
		return vendorUnknown(ctx, "the Windows application "+name, err)
	}
	recorded := rec.State().HasSystem(vendorSystemKey, vendorRecordName(r))
	f := engine.Finding{Version: snapshot.Version, Pending: true}
	f.Token, err = vendorDigest(struct {
		Recorded, Present, Healthy, Running bool
		ProductCode, Scope, Location        string
	}{recorded, snapshot.Present, snapshot.Healthy, snapshot.Running, pin.ProductCode, snapshot.Scope, strings.ToLower(snapshot.Location)})
	if err != nil {
		return f, err
	}
	if !want.Present {
		if !snapshot.Present {
			f.Status = engine.Absent
			return f, nil
		}
		f.Status, f.Reason = engine.Satisfied, "setup never uninstalls "+name
		if recorded {
			f.Commands = []engine.CommandBlock{p.removalBlock(pin, snapshot)}
			f.Notes = []string{"Personal data and a t3 command that the application installed stay"}
		}
		return f, nil
	}
	block := []engine.CommandBlock{{Title: name + " " + pin.Version + " (signed " + pin.Publisher + " installer for the current user)", Lines: []string{filepath.Base(pin.URL) + " " + windowsAppInstallerArguments}, By: "setup"}}
	switch {
	case !snapshot.Present:
		f.Status, f.Reason, f.Commands = engine.Missing, "run the "+pin.Publisher+" signed installer silently for the current user; it adds Start menu and desktop shortcuts; the app updates itself; personal data stays untouched", block
	case snapshot.Healthy:
		f.Status = engine.Satisfied
		if snapshot.Version != pin.Version {
			f.Notes = []string{name + " updates itself (installed " + snapshot.Version + ", pinned " + pin.Version + "); setup requests no change"}
		}
	case !recorded || !p.managed(pin, snapshot):
		f.Status, f.Reason = engine.Blocked, "preserve the existing "+name+" installation, which setup did not install: "+vendorOneLine(snapshot.HealthIssue)
	case vendorCompareVersions(snapshot.Version, pin.Version) > 0:
		f.Status, f.Reason = engine.Blocked, name+" is not usable ("+vendorOneLine(snapshot.HealthIssue)+") and newer than the reviewed pin (installed "+snapshot.Version+", pinned "+pin.Version+"); setup declines the downgrade: install its current release yourself"
	case snapshot.Running:
		f.Status, f.Reason = engine.Blocked, "quit "+name+" before setup repairs its installation: "+vendorOneLine(snapshot.HealthIssue)
	default:
		f.Status, f.Reason, f.Commands = engine.Unhealthy, vendorOneLine(snapshot.HealthIssue), block
	}
	return f, nil
}

// Apply installs or repairs the application with the reviewed installer. It
// records the pending mark and the provisional system record before the
// installer starts, and clears the mark after the registration and the
// signatures pass.
func (p *WindowsApps) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	if p.Issue != "" {
		return errors.New(p.Issue)
	}
	pin, err := p.validate(r)
	if err != nil {
		return err
	}
	name := windowsAppName(pin)
	if p.Run == nil {
		return errors.New("the " + name + " installer needs the foreground command runner")
	}
	before, err := p.query(ctx, pin)
	if err != nil {
		return err
	}
	action := map[engine.Status]string{engine.Missing: "install", engine.Unhealthy: "repair"}[approved.Status]
	recorded := rec.State().HasSystem(vendorSystemKey, vendorRecordName(r))
	if action == "" || action == "install" && before.Present || action == "repair" && (!before.Present || !recorded || !p.managed(pin, before)) {
		return errors.New(name + " changed after the review, or setup did not install it; review a new plan")
	}
	if action == "repair" && vendorCompareVersions(before.Version, pin.Version) > 0 {
		return fmt.Errorf("%s %s is newer than the reviewed pin %s; setup declines the downgrade", name, before.Version, pin.Version)
	}
	if before.Running {
		return errors.New("quit " + name + " before setup changes its installation")
	}
	if err := vendorPrepareInstaller(ctx, p.Fetch, p.Directory, pin.URL, pin.SHA256, pin.Size); err != nil {
		return err
	}
	input, err := p.input(pin, map[string]string{"Action": action, "File": vendorInstallerPath(p.Directory, pin.SHA256), "SHA256": pin.SHA256, "Arguments": windowsAppInstallerArguments})
	if err != nil {
		return err
	}
	return vendorChange(rec, r, action, func() (int, bool, error) {
		return vendorRun(ctx, p.Run, p.PowerShell, windowsAppChangeScript, json.RawMessage(input), false)
	}, func(code int, cause error) error {
		if code != 0 {
			failure := &platform.CommandError{Message: fmt.Sprintf("%s setup failed with exit code %d", name, code), ExitCode: &code}
			return fmt.Errorf("%w: %s", failure, vendorOneLine(cause.Error()))
		}
		after, err := p.query(ctx, pin)
		if err != nil {
			return err
		}
		if !after.Present || !after.Healthy || !p.managed(pin, after) || after.Version != pin.Version {
			return fmt.Errorf("the vendor installer did not leave the signed per-user %s %s: %+v", name, pin.Version, after)
		}
		return nil
	})
}

// Remove never uninstalls an application. It deletes the system record only
// when the application is absent.
func (p *WindowsApps) Remove(ctx context.Context, r catalog.Resource, _ engine.Finding, rec engine.Records) error {
	if p.Issue != "" {
		return errors.New(p.Issue)
	}
	pin, err := p.validate(r)
	if err != nil {
		return err
	}
	snapshot, err := p.query(ctx, pin)
	if err != nil {
		return err
	}
	return vendorForget(rec, r, snapshot.Present)
}
