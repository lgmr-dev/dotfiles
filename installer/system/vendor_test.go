package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

type vendorTestRecords struct{ store *state.Store }

func (r vendorTestRecords) State() state.State { return r.store.View() }

func (r vendorTestRecords) Update(edit func(*state.State) error) error { return r.store.Update(edit) }

func (r vendorTestRecords) record(t *testing.T, name string) {
	t.Helper()
	if err := r.Update(func(s *state.State) error { s.AddSystem("windows", name); return nil }); err != nil {
		t.Fatal(err)
	}
}

func (r vendorTestRecords) forget(name string) error {
	return r.Update(func(s *state.State) error { s.RemoveSystem("windows", name); return nil })
}

func vendorTestRoot(t *testing.T) (string, vendorTestRecords) {
	t.Helper()
	root, err := platform.ResolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(root, "state"), "windows/amd64:fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return root, vendorTestRecords{store}
}

type vendorTestRunner struct {
	t       *testing.T
	exit    int
	fail    error
	runs    int
	started func(command platform.Command, input map[string]any)
}

func (r *vendorTestRunner) Run(_ context.Context, command platform.Command) ([]byte, error) {
	r.runs++
	if err := command.Validate(); err != nil {
		r.t.Fatal(err)
	}
	if len(command.Args) != 5 || command.Args[3] != "-EncodedCommand" || command.Interactive {
		r.t.Fatal("installer command widened its process boundary", command.Args[:4])
	}
	data, err := io.ReadAll(command.Stdin)
	if err != nil {
		r.t.Fatal(err)
	}
	var input map[string]any
	if err := json.Unmarshal(data, &input); err != nil {
		r.t.Fatal(err)
	}
	if r.fail != nil {
		return nil, r.fail
	}
	r.started(command, input)
	if r.exit != 0 {
		code := r.exit
		return nil, &platform.CommandError{Message: fmt.Sprintf("native command failed: exit status %d", code), ExitCode: &code}
	}
	return nil, nil
}

type vendorTestPayload struct {
	data      []byte
	served    []byte
	downloads int
}

func newVendorTestPayload(text string) *vendorTestPayload {
	return &vendorTestPayload{data: []byte(text), served: []byte(text)}
}

func (p *vendorTestPayload) sha() string {
	hash := sha256.Sum256(p.data)
	return hex.EncodeToString(hash[:])
}

func (p *vendorTestPayload) fetch(_ context.Context, _, _ string, _ int64, destination string) error {
	p.downloads++
	return os.WriteFile(destination, p.served, 0600)
}

func vendorTestPending(rec engine.Records) []string { return rec.State().Pending }

func vendorTestBlocks(f engine.Finding, by string) []engine.CommandBlock {
	blocks := []engine.CommandBlock{}
	for _, block := range f.Commands {
		if block.By == by {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

func TestVendorCompareVersionsUsesNumericComponents(t *testing.T) {
	for _, sample := range []struct {
		a, b string
		want int
	}{
		{"14.51.36247.0", "14.51.36247.0", 0}, {"14.9.0.0", "14.10.0.0", -1}, {"14.51.36247.1", "14.51.36247.0", 1},
		{"0.1.9", "0.1.8", 1}, {"0.1", "0.1.0", 0}, {"", "0.1.8", -1}, {"17.14.37710.0", "17.9.40000.0", 1},
	} {
		if got := vendorCompareVersions(sample.a, sample.b); got != sample.want {
			t.Fatalf("%q against %q: %d, want %d", sample.a, sample.b, got, sample.want)
		}
	}
}

func TestVendorExitCodes(t *testing.T) {
	for code, success := range map[int]bool{0: true, 3010: true, 1641: true, 1638: false, 1603: false, 1223: false, 1602: false, 740: false, 1618: false, 1: false} {
		if vendorSucceeded(code) != success {
			t.Fatal("exit code classification changed", code)
		}
	}
	for code, text := range map[int]string{1223: "UAC permission was denied", 1602: "cancelled", 740: "administrator approval", 1618: "another Windows installer", 1001: "another Windows installer",
		1003: "another Windows installer", 1638: "does not downgrade", 1603: "exit code 1603; inspect fixture.log", vendorNotStartedExit: "did not start"} {
		if message := vendorExitMessage("Fixture", code, "fixture.log"); !strings.Contains(message, text) {
			t.Fatalf("exit code %d: %s", code, message)
		}
	}
	failure := vendorFailure("Fixture", 1603, "", errors.New("native command failed:\nexit status 1603"))
	if code, ok := platform.ExitCode(failure); !ok || code != 1603 || strings.Contains(failure.Error(), "\n") {
		t.Fatal("the failure lost its exit code or its one-line form", failure)
	}
}

func TestVendorInstallerPreparationVerifiesSizeAndHashAndReusesTheCache(t *testing.T) {
	root, _ := vendorTestRoot(t)
	payload := newVendorTestPayload("reviewed installer fixture")
	size := int64(len(payload.data))
	directory := filepath.Join(root, "vendor")
	if err := vendorPrepareInstaller(context.Background(), nil, directory, "https://fixture.invalid/installer.exe", payload.sha(), size); err == nil {
		t.Fatal("a missing download route was accepted")
	}
	for _, served := range []string{"changed installer fixture!!", "short"} {
		payload.served = []byte(served)
		if err := vendorPrepareInstaller(context.Background(), payload.fetch, directory, "https://fixture.invalid/installer.exe", payload.sha(), size); err == nil {
			t.Fatal("changed installer bytes were accepted", served)
		}
		if _, err := os.Lstat(vendorInstallerPath(directory, payload.sha())); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unverified bytes reached the installer cache", err)
		}
	}
	payload.served = payload.data
	for range 2 {
		if err := vendorPrepareInstaller(context.Background(), payload.fetch, directory, "https://fixture.invalid/installer.exe", payload.sha(), size); err != nil {
			t.Fatal(err)
		}
	}
	if payload.downloads != 3 {
		t.Fatal("the verified installer was downloaded again", payload.downloads)
	}
	entries, err := os.ReadDir(filepath.Join(directory, "payloads"))
	if err != nil || len(entries) != 1 {
		t.Fatal("a download stage stayed", entries, err)
	}
	if err := os.WriteFile(vendorInstallerPath(directory, payload.sha()), []byte("edited installer fixture!!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := vendorPrepareInstaller(context.Background(), payload.fetch, directory, "https://fixture.invalid/installer.exe", payload.sha(), size); err == nil {
		t.Fatal("an edited cached installer was accepted")
	}
}

func TestVendorRecordNameFollowsTheBinding(t *testing.T) {
	app := catalog.Resource{ID: "app.t3-code-windows", Bindings: map[string]catalog.Binding{"windows": {Provider: "windows-app", Package: "t3-code"}}}
	if vendorRecordName(app) != "t3-code" || vendorRecordName(catalog.Resource{ID: "tool.vcredist"}) != "tool.vcredist" {
		t.Fatal("system record name changed")
	}
	context := platform.Context{OS: "windows", Arch: "amd64"}
	if provider, name, ok := engine.SystemRecord(app, context); !ok || provider != vendorSystemKey || name != vendorRecordName(app) {
		t.Fatal("the provider and the engine disagree on the system record", provider, name)
	}
}

func TestWindowsVendorQueryProcessFixture(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "vendor-query-fixture" {
		return
	}
	role := os.Args[len(os.Args)-1]
	fmt.Fprint(os.Stderr, "#< CLIXML\n<Objs><Obj S=\"progress\">Preparing modules for first use.</Obj></Objs>\n")
	if role == "failure" {
		fmt.Fprint(os.Stderr, "native vendor query failed\x1b\n")
		os.Exit(23)
	}
	if role == "malformed" {
		fmt.Fprint(os.Stdout, "# malformed protocol output\n")
	}
	fmt.Fprint(os.Stdout, `{"ready":true}`)
	os.Exit(0)
}

func TestWindowsVendorQueryKeepsProgressSeparateAndRetainsFailures(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := QueryWindowsVendor(context.Background(), "relative.exe", nil); err == nil {
		t.Fatal("a relative inspection executable was accepted")
	}
	for _, role := range []string{"progress", "failure", "malformed"} {
		t.Run(role, func(t *testing.T) {
			output, err := QueryWindowsVendor(context.Background(), program, nil,
				"-test.run=^TestWindowsVendorQueryProcessFixture$", "--", "vendor-query-fixture", role)
			if role == "failure" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 23 || !strings.Contains(err.Error(), "native vendor query failed") || strings.ContainsRune(err.Error(), '\x1b') {
					t.Fatal("native query discarded its error or diagnostic", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var result struct{ Ready bool }
			if err := platform.Decode(output, &result); role == "malformed" {
				if err == nil {
					t.Fatal("query discarded malformed protocol output")
				}
			} else if err != nil || !result.Ready {
				t.Fatal("successful vendor JSON was contaminated by stderr progress", err)
			}
		})
	}
}

func TestWindowsVendorQueryPowerShellProgress(t *testing.T) {
	name := "pwsh"
	if runtime.GOOS == "windows" {
		name = "powershell.exe"
	}
	program, err := exec.LookPath(name)
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Fatal(err)
		}
		t.Skip("optional PowerShell stream probe; process boundary cases always run")
	}
	script := `Write-Progress -Activity 'Preparing modules for first use.' -Status 'Loading' -PercentComplete 10;[Console]::Out.Write('{"ready":true}')`
	output, err := QueryWindowsVendor(context.Background(), program, nil, windowsVendorArguments(script)...)
	if err != nil {
		t.Fatal(err)
	}
	var result struct{ Ready bool }
	if err := platform.Decode(output, &result); err != nil || !result.Ready {
		t.Fatal("encoded PowerShell progress contaminated vendor JSON", err)
	}
}

func vendorTestPowerShell(t *testing.T) string {
	t.Helper()
	program := os.Getenv("DOTFILES_TEST_POWERSHELL")
	if program == "" {
		var err error
		program, err = exec.LookPath("pwsh")
		if err != nil {
			t.Skip("PowerShell is not available")
		}
	}
	return program
}

func vendorTestInstallerScripts() map[string]string {
	return map[string]string{"VC++ runtime": windowsVendorInstallScript, "Build Tools": windowsBuildToolsInstallScript, "application": windowsAppChangeScript}
}

func TestVendorInstallerScriptsVerifyBeforeTheStartAndNeverUninstall(t *testing.T) {
	for name, script := range vendorTestInstallerScripts() {
		start := strings.Index(script, "[Diagnostics.Process]::Start(")
		hash := strings.Index(script, "Get-FileHash -LiteralPath $file -Algorithm SHA256")
		signature := max(strings.LastIndex(script, "Assert-MicrosoftSignature $file"), strings.LastIndex(script, "Assert-PublisherSignature $file"))
		if start < 0 || hash < 0 || signature < 0 || hash > start || signature > start {
			t.Fatal(name + " installer starts before its SHA-256 and signature checks")
		}
		if strings.Count(script, "[Diagnostics.Process]::Start(") != 1 || !strings.Contains(script, "$start.Arguments = [string]$inputData.Arguments") {
			t.Fatal(name + " installer builds arguments outside the reviewed Go recipe")
		}
		if launched := strings.Index(script, "$launched = $true"); launched < start || !strings.Contains(script, fmt.Sprintf("exit %d", vendorNotStartedExit)) {
			t.Fatal(name + " installer cannot report that it did not start")
		}
		lower := strings.ToLower(script)
		for _, forbidden := range []string{"'remove'", "/uninstall", "'uninstall'", "downgrade", "/currentuser /s"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("%s installer script can remove or downgrade: %s", name, forbidden)
			}
		}
	}
}

func TestVendorPowerShellSyntax(t *testing.T) {
	program := vendorTestPowerShell(t)
	for _, script := range []string{windowsVendorObserveScript, windowsVendorInstallScript, windowsAppObserveScript, windowsAppChangeScript,
		windowsBuildToolsObserveScript, windowsBuildToolsInstallScript, windowsBuildToolsEnvironmentScript, windowsBuildToolsEnvironmentProbe} {
		if len(strings.Join(windowsVendorArguments(script), " "))+len(program)+4 >= 32767 {
			t.Fatal("vendor PowerShell command exceeds the Windows command-line limit")
		}
		command := exec.CommandContext(t.Context(), program, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", `$source=[Console]::In.ReadToEnd(); $tokens=$null; $problems=$null; $null=[Management.Automation.Language.Parser]::ParseInput($source,[ref]$tokens,[ref]$problems); if($problems.Count) { $problems | Format-List | Out-String | Write-Output; exit 1 }`)
		command.Stdin = strings.NewReader(script)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("PowerShell syntax: %s: %v", output, err)
		}
	}
}

func TestVendorScriptsFitTheWindowsCommandLine(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{windowsVendorObserveScript, windowsVendorInstallScript, windowsAppObserveScript, windowsAppChangeScript,
		windowsBuildToolsObserveScript, windowsBuildToolsInstallScript, windowsBuildToolsEnvironmentScript, windowsBuildToolsEnvironmentProbe} {
		if len(strings.Join(windowsVendorArguments(script), " "))+len(`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`)+4 >= 32767 {
			t.Fatal("vendor PowerShell command exceeds the Windows command-line limit")
		}
		if err := (platform.Command{Path: program, Args: windowsVendorArguments(script)}).Validate(); err != nil {
			t.Fatal("the runner refuses the encoded script", err)
		}
	}
}
