package system

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/platform"
)

var windowsBuildToolsTestComponents = []string{"Microsoft.VisualStudio.Workload.VCTools", "Microsoft.VisualStudio.Component.VC.Tools.x86.x64", "Microsoft.VisualStudio.Component.Windows11SDK.26100"}

type windowsBuildToolsFixture struct {
	provider         *BuildTools
	resource         catalog.Resource
	rec              vendorTestRecords
	state            windowsBuildToolsSnapshot
	runner           *vendorTestRunner
	payload          *vendorTestPayload
	inputs           []map[string]any
	missingComponent bool
	unhealthy        bool
}

func newWindowsBuildToolsFixture(t *testing.T) *windowsBuildToolsFixture {
	t.Helper()
	root, rec := vendorTestRoot(t)
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	payload := newVendorTestPayload("official Microsoft Build Tools fixture")
	pin := catalog.WindowsBuildToolsPin{Version: "17.14.37710.0", URL: "https://download.visualstudio.microsoft.com/fixture/vs_BuildTools.exe", SHA256: payload.sha(), Size: int64(len(payload.data)), Components: slices.Clone(windowsBuildToolsTestComponents)}
	f := &windowsBuildToolsFixture{resource: catalog.Resource{ID: "tool.compiler", Name: "C/C++ toolchain", Action: "compiler", Scope: "machine", Shared: true}, rec: rec, payload: payload}
	f.runner = &vendorTestRunner{t: t}
	f.provider = NewBuildTools(filepath.Join(root, "state"), program, filepath.Join(root, "Program Files", "DotfilesBuildTools"), pin, f.runner, payload.fetch)
	f.provider.Query = func(_ context.Context, program string, input []byte, _ ...string) ([]byte, error) {
		if program != f.provider.PowerShell {
			t.Fatal("inspection escaped its process boundary")
		}
		var request map[string]any
		if err := json.Unmarshal(input, &request); err != nil {
			t.Fatal(err)
		}
		if request["Probe"] != nil {
			return json.Marshal(map[string]string{"PATH": "approved-bin", "INCLUDE": "approved-include", "LIB": "approved-lib", "LIBPATH": "approved-libpath", "VCINSTALLDIR": "approved-vc", "VCToolsInstallDir": "approved-vctools", "WindowsSdkDir": "approved-sdk", "WindowsSDKVersion": "10.0.26100.0\\"})
		}
		return json.Marshal(f.state)
	}
	f.runner.started = func(command platform.Command, input map[string]any) {
		if command.Path != f.provider.PowerShell || !command.Elevated {
			t.Fatal("the bootstrapper did not run through system PowerShell with its elevation declared")
		}
		st := f.rec.State()
		if !slices.ContainsFunc(st.Pending, func(mark string) bool { return strings.HasPrefix(mark, "tool.compiler:") }) || !st.HasSystem("windows", "tool.compiler") {
			t.Fatal("the bootstrapper started before its pending mark and system record", st.Pending, st.System)
		}
		if data, err := os.ReadFile(input["File"].(string)); err != nil || string(data) != string(payload.data) {
			t.Fatal("the bootstrapper started without its verified payload", err)
		}
		f.inputs = append(f.inputs, input)
		if f.runner.exit != vendorDeniedExit && f.runner.exit != 1603 {
			f.installed()
			f.state.Healthy = !f.unhealthy
		}
	}
	return f
}

func (f *windowsBuildToolsFixture) installed() {
	packages := []windowsBuildToolsPackage{}
	for _, component := range windowsBuildToolsTestComponents {
		packages = append(packages, windowsBuildToolsPackage{ID: component, Version: "17.14.0.0", Type: "Component", Count: 1})
	}
	if f.missingComponent {
		packages = packages[:2]
	}
	f.state = windowsBuildToolsSnapshot{Selected: "1234abcd", Healthy: true, Occupied: true, Instances: []windowsBuildToolsInstance{{ID: "1234abcd", Path: f.provider.InstallDirectory, Product: windowsBuildToolsProduct, Version: "17.14.37710.0", Complete: true, Packages: packages, Files: map[string]string{"cl.exe": strings.Repeat("1", 64), "link.exe": strings.Repeat("2", 64), "VsDevCmd.bat": strings.Repeat("3", 64), "Microsoft.VCToolsVersion.default.txt": strings.Repeat("4", 64)}}}}
}

func (f *windowsBuildToolsFixture) inspect(t *testing.T, want engine.Want) engine.Finding {
	t.Helper()
	finding, err := f.provider.Inspect(context.Background(), f.resource, want, f.rec)
	if err != nil {
		t.Fatal(err)
	}
	return finding
}

func (f *windowsBuildToolsFixture) apply(t *testing.T) error {
	t.Helper()
	return f.provider.Apply(context.Background(), f.resource, f.inspect(t, engine.Want{Present: true, Update: true}), f.rec)
}

func TestWindowsBuildToolsReusesPreexistingCompilerWithoutRecord(t *testing.T) {
	f := newWindowsBuildToolsFixture(t)
	f.installed()
	f.state.Instances[0].Path = filepath.Join(filepath.Dir(f.provider.InstallDirectory), "Visual Studio Enterprise")
	f.state.Instances[0].Version = "17.9.34622.214"
	finding := f.inspect(t, engine.Want{Present: true, Update: true})
	if finding.Status != engine.Satisfied || len(finding.Commands) != 0 || len(finding.Notes) != 1 || !strings.Contains(finding.Notes[0], "installed 17.9.34622.214, pinned 17.14.37710.0") {
		t.Fatal(finding)
	}
	for _, status := range []engine.Status{engine.Missing, engine.Outdated, engine.Unhealthy} {
		if err := f.provider.Apply(context.Background(), f.resource, engine.Finding{Status: status}, f.rec); err == nil {
			t.Fatal("changed a pre-existing compiler")
		}
	}
	f.rec.record(t, "tool.compiler")
	if finding := f.inspect(t, engine.Want{Present: true, Update: true}); finding.Status != engine.Satisfied || len(finding.Commands) != 0 {
		t.Fatal("a record of the dedicated instance changed another Visual Studio instance", finding)
	}
	if err := f.provider.Apply(context.Background(), f.resource, engine.Finding{Status: engine.Outdated}, f.rec); err == nil || f.runner.runs != 0 || f.payload.downloads != 0 {
		t.Fatal("updated another Visual Studio instance", err)
	}
	f.state.Healthy, f.state.HealthIssue = false, "The Microsoft C++/Windows SDK compile-and-run probe failed: 2"
	if finding := f.inspect(t, engine.Want{Present: true}); finding.Status != engine.Blocked || !strings.Contains(finding.Reason, "probe failed") {
		t.Fatal(finding)
	}
}

func TestWindowsBuildToolsInstallAndResolveRestrictedCompilerEnvironment(t *testing.T) {
	f := newWindowsBuildToolsFixture(t)
	if _, err := f.provider.CompilerEnvironment(context.Background()); err == nil {
		t.Fatal("resolved compiler environment before installation")
	}
	finding := f.inspect(t, engine.Want{Present: true})
	want := `--quiet --wait --norestart --installPath "` + f.provider.InstallDirectory + `" --add Microsoft.VisualStudio.Workload.VCTools --add Microsoft.VisualStudio.Component.VC.Tools.x86.x64 --add Microsoft.VisualStudio.Component.Windows11SDK.26100`
	if finding.Status != engine.Missing || !finding.Pending || !finding.Privileged || len(vendorTestBlocks(finding, "setup")) != 1 || finding.Commands[0].Lines[0] != "vs_BuildTools.exe "+want {
		t.Fatal(finding)
	}
	if err := f.provider.Apply(context.Background(), f.resource, finding, f.rec); err != nil {
		t.Fatal(err)
	}
	st := f.rec.State()
	if f.runner.runs != 1 || len(st.Pending) != 0 || !st.HasSystem("windows", "tool.compiler") {
		t.Fatal("verified installation left a mark or lost its record", st.Pending, st.System)
	}
	if f.inputs[0]["Action"] != "install" || f.inputs[0]["Arguments"] != want || f.inputs[0]["InstallDirectory"] != f.provider.InstallDirectory || f.inputs[0]["SHA256"] != f.payload.sha() {
		t.Fatal("bootstrapper input changed", f.inputs[0])
	}
	if after := f.inspect(t, engine.Want{Present: true}); after.Status != engine.Satisfied || after.Version != "17.14.37710.0" {
		t.Fatal(after)
	}
	environment, err := f.provider.CompilerEnvironment(context.Background())
	if err != nil || len(environment) != 8 || environment["PATH"] != "approved-bin" {
		t.Fatal(environment, err)
	}
}

func TestWindowsBuildToolsInstallerArgumentsHaveNoRemovalOrDowngrade(t *testing.T) {
	directory := `C:\Program Files\DotfilesBuildTools`
	for action, want := range map[string]string{
		"install": `--quiet --wait --norestart --installPath "C:\Program Files\DotfilesBuildTools" --add One --add Two`,
		"update":  `update --quiet --wait --norestart --installPath "C:\Program Files\DotfilesBuildTools"`,
		"repair":  `repair --quiet --wait --norestart --installPath "C:\Program Files\DotfilesBuildTools"`,
	} {
		if got, err := windowsBuildToolsInstallerArguments(action, directory, []string{"One", "Two"}); err != nil || got != want {
			t.Fatal(action, got, err)
		}
	}
	for _, action := range []string{"remove", "uninstall", "rollback", "downgrade", ""} {
		if got, err := windowsBuildToolsInstallerArguments(action, directory, nil); err == nil {
			t.Fatal("unsupported action built bootstrapper arguments", action, got)
		}
	}
	if _, err := windowsBuildToolsInstallerArguments("install", `C:\"quoted"`, nil); err == nil {
		t.Fatal("a quote in the installation directory reached the bootstrapper command line")
	}
}

func TestWindowsBuildToolsDoesNotClaimMissingRequestedSDK(t *testing.T) {
	f := newWindowsBuildToolsFixture(t)
	f.missingComponent = true
	if err := f.apply(t); err == nil || !strings.Contains(err.Error(), "Windows11SDK") {
		t.Fatal("accepted missing pinned SDK component", err)
	}
	if !slices.Contains(vendorTestPending(f.rec), "tool.compiler:install") {
		t.Fatal("an unverified installation cleared its pending mark")
	}
}

func TestWindowsBuildToolsDeniedElevationAndRestartRemainExplicit(t *testing.T) {
	f := newWindowsBuildToolsFixture(t)
	f.runner.exit = vendorDeniedExit
	if err := f.apply(t); err == nil {
		t.Fatal("accepted cancelled elevation")
	}
	st := f.rec.State()
	if len(st.Pending) != 0 || st.HasSystem("windows", "tool.compiler") || f.state.Selected != "" {
		t.Fatal("an unstarted installation kept its mark or provisional record", st.Pending, st.System)
	}
	f.runner.exit, f.unhealthy = 3010, true
	if err := f.apply(t); err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatal("a restart-required installation claimed completion before its probe passed", err)
	}
	st = f.rec.State()
	if !slices.Contains(st.Pending, "tool.compiler:install") || !st.HasSystem("windows", "tool.compiler") || f.runner.runs != 2 {
		t.Fatal(st.Pending, st.System, f.runner.runs)
	}
	f.state.Healthy = true
	if finding := f.inspect(t, engine.Want{Present: true}); finding.Status != engine.Satisfied {
		t.Fatal(finding)
	}
	g := newWindowsBuildToolsFixture(t)
	g.runner.exit = 3010
	if err := g.apply(t); err != nil || len(vendorTestPending(g.rec)) != 0 {
		t.Fatal("a usable compiler stayed pending after exit 3010", err)
	}
	h := newWindowsBuildToolsFixture(t)
	h.runner.exit = 1603
	if err := h.apply(t); err == nil || !strings.Contains(err.Error(), "exit code 1603") || !slices.Contains(vendorTestPending(h.rec), "tool.compiler:install") {
		t.Fatal(err)
	}
}

func TestWindowsBuildToolsUpdatesAndRepairsOnlyTheRecordedDedicatedInstance(t *testing.T) {
	f := newWindowsBuildToolsFixture(t)
	f.installed()
	f.state.Instances[0].Version = "17.12.35527.113"
	if finding := f.inspect(t, engine.Want{Present: true, Update: true}); finding.Status != engine.Satisfied || len(finding.Commands) != 0 {
		t.Fatal("requested a change of an unrecorded dedicated instance", finding)
	}
	f.rec.record(t, "tool.compiler")
	finding := f.inspect(t, engine.Want{Present: true})
	if finding.Status != engine.Outdated || finding.Reason != "installed 17.12.35527.113, pinned 17.14.37710.0" || !strings.HasPrefix(finding.Commands[0].Lines[0], "vs_BuildTools.exe update --quiet ") {
		t.Fatal(finding)
	}
	if err := f.provider.Apply(context.Background(), f.resource, finding, f.rec); err != nil {
		t.Fatal(err)
	}
	before := f.inputs[0]["Before"].(map[string]any)
	if f.inputs[0]["Action"] != "update" || before["selected"] != "1234abcd" || len(vendorTestPending(f.rec)) != 0 {
		t.Fatal(f.inputs[0])
	}
	f.state.Instances[0].Version = "17.14.40000.1"
	if finding := f.inspect(t, engine.Want{Present: true, Update: true}); finding.Status != engine.Satisfied || len(finding.Commands) != 0 {
		t.Fatal("a compiler newer than the pin was not kept", finding)
	}
	f.state.Healthy, f.state.HealthIssue = false, "The registered C++ instance is incomplete"
	finding = f.inspect(t, engine.Want{Present: true})
	if finding.Status != engine.Unhealthy || !strings.HasPrefix(finding.Commands[0].Lines[0], "vs_BuildTools.exe repair --quiet ") {
		t.Fatal(finding)
	}
	if err := f.provider.Apply(context.Background(), f.resource, finding, f.rec); err != nil || f.inputs[1]["Action"] != "repair" {
		t.Fatal(err)
	}
	f.runner.started = func(platform.Command, map[string]any) {
		f.state.Selected, f.state.Instances[0].ID = "9999ffff", "9999ffff"
	}
	if err := f.provider.Apply(context.Background(), f.resource, engine.Finding{Status: engine.Unhealthy}, f.rec); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatal("accepted a replaced instance", err)
	}
}

func TestWindowsBuildToolsPreservesAnOccupiedDirectory(t *testing.T) {
	f := newWindowsBuildToolsFixture(t)
	f.state.Occupied = true
	finding := f.inspect(t, engine.Want{Present: true})
	if finding.Status != engine.Blocked || !slices.Equal(finding.Preserved, []string{f.provider.InstallDirectory}) || len(finding.Commands) != 0 {
		t.Fatal(finding)
	}
	if err := f.provider.Apply(context.Background(), f.resource, engine.Finding{Status: engine.Missing}, f.rec); err == nil || f.runner.runs != 0 {
		t.Fatal("installed into an occupied directory", err)
	}
}

func TestWindowsBuildToolsRemovalRouteOnlyForARecordedAndUnneededCompiler(t *testing.T) {
	f := newWindowsBuildToolsFixture(t)
	f.installed()
	f.state.Consumers = []string{"Running cl.exe (PID 123)"}
	if finding := f.inspect(t, engine.Want{}); finding.Status != engine.Satisfied || len(finding.Commands) != 0 {
		t.Fatal("offered the removal of an unrecorded compiler", finding)
	}
	f.rec.record(t, "tool.compiler")
	if finding := f.inspect(t, engine.Want{Present: true}); len(vendorTestBlocks(finding, "you")) != 0 {
		t.Fatal("offered the removal of a needed compiler", finding)
	}
	finding := f.inspect(t, engine.Want{})
	blocks := vendorTestBlocks(finding, "you")
	if len(blocks) != 1 || len(finding.Commands) != 1 || !strings.HasPrefix(blocks[0].Lines[0], "Visual Studio Installer > ") || !slices.Contains(finding.Notes, "Running cl.exe (PID 123)") {
		t.Fatal(finding)
	}
	if err := vendorPrepareInstaller(context.Background(), f.payload.fetch, f.provider.Directory, f.provider.Pin.URL, f.provider.Pin.SHA256, f.provider.Pin.Size); err != nil {
		t.Fatal(err)
	}
	file := vendorInstallerPath(f.provider.Directory, f.provider.Pin.SHA256)
	if blocks = vendorTestBlocks(f.inspect(t, engine.Want{}), "you"); len(blocks) != 1 || blocks[0].Lines[0] != "& '"+file+"' uninstall --wait --norestart --installPath '"+f.provider.InstallDirectory+"'" {
		t.Fatal("the verified uninstall command of the dedicated instance is missing", blocks)
	}
	f.state.Instances[0].Path = filepath.Join(filepath.Dir(f.provider.InstallDirectory), "Other")
	if blocks = vendorTestBlocks(f.inspect(t, engine.Want{}), "you"); len(blocks) != 1 || !strings.HasPrefix(blocks[0].Lines[0], "Visual Studio Installer > ") {
		t.Fatal("another instance got the uninstall command of the dedicated instance", blocks)
	}
	if err := f.provider.Remove(context.Background(), f.resource, finding, f.rec); err != nil || f.runner.runs != 0 || !f.rec.State().HasSystem("windows", "tool.compiler") {
		t.Fatal("removal changed a present compiler or its record", err)
	}
	f.state = windowsBuildToolsSnapshot{}
	if finding := f.inspect(t, engine.Want{}); finding.Status != engine.Absent || len(finding.Commands) != 0 {
		t.Fatal(finding)
	}
	if err := f.provider.Remove(context.Background(), f.resource, finding, f.rec); err != nil || f.runner.runs != 0 || f.rec.State().HasSystem("windows", "tool.compiler") {
		t.Fatal("an absent compiler kept its record", err)
	}
}

func TestWindowsBuildToolsCompilerEnvironmentRejectsExtraVariables(t *testing.T) {
	f := newWindowsBuildToolsFixture(t)
	f.installed()
	original := f.provider.Query
	f.provider.Query = func(ctx context.Context, program string, input []byte, args ...string) ([]byte, error) {
		var request map[string]any
		if err := json.Unmarshal(input, &request); err != nil {
			return nil, err
		}
		if request["Probe"] != nil {
			return []byte(`{"PERSONAL_TOKEN":"redacted"}`), nil
		}
		return original(ctx, program, input, args...)
	}
	if _, err := f.provider.CompilerEnvironment(context.Background()); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatal("accepted unrelated environment", err)
	}
}

func TestWindowsBuildToolsPackageInventoryPreservesMultiplicity(t *testing.T) {
	program := vendorTestPowerShell(t)
	start := strings.Index(windowsBuildToolsPrelude, "$packages = @()")
	end := strings.Index(windowsBuildToolsPrelude, "$instances +=")
	if start < 0 || end <= start {
		t.Fatal("production inventory projection missing")
	}
	script := "$ErrorActionPreference='Stop';" + windowsBuildToolsPackagePrelude + "$item=[Console]::In.ReadToEnd() | ConvertFrom-Json;" + windowsBuildToolsPrelude[start:end] + "ConvertTo-Json -InputObject $packages -Depth 3 -Compress"
	for _, scenario := range []struct {
		name, packages string
		count, total   int
	}{
		{"parallel versions", `[{"id":"SDK","version":"1.0","type":"Msi"},{"id":"SDK","version":"2.0","type":"Msi"}]`, 2, 2},
		{"parallel branches", `[{"id":"SDK","version":"1.0","branch":"stable","type":"Msi"},{"id":"SDK","version":"1.0","branch":"preview","type":"Msi"}]`, 2, 2},
		{"identical record", `[{"id":"SDK","version":"1.0","type":"Msi"},{"id":"SDK","version":"1.0","type":"Msi"}]`, 1, 2},
		{"extension distinction", `[{"id":"SDK","version":"1.0","type":"Vsix"},{"id":"SDK","version":"1.0","type":"Vsix","extension":true}]`, 2, 2},
		{"ordinal case distinction", `[{"id":"SDK","version":"1.0","type":"Msi"},{"id":"sdk","version":"1.0","type":"Msi"}]`, 2, 2},
		{"empty inventory", `[]`, 0, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			command := exec.Command(program, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
			command.Stdin = strings.NewReader(`{"packages":` + scenario.packages + `}`)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatal(string(output), err)
			}
			var packages []map[string]any
			if err := json.Unmarshal(output, &packages); err != nil || len(packages) != scenario.count {
				t.Fatal(packages, string(output), err)
			}
			total := 0
			for _, entry := range packages {
				count, ok := entry["count"].(float64)
				if !ok || count < 1 {
					t.Fatal("invalid multiplicity", entry)
				}
				total += int(count)
			}
			if total != scenario.total {
				t.Fatal("inventory lost package occurrences", total, scenario.total)
			}
		})
	}
}

func TestWindowsBuildToolsPackageOrderDoesNotChangeTheToken(t *testing.T) {
	f := newWindowsBuildToolsFixture(t)
	f.installed()
	f.state.Instances[0].Packages[0].Count = 2
	before := f.inspect(t, engine.Want{Present: true})
	slices.Reverse(f.state.Instances[0].Packages)
	f.state.Consumers = []string{"Running cl.exe (PID 123)"}
	after := f.inspect(t, engine.Want{Present: true})
	if before.Token == "" || before.Token != after.Token {
		t.Fatal("package enumeration order or a running compiler changed the token", before, after)
	}
	f.state.Instances[0].Packages[0].Count++
	if changed := f.inspect(t, engine.Want{Present: true}); changed.Token == before.Token {
		t.Fatal("a changed package inventory kept the token")
	}
}

func TestWindowsBuildToolsInspectionParsing(t *testing.T) {
	for _, count := range []int{0, -1, 10001, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newWindowsBuildToolsFixture(t)
			f.installed()
			f.state.Instances[0].Packages[0].Count = count
			if _, err := f.provider.query(context.Background()); err == nil {
				t.Fatal("accepted invalid individual or total multiplicity", count)
			}
			if finding := f.inspect(t, engine.Want{Present: true}); finding.Status != engine.Unknown {
				t.Fatal("an invalid inspection proved a state", finding)
			}
		})
	}
	f := newWindowsBuildToolsFixture(t)
	f.installed()
	f.state.Instances[0].Packages[0].Count = 9998
	if _, err := f.provider.query(context.Background()); err != nil {
		t.Fatal("rejected exact 10000 occurrence bound", err)
	}
	for name, change := range map[string]func(*windowsBuildToolsSnapshot){
		"instance ID":       func(s *windowsBuildToolsSnapshot) { s.Instances[0].ID, s.Selected = "ABCD1234", "ABCD1234" },
		"relative path":     func(s *windowsBuildToolsSnapshot) { s.Instances[0].Path = "DotfilesBuildTools" },
		"version":           func(s *windowsBuildToolsSnapshot) { s.Instances[0].Version = "17.14" },
		"product":           func(s *windowsBuildToolsSnapshot) { s.Instances[0].Product = "Outside.Product" },
		"duplicate package": func(s *windowsBuildToolsSnapshot) { s.Instances[0].Packages[1] = s.Instances[0].Packages[0] },
		"package without ID": func(s *windowsBuildToolsSnapshot) {
			s.Instances[0].Packages[0].ID = ""
		},
		"unknown tool file":     func(s *windowsBuildToolsSnapshot) { s.Instances[0].Files["cmd.exe"] = strings.Repeat("1", 64) },
		"tool hash":             func(s *windowsBuildToolsSnapshot) { s.Instances[0].Files["cl.exe"] = "abc" },
		"unknown selection":     func(s *windowsBuildToolsSnapshot) { s.Selected = "0000ffff" },
		"healthy and partial":   func(s *windowsBuildToolsSnapshot) { s.Instances[0].Complete = false },
		"healthy without files": func(s *windowsBuildToolsSnapshot) { delete(s.Instances[0].Files, "link.exe") },
		"duplicate instance":    func(s *windowsBuildToolsSnapshot) { s.Instances = append(s.Instances, s.Instances[0]) },
		"consumer":              func(s *windowsBuildToolsSnapshot) { s.Consumers = []string{""} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newWindowsBuildToolsFixture(t)
			f.installed()
			change(&f.state)
			if _, err := f.provider.query(context.Background()); err == nil {
				t.Fatal("accepted an invalid inspection")
			}
		})
	}
	f.provider.Query = func(context.Context, string, []byte, ...string) ([]byte, error) {
		return []byte(`{"instances":[],"selected":"","healthy":false,"occupied":false,"health_issue":"","consumers":[],"boot":"boot-one"}`), nil
	}
	if _, err := f.provider.query(context.Background()); err == nil {
		t.Fatal("accepted an unknown inspection field")
	}
}

func TestWindowsBuildToolsWorkerRechecksPackageMultiplicity(t *testing.T) {
	program := vendorTestPowerShell(t)
	start := strings.Index(windowsBuildToolsInstallScript, "$expectedPackages =")
	end := strings.Index(windowsBuildToolsInstallScript[start:], "foreach ($field in @('files'))") + start
	if start < 0 || end <= start {
		t.Fatal("production package guard missing")
	}
	script := "$ErrorActionPreference='Stop';" + windowsBuildToolsPackagePrelude + `$data=[Console]::In.ReadToEnd() | ConvertFrom-Json;$before=@([pscustomobject]@{packages=$data.before});$instance=@([pscustomobject]@{packages=$data.after});` + windowsBuildToolsInstallScript[start:end]
	base := []windowsBuildToolsPackage{{ID: "SDK", Version: "1.0", Type: "Msi", Count: 2}, {ID: "Extension", Version: "2.0", Type: "Vsix", Extension: true, Count: 1}}
	for _, scenario := range []string{"reordered", "count changed", "record missing", "extension changed"} {
		t.Run(scenario, func(t *testing.T) {
			after := slices.Clone(base)
			switch scenario {
			case "reordered":
				slices.Reverse(after)
			case "count changed":
				after[0].Count--
			case "record missing":
				after = after[:1]
			case "extension changed":
				after[1].Extension = false
			}
			input, err := json.Marshal(map[string]any{"before": base, "after": after})
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command(program, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
			command.Stdin = bytes.NewReader(input)
			output, err := command.CombinedOutput()
			if scenario == "reordered" {
				if err != nil {
					t.Fatal(string(output), err)
				}
			} else if err == nil || !strings.Contains(string(output), "dedicated instance package") {
				t.Fatal("changed package bag reached vendor execution", string(output), err)
			}
		})
	}
}

func TestWindowsBuildToolsPackageGuardStaysInTheInstallerScript(t *testing.T) {
	start := strings.Index(windowsBuildToolsInstallScript, "$expectedPackages =")
	guard := strings.Index(windowsBuildToolsInstallScript, "foreach ($field in @('files'))")
	launch := strings.Index(windowsBuildToolsInstallScript, "[Diagnostics.Process]::Start(")
	if start < 0 || guard <= start || launch <= guard || !strings.Contains(windowsBuildToolsPrelude, "$packages = @()") || !strings.Contains(windowsBuildToolsPrelude, "$instances +=") {
		t.Fatal("the bootstrapper starts before the package and file checks of the dedicated instance")
	}
}
