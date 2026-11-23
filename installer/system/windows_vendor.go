package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/platform"
)

// WindowsVendor installs the Microsoft VC++ x64 runtime (tool.vcredist) with
// the reviewed, signed Microsoft bundle. Directory holds the verified
// installer and its logs. Query is read-only PowerShell. Run starts the
// installer in the foreground, and Windows shows the elevation prompt. Issue
// is a discovery failure of the wiring: it makes each inspection unknown.
// The provider reuses a healthy runtime, changes a present runtime only when
// the state has its record, declines a downgrade and never uninstalls.
type WindowsVendor struct {
	Directory, PowerShell string
	Pin                   catalog.WindowsVendorPin
	Query                 VendorQuery
	Run                   platform.Runner
	Fetch                 VendorFetch
	Issue                 string
}

// NewWindowsVendor returns the provider of tool.vcredist. It only reads its
// arguments. The provider keeps its files in <stateDir>/windows-vendor.
func NewWindowsVendor(stateDir, powershell string, pin catalog.WindowsVendorPin, run platform.Runner, fetch VendorFetch) *WindowsVendor {
	return &WindowsVendor{Directory: filepath.Join(stateDir, "windows-vendor"), PowerShell: powershell, Pin: pin, Query: QueryWindowsVendor, Run: run, Fetch: fetch}
}

const windowsVendorProduct = "Microsoft VC++ runtime"

var windowsVendorVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
var windowsVendorGUID = regexp.MustCompile(`^\{[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}\}$`)
var windowsVendorSHA256 = regexp.MustCompile(`^[a-f0-9]{64}$`)

type windowsVendorSnapshot struct {
	Present     bool              `json:"present"`
	Healthy     bool              `json:"healthy"`
	Version     string            `json:"version"`
	BundleID    string            `json:"bundle_id"`
	Packages    map[string]string `json:"packages"`
	Files       map[string]string `json:"files"`
	Consumers   []string          `json:"consumers"`
	HealthIssue string            `json:"health_issue"`
}

func (p *WindowsVendor) validate(r catalog.Resource) error {
	if r.ID != "tool.vcredist" || p.Query == nil || !vendorCanonical(p.PowerShell, p.Directory) {
		return errors.New("invalid Windows VC++ vendor provider boundary")
	}
	if err := vendorUnredirected(p.Directory); err != nil {
		return err
	}
	return p.Pin.Validate()
}

func (p *WindowsVendor) query(ctx context.Context) (windowsVendorSnapshot, error) {
	var snapshot windowsVendorSnapshot
	output, err := p.Query(ctx, p.PowerShell, nil, windowsVendorArguments(windowsVendorObserveScript)...)
	if err != nil {
		return snapshot, fmt.Errorf("inspect registered Microsoft VC++ runtime: %w", err)
	}
	if len(output) > 1<<20 {
		return snapshot, errors.New("Windows vendor inspection exceeded its bound")
	}
	if err := platform.Decode(output, &snapshot); err != nil {
		return snapshot, err
	}
	if len(snapshot.Packages) > 2 || len(snapshot.Files) > 3 || len(snapshot.Consumers) > 1024 || len(snapshot.HealthIssue) > 4096 ||
		snapshot.Present && !windowsVendorVersion.MatchString(snapshot.Version) || snapshot.BundleID != "" && !windowsVendorGUID.MatchString(snapshot.BundleID) || snapshot.Healthy && (!snapshot.Present || len(snapshot.Files) != 3) {
		return snapshot, errors.New("invalid registered VC++ runtime inspection")
	}
	for name, id := range snapshot.Packages {
		if !slices.Contains([]string{"minimum", "additional"}, name) || !windowsVendorGUID.MatchString(id) {
			return snapshot, errors.New("invalid registered VC++ runtime package identity")
		}
	}
	for name, hash := range snapshot.Files {
		if !slices.Contains([]string{"vcruntime140.dll", "vcruntime140_1.dll", "msvcp140.dll"}, name) || !windowsVendorSHA256.MatchString(hash) {
			return snapshot, errors.New("invalid VC++ runtime file identity")
		}
	}
	for _, consumer := range snapshot.Consumers {
		if consumer == "" || len(consumer) > 512 || strings.ContainsAny(consumer, "\x00\r\n") {
			return snapshot, errors.New("invalid registered VC++ runtime dependent")
		}
	}
	slices.Sort(snapshot.Consumers)
	snapshot.Consumers = slices.Compact(snapshot.Consumers)
	return snapshot, nil
}

func (p *WindowsVendor) log(action string) string {
	return filepath.Join(p.Directory, "logs", "vcredist-"+action+".log")
}

func windowsVendorInstallerArguments(action, log string) (string, error) {
	verb := map[string]string{"install": "/install", "update": "/install", "repair": "/repair"}[action]
	if verb == "" || strings.ContainsAny(log, "\"\x00\r\n") {
		return "", errors.New("unknown VC++ runtime action or invalid log path")
	}
	return strings.Join([]string{verb, "/quiet", "/norestart", "/log", `"` + log + `"`}, " "), nil
}

func (p *WindowsVendor) action(status engine.Status, snapshot windowsVendorSnapshot) string {
	switch status {
	case engine.Missing:
		return "install"
	case engine.Outdated:
		return "update"
	case engine.Unhealthy:
		if snapshot.BundleID == p.Pin.BundleID {
			return "repair"
		}
		return "update"
	}
	return ""
}

func (p *WindowsVendor) setupBlock(action string) []engine.CommandBlock {
	arguments, err := windowsVendorInstallerArguments(action, p.log(action))
	if err != nil {
		return nil
	}
	return []engine.CommandBlock{{Title: windowsVendorProduct + " " + p.Pin.Version + " (signed Microsoft installer, with elevation)", Lines: []string{"VC_redist.x64.exe " + arguments}, By: "setup"}}
}

func (p *WindowsVendor) removalBlock(snapshot windowsVendorSnapshot) engine.CommandBlock {
	block := engine.CommandBlock{Title: "Remove the " + windowsVendorProduct + " " + snapshot.Version + " yourself; examine first which applications use it", By: "you",
		Lines: []string{"Settings > Apps > Installed apps > Microsoft Visual C++ 2015-2022 Redistributable (x64) > Uninstall"}}
	file := vendorInstallerPath(p.Directory, p.Pin.SHA256)
	if snapshot.BundleID == p.Pin.BundleID && snapshot.Version == p.Pin.Version && vendorInstallerReady(file, p.Pin.SHA256, p.Pin.Size) == nil {
		block.Lines = []string{"& " + vendorPowerShellLiteral(file) + " /uninstall /norestart"}
	}
	return block
}

// Inspect reads the registered runtime and loads its libraries. It reports
// the installed and the pinned version. For a runtime that is not wanted, it
// returns the manual removal route only when the state has its record.
func (p *WindowsVendor) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	if p.Issue != "" {
		return engine.Finding{Status: engine.Unknown, Reason: p.Issue}, nil
	}
	if err := p.validate(r); err != nil {
		return engine.Finding{}, err
	}
	snapshot, err := p.query(ctx)
	if err != nil {
		return vendorUnknown(ctx, "the "+windowsVendorProduct, err)
	}
	recorded := rec.State().HasSystem(vendorSystemKey, vendorRecordName(r))
	f := engine.Finding{Version: snapshot.Version, Privileged: true, Pending: true}
	f.Token, err = vendorDigest(struct {
		Recorded, Present, Healthy bool
		Version, Bundle, Pin       string
		Packages, Files            map[string]string
	}{recorded, snapshot.Present, snapshot.Healthy, snapshot.Version, snapshot.BundleID, p.Pin.Version, snapshot.Packages, snapshot.Files})
	if err != nil {
		return f, err
	}
	versions := "installed " + snapshot.Version + ", pinned " + p.Pin.Version
	if !want.Present {
		if !snapshot.Present {
			f.Status = engine.Absent
			return f, nil
		}
		f.Status, f.Reason = engine.Satisfied, "setup never uninstalls the "+windowsVendorProduct
		if recorded {
			f.Commands = []engine.CommandBlock{p.removalBlock(snapshot)}
			f.Notes = append([]string{"Manual and other-user applications can use the shared " + windowsVendorProduct}, snapshot.Consumers...)
		}
		return f, nil
	}
	order := vendorCompareVersions(snapshot.Version, p.Pin.Version)
	switch {
	case !snapshot.Present:
		f.Status, f.Reason = engine.Missing, "install the "+windowsVendorProduct+" "+p.Pin.Version
	case !snapshot.Healthy && order > 0:
		f.Status, f.Reason = engine.Blocked, "the "+windowsVendorProduct+" is not usable ("+vendorOneLine(snapshot.HealthIssue)+") and newer than the reviewed pin ("+versions+"); setup declines the downgrade: repair it in Settings > Apps"
	case !snapshot.Healthy && !recorded:
		f.Status, f.Reason = engine.Blocked, "preserve the existing "+windowsVendorProduct+", which setup did not install: "+vendorOneLine(snapshot.HealthIssue)+"; repair it in Settings > Apps"
	case !snapshot.Healthy:
		f.Status, f.Reason = engine.Unhealthy, vendorOneLine(snapshot.HealthIssue)+" ("+versions+"); restart Windows first if its installer asked for a restart"
	case order > 0:
		f.Status = engine.Satisfied
		f.Notes = []string{"The " + windowsVendorProduct + " is newer than the reviewed pin (" + versions + "); setup keeps it and declines the downgrade"}
	case order < 0 && recorded:
		f.Status, f.Reason = engine.Outdated, versions
	case order < 0:
		f.Status = engine.Satisfied
		f.Notes = []string{"The " + windowsVendorProduct + " is older than the reviewed pin (" + versions + "); setup did not install it and requests no change"}
	default:
		f.Status = engine.Satisfied
	}
	if action := p.action(f.Status, snapshot); action != "" {
		f.Commands = p.setupBlock(action)
	}
	return f, nil
}

// Apply installs, updates or repairs the runtime with the reviewed bundle.
// It records the pending mark and the provisional system record before the
// installer starts, and clears the mark after the live probe passes. Exit
// code 3010 is complete only when the probe passes.
func (p *WindowsVendor) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	if p.Issue != "" {
		return errors.New(p.Issue)
	}
	if err := p.validate(r); err != nil {
		return err
	}
	if p.Run == nil {
		return errors.New("the " + windowsVendorProduct + " installer needs the foreground command runner")
	}
	before, err := p.query(ctx)
	if err != nil {
		return err
	}
	action := p.action(approved.Status, before)
	recorded := rec.State().HasSystem(vendorSystemKey, vendorRecordName(r))
	if action == "" || action == "install" && before.Present || action != "install" && (!before.Present || !recorded) {
		return errors.New("the " + windowsVendorProduct + " changed after the review, or setup did not install it; review a new plan")
	}
	if action != "install" && vendorCompareVersions(before.Version, p.Pin.Version) > 0 {
		return fmt.Errorf("the %s %s is newer than the reviewed pin %s; setup declines the downgrade", windowsVendorProduct, before.Version, p.Pin.Version)
	}
	log := p.log(action)
	arguments, err := windowsVendorInstallerArguments(action, log)
	if err != nil {
		return err
	}
	if err := vendorPrepareInstaller(ctx, p.Fetch, p.Directory, p.Pin.URL, p.Pin.SHA256, p.Pin.Size); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(log), 0700); err != nil {
		return err
	}
	input := struct {
		Action, File, SHA256, Arguments string
		Before                          windowsVendorSnapshot
	}{action, vendorInstallerPath(p.Directory, p.Pin.SHA256), p.Pin.SHA256, arguments, before}
	return vendorChange(rec, r, action, func() (int, bool, error) {
		return vendorRun(ctx, p.Run, p.PowerShell, windowsVendorInstallScript, input, true)
	}, func(code int, cause error) error {
		if !vendorSucceeded(code) && code != 1638 {
			return vendorFailure(windowsVendorProduct, code, log, cause)
		}
		after, err := p.query(ctx)
		if err != nil {
			return err
		}
		if code == 1638 {
			if after.Present && after.Healthy && vendorCompareVersions(after.Version, p.Pin.Version) > 0 {
				engine.ReportDetail(ctx, "The %s %s is newer than the reviewed pin %s; setup keeps it.", windowsVendorProduct, after.Version, p.Pin.Version)
				return nil
			}
			return vendorFailure(windowsVendorProduct, code, log, cause)
		}
		if !after.Present || after.BundleID != p.Pin.BundleID || after.Version != p.Pin.Version || len(after.Packages) != 2 {
			return fmt.Errorf("vendor installer did not leave the exact registered %s (installed %q, pinned %s); inspect %s", windowsVendorProduct, after.Version, p.Pin.Version, log)
		}
		if !after.Healthy {
			if code != 0 {
				return errors.New("the " + windowsVendorProduct + " setup needs a restart of Windows; restart, then run setup again")
			}
			return fmt.Errorf("vendor installer did not leave a usable %s: %s; inspect %s", windowsVendorProduct, vendorOneLine(after.HealthIssue), log)
		}
		if code != 0 {
			engine.ReportDetail(ctx, "The %s is usable. Its installer asked for a restart of Windows.", windowsVendorProduct)
		}
		return nil
	})
}

// Remove never uninstalls the runtime. It deletes the system record only when
// the runtime is absent.
func (p *WindowsVendor) Remove(ctx context.Context, r catalog.Resource, _ engine.Finding, rec engine.Records) error {
	if p.Issue != "" {
		return errors.New(p.Issue)
	}
	if err := p.validate(r); err != nil {
		return err
	}
	snapshot, err := p.query(ctx)
	if err != nil {
		return err
	}
	return vendorForget(rec, r, snapshot.Present)
}
