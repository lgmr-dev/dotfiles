package system

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/platform"
)

const windowsVendorTestMark = "tool.vcredist:install"

type windowsVendorFixture struct {
	provider  *WindowsVendor
	resource  catalog.Resource
	rec       vendorTestRecords
	snapshot  windowsVendorSnapshot
	runner    *vendorTestRunner
	payload   *vendorTestPayload
	inputs    []map[string]any
	unhealthy bool
	leaves    string
}

func newWindowsVendorFixture(t *testing.T) *windowsVendorFixture {
	t.Helper()
	root, rec := vendorTestRoot(t)
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	payload := newVendorTestPayload("reviewed Microsoft installer fixture")
	pin := catalog.WindowsVendorPin{Version: "14.51.36247.0", URL: "https://download.visualstudio.microsoft.com/fixture/VC_redist.x64.exe", SHA256: payload.sha(), Size: int64(len(payload.data)), BundleID: "{0e3bb569-69d6-4c34-bff9-c2f81db5e5f0}"}
	f := &windowsVendorFixture{resource: catalog.Resource{ID: "tool.vcredist", Name: "Microsoft Visual C++ runtime", Action: "vcredist", Scope: "machine", Shared: true}, rec: rec, payload: payload, leaves: pin.Version}
	f.runner = &vendorTestRunner{t: t}
	f.provider = NewWindowsVendor(filepath.Join(root, "state"), program, pin, f.runner, payload.fetch)
	f.provider.Query = func(_ context.Context, program string, input []byte, args ...string) ([]byte, error) {
		if program != f.provider.PowerShell || len(input) != 0 || len(args) != 5 || args[3] != "-EncodedCommand" {
			t.Fatal("inspection widened its process boundary")
		}
		return json.Marshal(f.snapshot)
	}
	f.runner.started = func(command platform.Command, input map[string]any) {
		if command.Path != f.provider.PowerShell || !command.Elevated {
			t.Fatal("the installer did not run through system PowerShell with its elevation declared")
		}
		st := f.rec.State()
		if !slices.ContainsFunc(st.Pending, func(mark string) bool { return strings.HasPrefix(mark, "tool.vcredist:") }) || !st.HasSystem("windows", "tool.vcredist") {
			t.Fatal("the installer started before its pending mark and system record", st.Pending, st.System)
		}
		if data, err := os.ReadFile(input["File"].(string)); err != nil || string(data) != string(payload.data) {
			t.Fatal("the installer started without its verified payload", err)
		}
		f.inputs = append(f.inputs, input)
		if f.runner.exit != vendorDeniedExit && f.runner.exit != vendorNotStartedExit && f.runner.exit != 1638 && f.runner.exit != 1603 {
			f.installed(f.leaves)
			f.snapshot.Healthy = !f.unhealthy
		}
	}
	return f
}

func (f *windowsVendorFixture) installed(version string) {
	f.snapshot = windowsVendorSnapshot{Present: true, Healthy: true, Version: version, BundleID: f.provider.Pin.BundleID,
		Packages: map[string]string{"minimum": "{11111111-1111-1111-1111-111111111111}", "additional": "{22222222-2222-2222-2222-222222222222}"},
		Files:    map[string]string{"vcruntime140.dll": strings.Repeat("1", 64), "vcruntime140_1.dll": strings.Repeat("2", 64), "msvcp140.dll": strings.Repeat("3", 64)}}
}

func (f *windowsVendorFixture) inspect(t *testing.T, want engine.Want) engine.Finding {
	t.Helper()
	finding, err := f.provider.Inspect(context.Background(), f.resource, want, f.rec)
	if err != nil {
		t.Fatal(err)
	}
	return finding
}

func (f *windowsVendorFixture) apply(t *testing.T) error {
	t.Helper()
	return f.provider.Apply(context.Background(), f.resource, f.inspect(t, engine.Want{Present: true, Update: true}), f.rec)
}

func TestWindowsVendorReusesPreexistingRuntimeWithoutStateOrRecord(t *testing.T) {
	f := newWindowsVendorFixture(t)
	f.installed(f.provider.Pin.Version)
	finding := f.inspect(t, engine.Want{Present: true})
	if finding.Status != engine.Satisfied || finding.Version != f.provider.Pin.Version || len(finding.Commands) != 0 {
		t.Fatal(finding)
	}
	if err := f.provider.Apply(context.Background(), f.resource, engine.Finding{Status: engine.Missing}, f.rec); err == nil {
		t.Fatal("installed over a pre-existing runtime")
	}
	if f.runner.runs != 0 || f.payload.downloads != 0 || f.rec.State().HasSystem("windows", "tool.vcredist") || len(vendorTestPending(f.rec)) != 0 {
		t.Fatal("pre-existing runtime caused installation or a record")
	}
	if _, err := os.Stat(f.provider.Directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inspection created state", err)
	}
}

func TestWindowsVendorInstallMarksBeforeTheStartAndClearsAfterTheProbe(t *testing.T) {
	f := newWindowsVendorFixture(t)
	finding := f.inspect(t, engine.Want{Present: true})
	blocks := vendorTestBlocks(finding, "setup")
	if finding.Status != engine.Missing || !finding.Pending || !finding.Privileged || len(blocks) != 1 || len(vendorTestBlocks(finding, "you")) != 0 ||
		!strings.HasPrefix(blocks[0].Lines[0], "VC_redist.x64.exe /install /quiet /norestart /log ") {
		t.Fatal(finding)
	}
	if err := f.provider.Apply(context.Background(), f.resource, finding, f.rec); err != nil {
		t.Fatal(err)
	}
	st := f.rec.State()
	if f.runner.runs != 1 || len(st.Pending) != 0 || !st.HasSystem("windows", "tool.vcredist") {
		t.Fatal("verified installation left a mark or lost its record", st.Pending, st.System)
	}
	log := filepath.Join(f.provider.Directory, "logs", "vcredist-install.log")
	if f.inputs[0]["Action"] != "install" || f.inputs[0]["Arguments"] != `/install /quiet /norestart /log "`+log+`"` || f.inputs[0]["SHA256"] != f.payload.sha() {
		t.Fatal("installer input changed", f.inputs[0])
	}
	after := f.inspect(t, engine.Want{Present: true})
	if after.Status != engine.Satisfied || after.Token == finding.Token {
		t.Fatal(after)
	}
	if again := f.inspect(t, engine.Want{Present: true}); again.Token != after.Token {
		t.Fatal("an unchanged runtime changed its token")
	}
}

func TestWindowsVendorInstallerArgumentsHaveNoRemovalOrDowngrade(t *testing.T) {
	for action, want := range map[string]string{"install": `/install /quiet /norestart /log "C:\state\log.txt"`, "update": `/install /quiet /norestart /log "C:\state\log.txt"`, "repair": `/repair /quiet /norestart /log "C:\state\log.txt"`} {
		if got, err := windowsVendorInstallerArguments(action, `C:\state\log.txt`); err != nil || got != want {
			t.Fatal(action, got, err)
		}
	}
	for _, action := range []string{"remove", "uninstall", "downgrade", ""} {
		if got, err := windowsVendorInstallerArguments(action, `C:\state\log.txt`); err == nil {
			t.Fatal("unsupported action built installer arguments", action, got)
		}
	}
	if _, err := windowsVendorInstallerArguments("install", `C:\state\"quoted".txt`); err == nil {
		t.Fatal("a quote in the log path reached the installer command line")
	}
}

func TestWindowsVendorExitCodesDecideThePendingLifecycle(t *testing.T) {
	for _, sample := range []struct {
		name               string
		exit               int
		fail               error
		unhealthy          bool
		succeeds           bool
		marked, recorded   bool
		message            string
		newerAlreadyExists bool
	}{
		{name: "success", exit: 0, succeeds: true, recorded: true},
		{name: "restart required and usable", exit: 3010, succeeds: true, recorded: true},
		{name: "restart started and usable", exit: 1641, succeeds: true, recorded: true},
		{name: "restart required before use", exit: 3010, unhealthy: true, marked: true, recorded: true, message: "restart"},
		{name: "success without a usable runtime", exit: 0, unhealthy: true, marked: true, recorded: true, message: "usable"},
		{name: "fatal installer error", exit: 1603, marked: true, recorded: true, message: "exit code 1603"},
		{name: "another version", exit: 1638, marked: true, recorded: true, message: "does not downgrade"},
		{name: "denied elevation", exit: 1223, message: "UAC permission was denied"},
		{name: "check before the start failed", exit: vendorNotStartedExit, message: "did not start"},
		{name: "PowerShell did not start", fail: errors.New("fork failed"), message: "fork failed"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			f := newWindowsVendorFixture(t)
			f.runner.exit, f.runner.fail, f.unhealthy = sample.exit, sample.fail, sample.unhealthy
			err := f.apply(t)
			if (err == nil) != sample.succeeds || err != nil && !strings.Contains(err.Error(), sample.message) {
				t.Fatal(err)
			}
			st := f.rec.State()
			if slices.Contains(st.Pending, windowsVendorTestMark) != sample.marked || st.HasSystem("windows", "tool.vcredist") != sample.recorded || f.runner.runs != 1 {
				t.Fatal("pending mark or provisional record differs", st.Pending, st.System, f.runner.runs)
			}
			if code, ok := platform.ExitCode(err); sample.exit != 0 && !sample.succeeds && sample.message != "restart" && (!ok || code != sample.exit) {
				t.Fatal("the failure lost its exit code", err)
			}
		})
	}
}

func TestWindowsVendorDeniedUpdateKeepsTheEarlierRecord(t *testing.T) {
	f := newWindowsVendorFixture(t)
	f.installed("14.44.35211.0")
	f.rec.record(t, "tool.vcredist")
	f.runner.exit = vendorDeniedExit
	if err := f.apply(t); err == nil {
		t.Fatal("accepted denied elevation")
	}
	st := f.rec.State()
	if len(st.Pending) != 0 || !st.HasSystem("windows", "tool.vcredist") {
		t.Fatal("an unstarted update lost the record of the installed runtime", st.Pending, st.System)
	}
}

func TestWindowsVendorRejectsDamagedDownloadBeforeProcessExecution(t *testing.T) {
	f := newWindowsVendorFixture(t)
	f.payload.served = []byte("changed Microsoft installer fixture!!")
	if err := f.apply(t); err == nil {
		t.Fatal("corrupt downloaded payload accepted")
	}
	st := f.rec.State()
	if f.runner.runs != 0 || len(st.Pending) != 0 || st.HasSystem("windows", "tool.vcredist") {
		t.Fatal("executed or recorded an unverified payload")
	}
}

func TestWindowsVendorUpdatesOnlyARecordedRuntimeToThePin(t *testing.T) {
	f := newWindowsVendorFixture(t)
	f.installed("14.44.35211.0")
	finding := f.inspect(t, engine.Want{Present: true, Update: true})
	if finding.Status != engine.Satisfied || len(finding.Commands) != 0 || len(finding.Notes) != 1 || !strings.Contains(finding.Notes[0], "installed 14.44.35211.0, pinned 14.51.36247.0") {
		t.Fatal("requested a change of an unrecorded runtime", finding)
	}
	if err := f.provider.Apply(context.Background(), f.resource, engine.Finding{Status: engine.Outdated}, f.rec); err == nil || f.runner.runs != 0 {
		t.Fatal("updated an unrecorded runtime", err)
	}
	f.rec.record(t, "tool.vcredist")
	finding = f.inspect(t, engine.Want{Present: true})
	if finding.Status != engine.Outdated || finding.Reason != "installed 14.44.35211.0, pinned 14.51.36247.0" || len(vendorTestBlocks(finding, "setup")) != 1 {
		t.Fatal(finding)
	}
	if err := f.provider.Apply(context.Background(), f.resource, finding, f.rec); err != nil {
		t.Fatal(err)
	}
	before := f.inputs[0]["Before"].(map[string]any)
	if f.inputs[0]["Action"] != "update" || !strings.HasPrefix(f.inputs[0]["Arguments"].(string), "/install /quiet /norestart ") || before["version"] != "14.44.35211.0" {
		t.Fatal("update input changed", f.inputs[0])
	}
	if after := f.inspect(t, engine.Want{Present: true, Update: true}); after.Status != engine.Satisfied || after.Version != "14.51.36247.0" || len(vendorTestPending(f.rec)) != 0 {
		t.Fatal(after)
	}
}

func TestWindowsVendorDeclinesDowngrade(t *testing.T) {
	f := newWindowsVendorFixture(t)
	f.installed("14.52.40000.0")
	f.rec.record(t, "tool.vcredist")
	finding := f.inspect(t, engine.Want{Present: true, Update: true})
	if finding.Status != engine.Satisfied || len(finding.Commands) != 0 || len(finding.Notes) != 1 || !strings.Contains(finding.Notes[0], "installed 14.52.40000.0, pinned 14.51.36247.0") || !strings.Contains(finding.Notes[0], "declines the downgrade") {
		t.Fatal(finding)
	}
	for _, status := range []engine.Status{engine.Outdated, engine.Unhealthy} {
		if err := f.provider.Apply(context.Background(), f.resource, engine.Finding{Status: status}, f.rec); err == nil || !strings.Contains(err.Error(), "declines the downgrade") {
			t.Fatal("a newer runtime reached the pinned installer", err)
		}
	}
	f.snapshot.Healthy, f.snapshot.HealthIssue = false, "Runtime library could not load: msvcp140.dll"
	if finding := f.inspect(t, engine.Want{Present: true}); finding.Status != engine.Blocked || !strings.Contains(finding.Reason, "declines the downgrade") {
		t.Fatal(finding)
	}
	if f.runner.runs != 0 || f.payload.downloads != 0 || len(vendorTestPending(f.rec)) != 0 {
		t.Fatal("a declined downgrade started work")
	}
}

func TestWindowsVendorNewerRuntimeAfterExit1638IsKept(t *testing.T) {
	f := newWindowsVendorFixture(t)
	f.runner.exit = 1638
	started := f.runner.started
	f.runner.started = func(command platform.Command, input map[string]any) {
		started(command, input)
		f.installed("14.52.40000.0")
	}
	if err := f.apply(t); err != nil {
		t.Fatal(err)
	}
	if after := f.inspect(t, engine.Want{Present: true}); after.Status != engine.Satisfied || len(vendorTestPending(f.rec)) != 0 {
		t.Fatal(after)
	}
}

func TestWindowsVendorRepairsOnlyARecordedRuntime(t *testing.T) {
	f := newWindowsVendorFixture(t)
	f.installed(f.provider.Pin.Version)
	f.snapshot.Healthy, f.snapshot.HealthIssue = false, "Runtime library could not load: msvcp140.dll"
	finding := f.inspect(t, engine.Want{Present: true})
	if finding.Status != engine.Blocked || !strings.Contains(finding.Reason, "could not load") || len(finding.Commands) != 0 {
		t.Fatal(finding)
	}
	if err := f.provider.Apply(context.Background(), f.resource, engine.Finding{Status: engine.Unhealthy}, f.rec); err == nil || f.runner.runs != 0 {
		t.Fatal("repaired an unrecorded runtime", err)
	}
	f.rec.record(t, "tool.vcredist")
	finding = f.inspect(t, engine.Want{Present: true})
	if finding.Status != engine.Unhealthy || len(vendorTestBlocks(finding, "setup")) != 1 || !strings.HasPrefix(finding.Commands[0].Lines[0], "VC_redist.x64.exe /repair ") {
		t.Fatal(finding)
	}
	if err := f.provider.Apply(context.Background(), f.resource, finding, f.rec); err != nil {
		t.Fatal(err)
	}
	if f.inputs[0]["Action"] != "repair" || !strings.HasPrefix(f.inputs[0]["Arguments"].(string), "/repair /quiet /norestart ") || len(vendorTestPending(f.rec)) != 0 {
		t.Fatal(f.inputs[0])
	}
	f.snapshot.Healthy, f.snapshot.BundleID = false, "{33333333-3333-3333-3333-333333333333}"
	if finding = f.inspect(t, engine.Want{Present: true}); finding.Status != engine.Unhealthy || !strings.HasPrefix(finding.Commands[0].Lines[0], "VC_redist.x64.exe /install ") {
		t.Fatal("another registered bundle was sent to the repair of the pinned bundle", finding)
	}
}

func TestWindowsVendorRemovalRouteOnlyForARecordedAndUnneededRuntime(t *testing.T) {
	f := newWindowsVendorFixture(t)
	f.installed(f.provider.Pin.Version)
	f.snapshot.Consumers = []string{"registered-outside-bundle"}
	if finding := f.inspect(t, engine.Want{}); finding.Status != engine.Satisfied || len(finding.Commands) != 0 {
		t.Fatal("offered the removal of an unrecorded runtime", finding)
	}
	f.rec.record(t, "tool.vcredist")
	if finding := f.inspect(t, engine.Want{Present: true}); len(vendorTestBlocks(finding, "you")) != 0 {
		t.Fatal("offered the removal of a needed runtime", finding)
	}
	finding := f.inspect(t, engine.Want{})
	blocks := vendorTestBlocks(finding, "you")
	if finding.Status != engine.Satisfied || len(blocks) != 1 || len(finding.Commands) != 1 || !strings.HasPrefix(blocks[0].Lines[0], "Settings > Apps") || !slices.Contains(finding.Notes, "registered-outside-bundle") {
		t.Fatal(finding)
	}
	if err := vendorPrepareInstaller(context.Background(), f.payload.fetch, f.provider.Directory, f.provider.Pin.URL, f.provider.Pin.SHA256, f.provider.Pin.Size); err != nil {
		t.Fatal(err)
	}
	file := vendorInstallerPath(f.provider.Directory, f.provider.Pin.SHA256)
	if blocks = vendorTestBlocks(f.inspect(t, engine.Want{}), "you"); len(blocks) != 1 || blocks[0].Lines[0] != "& '"+file+"' /uninstall /norestart" {
		t.Fatal("the verified uninstall command of the pinned bundle is missing", blocks)
	}
	f.snapshot.BundleID = "{33333333-3333-3333-3333-333333333333}"
	if blocks = vendorTestBlocks(f.inspect(t, engine.Want{}), "you"); len(blocks) != 1 || !strings.HasPrefix(blocks[0].Lines[0], "Settings > Apps") {
		t.Fatal("another bundle got the uninstall command of the pinned bundle", blocks)
	}
	if err := f.provider.Remove(context.Background(), f.resource, finding, f.rec); err != nil || f.runner.runs != 0 || !f.rec.State().HasSystem("windows", "tool.vcredist") {
		t.Fatal("removal changed a present runtime or its record", err)
	}
	f.snapshot = windowsVendorSnapshot{}
	if finding := f.inspect(t, engine.Want{}); finding.Status != engine.Absent || len(finding.Commands) != 0 {
		t.Fatal(finding)
	}
	if err := f.provider.Remove(context.Background(), f.resource, finding, f.rec); err != nil || f.runner.runs != 0 || f.rec.State().HasSystem("windows", "tool.vcredist") {
		t.Fatal("an absent runtime kept its record", err)
	}
}

func TestWindowsVendorInspectionParsing(t *testing.T) {
	f := newWindowsVendorFixture(t)
	f.installed(f.provider.Pin.Version)
	valid, err := json.Marshal(f.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	edit := func(change func(map[string]any)) []byte {
		var fields map[string]any
		if err := json.Unmarshal(valid, &fields); err != nil {
			t.Fatal(err)
		}
		change(fields)
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	samples := map[string][]byte{
		"unknown field":   edit(func(m map[string]any) { m["boot"] = "boot-one" }),
		"second document": append(slices.Clone(valid), valid...),
		"not JSON":        []byte("# malformed protocol output"),
		"invalid version": edit(func(m map[string]any) { m["version"] = "14.51" }),
		"invalid bundle":  edit(func(m map[string]any) { m["bundle_id"] = "0e3bb569" }),
		"unknown package": edit(func(m map[string]any) {
			m["packages"].(map[string]any)["debug"] = "{11111111-1111-1111-1111-111111111111}"
		}),
		"invalid package ID": edit(func(m map[string]any) { m["packages"].(map[string]any)["minimum"] = "outside" }),
		"unknown file":       edit(func(m map[string]any) { m["files"].(map[string]any)["other.dll"] = strings.Repeat("1", 64) }),
		"invalid file hash":  edit(func(m map[string]any) { m["files"].(map[string]any)["msvcp140.dll"] = "abc" }),
		"healthy but absent": edit(func(m map[string]any) { m["present"] = false }),
		"healthy without files": edit(func(m map[string]any) {
			delete(m["files"].(map[string]any), "msvcp140.dll")
		}),
		"invalid dependent": edit(func(m map[string]any) { m["consumers"] = []string{"line\nbreak"} }),
		"oversized":         []byte(`{"health_issue":"` + strings.Repeat("a", 1<<20) + `"}`),
	}
	for name, output := range samples {
		t.Run(name, func(t *testing.T) {
			f.provider.Query = func(context.Context, string, []byte, ...string) ([]byte, error) { return output, nil }
			if _, err := f.provider.query(context.Background()); err == nil {
				t.Fatal("accepted an invalid inspection")
			}
			finding := f.inspect(t, engine.Want{Present: true})
			if finding.Status != engine.Unknown || finding.Reason == "" || strings.Contains(finding.Reason, "\n") || len(finding.Commands) != 0 {
				t.Fatal("an invalid inspection proved a state", finding)
			}
			if err := f.provider.Remove(context.Background(), f.resource, finding, f.rec); err == nil {
				t.Fatal("an invalid inspection proved absence")
			}
		})
	}
	f.provider.Query = func(context.Context, string, []byte, ...string) ([]byte, error) {
		return edit(func(m map[string]any) { m["consumers"] = []string{"b", "a", "b"} }), nil
	}
	if snapshot, err := f.provider.query(context.Background()); err != nil || !slices.Equal(snapshot.Consumers, []string{"a", "b"}) {
		t.Fatal(snapshot, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.provider.Query = func(ctx context.Context, _ string, _ []byte, _ ...string) ([]byte, error) { return nil, ctx.Err() }
	if _, err := f.provider.Inspect(ctx, f.resource, engine.Want{Present: true}, f.rec); !errors.Is(err, context.Canceled) {
		t.Fatal("a cancelled inspection became a finding", err)
	}
}

func TestWindowsVendorBoundaryAndPinValidation(t *testing.T) {
	f := newWindowsVendorFixture(t)
	f.provider.Issue = "system PowerShell is unavailable"
	if finding := f.inspect(t, engine.Want{Present: true}); finding.Status != engine.Unknown || finding.Reason != f.provider.Issue {
		t.Fatal("system discovery failure lost its explanation", finding)
	}
	if err := f.provider.Apply(context.Background(), f.resource, engine.Finding{Status: engine.Missing}, f.rec); err == nil || f.runner.runs != 0 {
		t.Fatal("a provider without system PowerShell started an installer", err)
	}
	f.provider.Issue = ""
	if _, err := f.provider.Inspect(context.Background(), catalog.Resource{ID: "tool.compiler"}, engine.Want{Present: true}, f.rec); err == nil {
		t.Fatal("the VC++ provider accepted another resource")
	}
	valid := f.provider.Pin
	for name, change := range map[string]func(*catalog.WindowsVendorPin){
		"other host":    func(pin *catalog.WindowsVendorPin) { pin.URL = "https://example.com/VC_redist.x64.exe" },
		"other file":    func(pin *catalog.WindowsVendorPin) { pin.URL = strings.Replace(pin.URL, "x64", "x86", 1) },
		"no SHA-256":    func(pin *catalog.WindowsVendorPin) { pin.SHA256 = "" },
		"no size":       func(pin *catalog.WindowsVendorPin) { pin.Size = 0 },
		"other major":   func(pin *catalog.WindowsVendorPin) { pin.Version = "15.0.0.0" },
		"no bundle":     func(pin *catalog.WindowsVendorPin) { pin.BundleID = "" },
		"plain HTTP":    func(pin *catalog.WindowsVendorPin) { pin.URL = strings.Replace(pin.URL, "https", "http", 1) },
		"query in URL":  func(pin *catalog.WindowsVendorPin) { pin.URL += "?mirror=1" },
		"short version": func(pin *catalog.WindowsVendorPin) { pin.Version = "14.51" },
	} {
		f.provider.Pin = valid
		change(&f.provider.Pin)
		if _, err := f.provider.Inspect(context.Background(), f.resource, engine.Want{Present: true}, f.rec); err == nil {
			t.Fatal("accepted an unreviewed pin: " + name)
		}
	}
	f.provider.Pin = valid
	f.provider.PowerShell = "powershell.exe"
	if _, err := f.provider.Inspect(context.Background(), f.resource, engine.Want{Present: true}, f.rec); err == nil {
		t.Fatal("accepted PowerShell from PATH")
	}
}

func TestWindowsProvidersUseTheReviewedPinsOfTheCheckout(t *testing.T) {
	read := func(file string, pin any) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "tools", file))
		if err != nil {
			t.Fatal(err)
		}
		if err := platform.Decode(data, pin); err != nil {
			t.Fatal(file, err)
		}
	}
	var runtimePin catalog.WindowsVendorPin
	var compilerPin catalog.WindowsBuildToolsPin
	appPins := map[string]catalog.WindowsAppPin{}
	read(filepath.Join("vcredist", "vcredist-pin.json"), &runtimePin)
	read(filepath.Join("compiler", "buildtools-pin.json"), &compilerPin)
	read(filepath.Join("t3-code-app", "windows-app-pins.json"), &appPins)
	root, _ := vendorTestRoot(t)
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	vendor := NewWindowsVendor(root, program, runtimePin, nil, nil)
	compiler := NewBuildTools(root, program, filepath.Join(root, "Program Files", "DotfilesBuildTools"), compilerPin, nil, nil)
	apps := NewWindowsApps(root, program, filepath.Join(root, "Local"), appPins, nil, nil)
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 {
		t.Fatal("provider construction wrote state", entries, err)
	}
	if err := vendor.validate(catalog.Resource{ID: "tool.vcredist"}); err != nil || vendor.Directory != filepath.Join(root, "windows-vendor") {
		t.Fatal("the VC++ runtime route refuses the reviewed pin", err)
	}
	if err := compiler.validate(catalog.Resource{ID: "tool.compiler"}); err != nil || compiler.Directory != filepath.Join(root, "windows-buildtools") {
		t.Fatal("the compiler route refuses the reviewed pin", err)
	}
	app := catalog.Resource{ID: "app.t3-code-windows", Bindings: map[string]catalog.Binding{"windows": {Provider: "windows-app", Package: "t3-code"}}}
	if pin, err := apps.validate(app); err != nil || pin.Publisher != "T3 Tools Inc" || apps.Programs != filepath.Join(root, "Local", "Programs") || apps.Directory != filepath.Join(root, "windows-app") {
		t.Fatal("the Windows application route refuses the reviewed pin", pin, err)
	}
}

func TestWindowsRuntimeRegistrationReconciliation(t *testing.T) {
	program := os.Getenv("DOTFILES_TEST_POWERSHELL")
	if program == "" && runtime.GOOS == "windows" {
		var err error
		program, err = DiscoverWindowsPowerShell()
		if err != nil {
			t.Fatal(err)
		}
	}
	if program == "" {
		program = vendorTestPowerShell(t)
	}
	data, err := os.ReadFile(filepath.Join("testdata", "windows-runtime-mixed-views.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, change string
		refused      bool
		consumer     string
	}{
		{"absent", "$inputData.Registry=@(); $inputData.Files=@()", false, ""},
		{"captured-mixed-views", "", false, "Older VC++ x64 runtime registration in Registry32: 14.44.35211.0"},
		{"reverse-views", "$inputData.Registry[0].View='Registry32'; $inputData.Registry[3].View='Registry64'", false, "Older VC++ x64 runtime registration in Registry64: 14.44.35211.0"},
		{"equivalent-versions", "$inputData.Registry[3].Version='14.51.36247'", false, ""},
		{"single-view", "$inputData.Registry=@($inputData.Registry | Where-Object {$_.View -ne 'Registry32' -or $_.Kind -ne 'runtime'})", false, ""},
		{"unverified-newer-value", "$inputData.Registry[3].Version='14.52.40000.0'", true, ""},
		{"newer-revision", "$inputData.Registry[3].Version='14.51.36247.1'", true, ""},
		{"missing-bundle", "$inputData.Registry=@($inputData.Registry | Where-Object {$_.Kind -ne 'bundle'})", true, ""},
		{"second-bundle", "$other=$inputData.Registry[4] | ConvertTo-Json | ConvertFrom-Json; $other.ID='{11111111-1111-1111-1111-111111111111}'; $inputData.Registry+= $other", true, ""},
		{"bundle-view-disagreement", "$other=$inputData.Registry[4] | ConvertTo-Json | ConvertFrom-Json; $other.DisplayVersion='14.44.35211'; $inputData.Registry+= $other", true, ""},
		{"single-version-bundle-disagreement", "$inputData.Registry[0].Version='14.44.35211'", true, ""},
		{"wrong-publisher", "$inputData.Registry[4].Publisher='Other publisher'", true, ""},
		{"different-msi-id", "$inputData.Registry[5].ID='{11111111-1111-1111-1111-111111111111}'", true, ""},
		{"different-msi-version", "$inputData.Registry[5].Version='14.44.35211'", true, ""},
		{"missing-msi-version", "$inputData.Registry[5].Version=''", true, ""},
		{"missing-msi-part", "$inputData.Registry=@($inputData.Registry | Where-Object {$_.Part -ne 'Additional'})", true, ""},
		{"missing-dll", "$inputData.Files=@($inputData.Files | Where-Object {$_.Name -ne 'msvcp140.dll'})", true, ""},
		{"different-dll-version", "$inputData.Files[2].Version='14.44.35211.0'", true, ""},
		{"outside-dependent", "$inputData.Registry[1].Dependents+= 'outside-consumer'", false, "Older VC++ x64 runtime registration in Registry32: 14.44.35211.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue'; " + windowsRuntimeVersionFunction + windowsRuntimeRegistrationFunctions + `
$inputData = [Console]::In.ReadToEnd() | ConvertFrom-Json
` + tc.change + `
$files = @{}
foreach ($file in $inputData.Files) { $files[$file.Name] = $file.Version }
Resolve-RuntimeRegistration $inputData.Registry $files | ConvertTo-Json -Depth 4 -Compress
`
			command := exec.CommandContext(t.Context(), program, windowsVendorArguments(script)...)
			command.Stdin = strings.NewReader(string(data))
			output, err := command.CombinedOutput()
			if tc.refused {
				if err == nil || !strings.Contains(strings.ToLower(string(output)), "runtime") {
					t.Fatalf("unproved registry reconciliation accepted: %s: %v", output, err)
				}
				return
			}
			var got struct {
				Version   string            `json:"version"`
				Bundle    string            `json:"bundle_id"`
				Packages  map[string]string `json:"packages"`
				Consumers []string          `json:"consumers"`
			}
			if err != nil {
				t.Fatalf("corroborated runtime rejected: %s: %v", output, err)
			}
			version, bundle, packages := "14.51.36247.0", "{0e3bb569-69d6-4c34-bff9-c2f81db5e5f0}", 2
			if tc.name == "absent" {
				version, bundle, packages = "", "", 0
			}
			if err := json.Unmarshal(output, &got); err != nil || got.Version != version || got.Bundle != bundle || len(got.Packages) != packages {
				t.Fatalf("resolved identity changed: %+v: %v", got, err)
			}
			want := []string{}
			if tc.consumer != "" {
				want = append(want, tc.consumer)
			}
			if tc.name == "outside-dependent" {
				want = append(want, "outside-consumer")
			}
			if strings.Join(got.Consumers, "\n") != strings.Join(want, "\n") {
				t.Fatalf("outside registrations/dependents changed: %v; want %v", got.Consumers, want)
			}
		})
	}
}

func TestWindowsRuntimeVersionsCompareNumericComponents(t *testing.T) {
	program := vendorTestPowerShell(t)
	script := "$ErrorActionPreference='Stop'; " + windowsRuntimeVersionFunction + `
foreach ($value in @('v14.51.36247.00', '14.51.36247.0', '14.51.36247')) {
	if ((ConvertTo-RuntimeVersion $value) -cne '14.51.36247.0') { throw 'Equivalent versions differ' }
}
if ((ConvertTo-RuntimeVersion '14.51.36247.1') -ceq '14.51.36247.0') { throw 'Different revisions collapsed' }
foreach ($value in @('', '14.51', '14.51.36247.0.1', '14.51.text', '14.51.36247.-1')) {
	$rejected = $false
	try { $null = ConvertTo-RuntimeVersion $value } catch { $rejected = $true }
	if (-not $rejected) { throw 'Invalid version accepted' }
}
`
	command := exec.CommandContext(t.Context(), program, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native registry version comparison: %s: %v", output, err)
	}
}
