package system

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

type windowsAppFixture struct {
	provider *WindowsApps
	resource catalog.Resource
	rec      vendorTestRecords
	snapshot windowsAppSnapshot
	runner   *vendorTestRunner
	payload  *vendorTestPayload
	inputs   []map[string]any
}

func newWindowsAppFixture(t *testing.T) *windowsAppFixture {
	t.Helper()
	root, rec := vendorTestRoot(t)
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	payload := newVendorTestPayload("reviewed T3 Code installer fixture")
	pin := catalog.WindowsAppPin{Version: "0.1.7", URL: "https://github.com/pingdotgg/t3code/releases/download/v0.1.7/T3-Code-0.1.7-x64.exe", SHA256: payload.sha(), Size: int64(len(payload.data)),
		ProductCode: "e9197887-efb3-55e0-985e-d6d3b5dd594a", Publisher: "T3 Tools Inc", Folder: "t3code", Executable: "T3 Code (Beta).exe", Uninstaller: "Uninstall T3 Code (Beta).exe"}
	f := &windowsAppFixture{resource: catalog.Resource{ID: "app.t3-code-windows", Name: "T3 Code", Action: "package", Scope: "user", Bindings: map[string]catalog.Binding{"windows": {Provider: "windows-app", Package: "t3-code"}}}, rec: rec, payload: payload}
	f.runner = &vendorTestRunner{t: t}
	f.provider = NewWindowsApps(filepath.Join(root, "state"), program, filepath.Join(root, "Local"), map[string]catalog.WindowsAppPin{f.resource.ID: pin}, f.runner, payload.fetch)
	f.provider.Query = func(_ context.Context, program string, input []byte, args ...string) ([]byte, error) {
		var fields map[string]string
		if program != f.provider.PowerShell || json.Unmarshal(input, &fields) != nil || fields["ProductCode"] != pin.ProductCode || fields["Action"] != "" || len(args) != 5 || args[3] != "-EncodedCommand" {
			t.Fatal("inspection widened its process boundary")
		}
		return json.Marshal(f.snapshot)
	}
	f.runner.started = func(command platform.Command, input map[string]any) {
		if command.Path != f.provider.PowerShell || command.Elevated {
			t.Fatal("the per-user installer did not run through system PowerShell without elevation")
		}
		st := f.rec.State()
		if !slices.ContainsFunc(st.Pending, func(mark string) bool { return strings.HasPrefix(mark, "app.t3-code-windows:") }) || !st.HasSystem("windows", "t3-code") {
			t.Fatal("the installer started before its pending mark and system record", st.Pending, st.System)
		}
		if data, err := os.ReadFile(input["File"].(string)); err != nil || string(data) != string(payload.data) {
			t.Fatal("the installer started without its verified payload", err)
		}
		f.inputs = append(f.inputs, input)
		if f.runner.exit == 0 {
			f.installed()
		}
	}
	return f
}

func (f *windowsAppFixture) installed() {
	pin := f.provider.Pins[f.resource.ID]
	f.snapshot = windowsAppSnapshot{Present: true, Healthy: true, Version: pin.Version, Scope: "user", Location: filepath.Join(f.provider.Programs, pin.Folder)}
}

func (f *windowsAppFixture) inspect(t *testing.T, want engine.Want) engine.Finding {
	t.Helper()
	finding, err := f.provider.Inspect(context.Background(), f.resource, want, f.rec)
	if err != nil {
		t.Fatal(err)
	}
	return finding
}

func (f *windowsAppFixture) owned(t *testing.T) {
	t.Helper()
	finding := f.inspect(t, engine.Want{Present: true})
	if finding.Status != engine.Missing || !finding.Pending || finding.Privileged || len(vendorTestBlocks(finding, "setup")) != 1 || finding.Commands[0].Lines[0] != "T3-Code-0.1.7-x64.exe /S" {
		t.Fatal(finding)
	}
	if err := f.provider.Apply(context.Background(), f.resource, finding, f.rec); err != nil {
		t.Fatal(err)
	}
	st := f.rec.State()
	if len(st.Pending) != 0 || !st.HasSystem("windows", "t3-code") || st.HasSystem("windows", f.resource.ID) {
		t.Fatal("verified installation left a mark or recorded another name", st.Pending, st.System)
	}
}

func TestWindowsAppInstallsWithTheReviewedInstallerAndKeepsASelfUpdate(t *testing.T) {
	f := newWindowsAppFixture(t)
	f.owned(t)
	if f.runner.runs != 1 || f.inputs[0]["Action"] != "install" || f.inputs[0]["Arguments"] != "/S" || f.inputs[0]["SHA256"] != f.payload.sha() || f.inputs[0]["Programs"] != f.provider.Programs {
		t.Fatal("installer input changed", f.inputs[0])
	}
	before := f.inspect(t, engine.Want{Present: true})
	f.snapshot.Version = "0.1.9"
	for _, want := range []engine.Want{{Present: true}, {Present: true, Update: true}} {
		finding := f.inspect(t, want)
		if finding.Status != engine.Satisfied || finding.Version != "0.1.9" || finding.Token != before.Token || len(finding.Commands) != 0 || len(finding.Notes) != 1 || !strings.Contains(finding.Notes[0], "installed 0.1.9, pinned 0.1.7") {
			t.Fatalf("a vendor update changed the application or asked for a change: %+v", finding)
		}
	}
	if err := f.provider.Apply(context.Background(), f.resource, engine.Finding{Status: engine.Outdated}, f.rec); err == nil || f.runner.runs != 1 {
		t.Fatal("the pinned installer ran over a self-updated application", err)
	}
}

func TestWindowsAppRemovalRouteOnlyForARecordedAndUnneededApplication(t *testing.T) {
	f := newWindowsAppFixture(t)
	f.installed()
	if finding := f.inspect(t, engine.Want{}); finding.Status != engine.Satisfied || len(finding.Commands) != 0 {
		t.Fatal("offered the removal of an unrecorded application", finding)
	}
	f.rec.record(t, "t3-code")
	if finding := f.inspect(t, engine.Want{Present: true}); len(vendorTestBlocks(finding, "you")) != 0 {
		t.Fatal("offered the removal of a needed application", finding)
	}
	finding := f.inspect(t, engine.Want{})
	blocks := vendorTestBlocks(finding, "you")
	uninstaller := filepath.Join(f.provider.Programs, "t3code", "Uninstall T3 Code (Beta).exe")
	if finding.Status != engine.Satisfied || len(blocks) != 1 || len(finding.Commands) != 1 || blocks[0].Lines[0] != "& '"+uninstaller+"' /currentuser" || len(finding.Notes) != 1 {
		t.Fatal(finding)
	}
	for name, change := range map[string]func(*windowsAppSnapshot){
		"moved installation":   func(s *windowsAppSnapshot) { s.Location = filepath.Join(f.provider.Programs, "Elsewhere") },
		"machine installation": func(s *windowsAppSnapshot) { s.Scope = "machine" },
		"unsigned files": func(s *windowsAppSnapshot) {
			s.Healthy, s.HealthIssue = false, "The file is not signed by T3 Tools Inc"
		},
	} {
		f.installed()
		change(&f.snapshot)
		if blocks := vendorTestBlocks(f.inspect(t, engine.Want{}), "you"); len(blocks) != 1 || !strings.HasPrefix(blocks[0].Lines[0], "Settings > Apps") {
			t.Fatal("an unverified installation got the uninstaller command: "+name, blocks)
		}
	}
	if err := f.provider.Remove(context.Background(), f.resource, finding, f.rec); err != nil || f.runner.runs != 0 || !f.rec.State().HasSystem("windows", "t3-code") {
		t.Fatal("removal changed a present application or its record", err)
	}
	f.snapshot = windowsAppSnapshot{}
	if finding := f.inspect(t, engine.Want{}); finding.Status != engine.Absent || len(finding.Commands) != 0 {
		t.Fatal(finding)
	}
	if err := f.provider.Remove(context.Background(), f.resource, finding, f.rec); err != nil || f.runner.runs != 0 || f.rec.State().HasSystem("windows", "t3-code") {
		t.Fatal("an absent application kept its record", err)
	}
}

func TestWindowsAppPreservesInstallationsThatItDidNotInstall(t *testing.T) {
	for name, prepare := range map[string]func(*windowsAppFixture){
		"pre-existing install": func(f *windowsAppFixture) {
			if err := f.rec.forget("t3-code"); err != nil {
				panic(err)
			}
		},
		"moved installation":   func(f *windowsAppFixture) { f.snapshot.Location = filepath.Join(f.provider.Programs, "Elsewhere") },
		"machine installation": func(f *windowsAppFixture) { f.snapshot.Scope = "machine" },
		"running application":  func(f *windowsAppFixture) { f.snapshot.Running = true },
		"newer than the pin":   func(f *windowsAppFixture) { f.snapshot.Version = "0.1.9" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newWindowsAppFixture(t)
			f.owned(t)
			f.snapshot.Healthy, f.snapshot.HealthIssue = false, "The file is not signed by T3 Tools Inc"
			prepare(f)
			finding := f.inspect(t, engine.Want{Present: true})
			if finding.Status != engine.Blocked || !strings.Contains(finding.Reason, "not signed by T3 Tools Inc") || len(finding.Commands) != 0 {
				t.Fatal(finding)
			}
			if err := f.provider.Apply(context.Background(), f.resource, engine.Finding{Status: engine.Unhealthy}, f.rec); err == nil || f.runner.runs != 1 {
				t.Fatal("repair ran the installer over an installation that setup must keep", err)
			}
			if len(vendorTestPending(f.rec)) != 0 {
				t.Fatal("a refused repair left a pending mark")
			}
		})
	}
	f := newWindowsAppFixture(t)
	f.installed()
	if finding := f.inspect(t, engine.Want{Present: true}); finding.Status != engine.Satisfied || len(finding.Commands) != 0 {
		t.Fatal(finding)
	}
	if err := f.provider.Apply(context.Background(), f.resource, engine.Finding{Status: engine.Missing}, f.rec); err == nil || f.runner.runs != 0 || f.rec.State().HasSystem("windows", "t3-code") {
		t.Fatal("install ran over a pre-existing application", err)
	}
}

func TestWindowsAppRepairsARecordedApplication(t *testing.T) {
	f := newWindowsAppFixture(t)
	f.owned(t)
	f.snapshot.Healthy, f.snapshot.HealthIssue = false, "The application files have no uninstall registration"
	finding := f.inspect(t, engine.Want{Present: true})
	if finding.Status != engine.Unhealthy || len(vendorTestBlocks(finding, "setup")) != 1 {
		t.Fatal(finding)
	}
	if err := f.provider.Apply(context.Background(), f.resource, finding, f.rec); err != nil {
		t.Fatal(err)
	}
	if f.runner.runs != 2 || f.inputs[1]["Action"] != "repair" || f.inputs[1]["Arguments"] != "/S" || len(vendorTestPending(f.rec)) != 0 {
		t.Fatal(f.inputs[1])
	}
}

func TestWindowsAppRefusesChangedInstallerBytes(t *testing.T) {
	f := newWindowsAppFixture(t)
	f.payload.served = []byte("changed T3 Code installer fixture!")
	err := f.provider.Apply(context.Background(), f.resource, f.inspect(t, engine.Want{Present: true}), f.rec)
	st := f.rec.State()
	if err == nil || f.runner.runs != 0 || len(st.Pending) != 0 || st.HasSystem("windows", "t3-code") {
		t.Fatal("installer ran without its reviewed SHA-256", err)
	}
}

func TestWindowsAppExitCodesDecideThePendingLifecycle(t *testing.T) {
	for _, sample := range []struct {
		name             string
		exit             int
		fail             error
		marked, recorded bool
	}{
		{"installer failure", 2, nil, true, true},
		{"check before the start failed", vendorNotStartedExit, nil, false, false},
		{"PowerShell did not start", 0, errors.New("fork failed"), false, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			f := newWindowsAppFixture(t)
			f.runner.exit, f.runner.fail = sample.exit, sample.fail
			err := f.provider.Apply(context.Background(), f.resource, f.inspect(t, engine.Want{Present: true}), f.rec)
			if err == nil {
				t.Fatal("failed installer was not reported")
			}
			if code, ok := platform.ExitCode(err); sample.exit != 0 && (!ok || code != sample.exit) {
				t.Fatal("the failure lost its exit code", err)
			}
			st := f.rec.State()
			if slices.Contains(st.Pending, "app.t3-code-windows:install") != sample.marked || st.HasSystem("windows", "t3-code") != sample.recorded {
				t.Fatal("pending mark or provisional record differs", st.Pending, st.System)
			}
			if finding := f.inspect(t, engine.Want{Present: true}); finding.Status != engine.Missing {
				t.Fatalf("failed installer claimed completion: %+v", finding)
			}
		})
	}
	f := newWindowsAppFixture(t)
	started := f.runner.started
	f.runner.started = func(command platform.Command, input map[string]any) {
		started(command, input)
		f.snapshot.Version = "0.1.6"
	}
	if err := f.provider.Apply(context.Background(), f.resource, f.inspect(t, engine.Want{Present: true}), f.rec); err == nil || !slices.Contains(vendorTestPending(f.rec), "app.t3-code-windows:install") {
		t.Fatal("an installation of another version passed verification", err)
	}
}

func TestWindowsAppInspectionParsing(t *testing.T) {
	for name, change := range map[string]func(*windowsAppSnapshot){
		"scope":                    func(s *windowsAppSnapshot) { s.Scope = "system" },
		"version":                  func(s *windowsAppSnapshot) { s.Version = "v0.1.7" },
		"relative location":        func(s *windowsAppSnapshot) { s.Location = "t3code" },
		"healthy without location": func(s *windowsAppSnapshot) { s.Location = "" },
		"healthy without scope":    func(s *windowsAppSnapshot) { s.Scope = "" },
		"healthy and absent":       func(s *windowsAppSnapshot) { s.Present = false },
		"long issue":               func(s *windowsAppSnapshot) { s.HealthIssue = strings.Repeat("a", 513) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newWindowsAppFixture(t)
			f.installed()
			change(&f.snapshot)
			if _, err := f.provider.query(context.Background(), f.provider.Pins[f.resource.ID]); err == nil {
				t.Fatal("accepted an invalid inspection")
			}
			finding := f.inspect(t, engine.Want{Present: true})
			if finding.Status != engine.Unknown || len(finding.Commands) != 0 {
				t.Fatal("an invalid inspection proved a state", finding)
			}
			if err := f.provider.Remove(context.Background(), f.resource, finding, f.rec); err == nil {
				t.Fatal("an invalid inspection proved absence")
			}
		})
	}
	f := newWindowsAppFixture(t)
	other := f.resource
	other.ID = "app.other"
	if _, err := f.provider.Inspect(context.Background(), other, engine.Want{Present: true}, f.rec); err == nil {
		t.Fatal("accepted an application without a reviewed pin")
	}
	unbound := f.resource
	unbound.Bindings = nil
	if _, err := f.provider.Inspect(context.Background(), unbound, engine.Want{Present: true}, f.rec); err == nil {
		t.Fatal("accepted an application without its windows-app binding")
	}
	pin := f.provider.Pins[f.resource.ID]
	pin.Publisher = ""
	f.provider.Pins[f.resource.ID] = pin
	if _, err := f.provider.Inspect(context.Background(), f.resource, engine.Want{Present: true}, f.rec); err == nil {
		t.Fatal("accepted a pin without its publisher")
	}
}
