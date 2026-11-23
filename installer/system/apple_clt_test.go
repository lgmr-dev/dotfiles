package system

import (
	"context"
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

type appleCLTFixture struct {
	t                    *testing.T
	d                    *AppleCLT
	r                    catalog.Resource
	rec                  *macTestRecords
	runner               *macTestRunner
	selected, fail       string
	installed, unhealthy bool
	receipt              string
	unchanged            bool
	pending              [][]string
}

func newAppleCLTFixture(t *testing.T) *appleCLTFixture {
	t.Helper()
	root := macTestRoot(t)
	f := &appleCLTFixture{t: t, d: NewAppleCLT(), r: catalog.Resource{ID: "infra.apple-clt", Name: "Apple Command Line Tools", Retain: true, ReplanAfter: true}, rec: newMacTestRecords(), runner: &macTestRunner{}}
	f.d.clt, f.d.placeholder = filepath.Join(root, "CLT"), filepath.Join(root, "placeholder")
	f.d.Query, f.d.Runner, f.runner.run = f.query, f.runner, f.run
	return f
}

func (f *appleCLTFixture) query(_ context.Context, program string, args ...string) ([]byte, error) {
	switch program {
	case "/usr/bin/xcode-select":
		if f.selected == "" {
			return nil, errMacNoDeveloperSelection
		}
		return []byte(f.selected + "\n"), nil
	case "/usr/sbin/pkgutil":
		if args[0] == "--pkgs" {
			if f.installed || f.receipt != "" {
				return []byte("com.apple.pkg.CLTools_Executables\n"), nil
			}
			return []byte("unrelated.package\ncom.apple.pkg.CLTools_Executables.extra\n"), nil
		}
		if f.installed || f.receipt != "" {
			receipt := f.receipt
			if receipt == "" {
				receipt = "26.0.0@100"
			}
			version, timestamp, _ := strings.Cut(receipt, "@")
			return []byte("package-id: com.apple.pkg.CLTools_Executables\nversion: " + version + "\nvolume: /\nlocation: /\ninstall-time: " + timestamp + "\n"), nil
		}
		return nil, errors.New("no package receipt")
	case "/usr/bin/xcrun":
		if slices.Contains(args, "--show-sdk-path") {
			return []byte(filepath.Join(f.selected, "SDKs", "macos.sdk") + "\n"), nil
		}
		return []byte(filepath.Join(f.selected, "usr", "bin", args[len(args)-1]) + "\n"), nil
	default:
		if f.unhealthy {
			return nil, errors.New("broken compiler")
		}
		if filepath.Base(program) == "make" {
			return []byte("GNU Make 3.81\n"), nil
		}
		return []byte("Apple clang version 17.0.0\n"), nil
	}
}

func (f *appleCLTFixture) install(path string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(path, "SDKs", "macos.sdk"), 0755); err != nil {
		f.t.Fatal(err)
	}
	for _, name := range []string{"clang", "clang++", "make"} {
		macTestWrite(f.t, filepath.Join(path, "usr", "bin", name), "fixture; never executed", 0755)
	}
	f.installed = true
	if !f.unchanged {
		f.receipt = "26.0.0@200"
	}
}

func (f *appleCLTFixture) run(c platform.Command, _ string) ([]byte, error) {
	f.pending = append(f.pending, f.rec.State().Pending)
	if f.fail == "not-started" {
		return nil, errors.New("fork failed")
	}
	operation := c.Args[len(c.Args)-7]
	output := ""
	if f.fail != "before" {
		f.install(f.d.clt)
		output += "DOTFILES_CLT_RECEIPT:" + f.receipt + "\nDOTFILES_CLT_INSTALLED:" + operation + "\n"
		if f.fail != "after-install" {
			f.selected = f.d.clt
			output += "DOTFILES_CLT_SELECTED:" + operation + "\n"
		}
	}
	if f.fail != "" {
		return []byte(output), macTestExit(1)
	}
	return []byte(output), nil
}

func (f *appleCLTFixture) inspect() engine.Finding {
	f.t.Helper()
	found, err := f.d.Inspect(context.Background(), f.r, engine.Want{Present: true}, f.rec)
	if err != nil {
		f.t.Fatal(err)
	}
	return found
}

func (f *appleCLTFixture) apply() error {
	found, err := f.d.Inspect(context.Background(), f.r, engine.Want{Present: true}, f.rec)
	if err != nil {
		return err
	}
	return f.d.Apply(context.Background(), f.r, found, f.rec)
}

func TestAppleCLTFreshLifecycleUsesThePendingMarkAndReplans(t *testing.T) {
	f := newAppleCLTFixture(t)
	missing := f.inspect()
	if missing.Status != engine.Missing || !missing.Replan || !missing.Pending || !missing.Privileged || missing.Version != "" || !strings.Contains(missing.Reason, "no prior receipt") {
		t.Fatal(missing)
	}
	if len(missing.Commands) != 1 || missing.Commands[0].By != "setup" || !slices.Contains(missing.Commands[0].Lines, "sudo xcode-select --switch "+f.d.clt) {
		t.Fatal("the review lacks the setup command block", missing.Commands)
	}
	if err := f.d.Apply(context.Background(), f.r, missing, f.rec); err != nil {
		t.Fatal(err)
	}
	command := f.runner.commands[0]
	want := []string{"-n", "/usr/bin/env", "-i", "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C", "/bin/bash", "--noprofile", "--norc", "-s", "--"}
	if len(f.runner.commands) != 1 || command.Path != "/usr/bin/sudo" || !command.Elevated || !slices.Equal(command.Args[:10], want) ||
		!slices.Equal(command.Args[11:], []string{"install", f.d.clt, f.d.placeholder, "none", "", ""}) || f.runner.inputs[0] != appleCLTScript || len(command.Env) != 0 {
		t.Fatal("unexpected Apple CLT command", command.Path, command.Args)
	}
	if !slices.Equal(f.pending[0], []string{"infra.apple-clt:install"}) || len(f.rec.State().Pending) != 0 {
		t.Fatal("the pending mark did not cover exactly the native command", f.pending, f.rec.State().Pending)
	}
	if len(f.rec.State().System) != 0 {
		t.Fatal("retained Apple tools got a system record", f.rec.State().System)
	}
	after := f.inspect()
	if after.Status != engine.Satisfied || after.Token == missing.Token || after.Pending || len(after.Commands) != 0 {
		t.Fatal(after)
	}
	if err := f.d.Apply(context.Background(), f.r, after, f.rec); err == nil || len(f.runner.commands) != 1 {
		t.Fatal("present Apple tools were changed", err)
	}
	if err := f.d.Remove(context.Background(), f.r, after, f.rec); err == nil {
		t.Fatal("Apple tools were removed")
	}
}

func TestAppleCLTHistoricalReceiptAllowsMissingPayload(t *testing.T) {
	f := newAppleCLTFixture(t)
	f.receipt = "25.0.0@100"
	missing := f.inspect()
	if missing.Status != engine.Missing || missing.Version != "25.0.0" || !strings.Contains(missing.Reason, "historical receipt 25.0.0, install-time 100") {
		t.Fatal(missing)
	}
	if err := f.d.Apply(context.Background(), f.r, missing, f.rec); err != nil {
		t.Fatal(err)
	}
	if args := f.runner.commands[0].Args; args[len(args)-3] != "25.0.0@100" {
		t.Fatal("the command lost the approved receipt baseline", args)
	}
}

func TestAppleCLTUsesHealthyXcodeAndKeepsBrokenOrForeignTools(t *testing.T) {
	f := newAppleCLTFixture(t)
	xcode := filepath.Join(filepath.Dir(f.d.clt), "Xcode.app", "Contents", "Developer")
	f.install(xcode)
	f.selected = xcode
	if found := f.inspect(); found.Status != engine.Satisfied {
		t.Fatal("a healthy selected Xcode was not used", found)
	}
	f.unhealthy = true
	found := f.inspect()
	if found.Status != engine.Unknown || !strings.Contains(found.Reason, "selected Xcode") || found.Replan || found.Pending {
		t.Fatal(found)
	}
	if err := f.d.Apply(context.Background(), f.r, engine.Finding{Status: engine.Missing}, f.rec); err == nil || len(f.runner.commands) != 0 {
		t.Fatal("a foreign developer directory was replaced", err)
	}
	f = newAppleCLTFixture(t)
	if err := os.Mkdir(f.d.clt, 0755); err != nil {
		t.Fatal(err)
	}
	found = f.inspect()
	if found.Status != engine.Unknown || !strings.Contains(found.Reason, "existing Apple Command Line Tools need native repair or selection") {
		t.Fatal("a partial payload was not preserved", found)
	}
	if err := f.apply(); err == nil || len(f.runner.commands) != 0 {
		t.Fatal("a partial payload was overwritten", err)
	}
}

func TestAppleCLTFailureKeepsTheMarkAndAnUnstartedCommandClearsIt(t *testing.T) {
	for _, failure := range []string{"before", "after-install"} {
		f := newAppleCLTFixture(t)
		f.fail = failure
		if err := f.apply(); err == nil || !strings.Contains(err.Error(), "confirm that the command exited") {
			t.Fatal(failure, err)
		}
		if !slices.Equal(f.rec.State().Pending, []string{"infra.apple-clt:install"}) {
			t.Fatal("a failed command lost its pending mark", failure, f.rec.State().Pending)
		}
	}
	f := newAppleCLTFixture(t)
	f.fail = "not-started"
	if err := f.apply(); err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatal(err)
	}
	if len(f.rec.State().Pending) != 0 || !slices.Equal(f.pending[0], []string{"infra.apple-clt:install"}) {
		t.Fatal("a command that did not start kept its mark", f.rec.State().Pending)
	}
}

func TestAppleCLTUnchangedReceiptCannotProveCompletion(t *testing.T) {
	f := newAppleCLTFixture(t)
	f.receipt, f.unchanged = "26.0.0@100", true
	if err := f.apply(); err == nil || !strings.Contains(err.Error(), "installation and final selection evidence") {
		t.Fatal("an unchanged receipt proved an installation", err)
	}
	if !slices.Equal(f.rec.State().Pending, []string{"infra.apple-clt:install"}) {
		t.Fatal("an unproved installation cleared its mark")
	}
	if appleCLTReceiptChanged("26.0@100", "27.0@100") || !appleCLTReceiptChanged("none", "27.0@100") || appleCLTReceiptChanged("26.0@100", "none") {
		t.Fatal("a version change without a new install time is not an installation")
	}
}

func TestAppleCLTReceiptParserIsStrict(t *testing.T) {
	valid := "package-id: com.apple.pkg.CLTools_Executables\nversion: 27.0.0.0.1788430756\nvolume: /\nlocation: /\ninstall-time: 1789093597\n"
	if got, err := ParseAppleCLTReceipt([]byte(valid)); err != nil || got != "27.0.0.0.1788430756@1789093597" || !ValidAppleCLTReceipt(got) {
		t.Fatal(got, err)
	}
	for _, bad := range []string{"", valid + "version: 2\n", strings.Replace(valid, "volume: /", "volume: /other", 1), strings.Replace(valid, "location: /", "location: /other", 1), strings.Replace(valid, "1789093597", "0", 1), strings.Replace(valid, "1789093597", "18446744073709551616", 1), strings.Replace(valid, "package-id: com.apple.pkg.CLTools_Executables", "package-id: unrelated", 1), strings.Replace(valid, "version: 27.0.0.0.1788430756", "version: unknown", 1)} {
		if _, err := ParseAppleCLTReceipt([]byte(bad)); err == nil {
			t.Fatal("accepted malformed receipt", bad)
		}
	}
	for _, identity := range []string{"", "1.0", "1.0@0", "1.0@abc", "@1", "1.0@" + strings.Repeat("9", 19)} {
		if ValidAppleCLTReceipt(identity) {
			t.Fatal("invalid receipt identity accepted", identity)
		}
	}
	if !ValidAppleCLTReceipt("none") {
		t.Fatal("the absent receipt identity was refused")
	}
	for _, failure := range []string{"info-error", "malformed", "selection-error"} {
		f := newAppleCLTFixture(t)
		f.receipt = "26.0.0@100"
		query := f.d.Query
		f.d.Query = func(ctx context.Context, program string, args ...string) ([]byte, error) {
			if failure == "selection-error" && program == "/usr/bin/xcode-select" {
				return nil, errors.New("unexpected native selection failure")
			}
			if program == "/usr/sbin/pkgutil" && args[0] == "--pkg-info=com.apple.pkg.CLTools_Executables" {
				if failure == "info-error" {
					return nil, errors.New("receipt database unavailable")
				}
				return []byte("invalid"), nil
			}
			return query(ctx, program, args...)
		}
		if err := f.apply(); err == nil || len(f.runner.commands) != 0 {
			t.Fatal("a failed query proved absence", failure, err)
		}
	}
}

func TestAppleCLTSelectionExitBoundary(t *testing.T) {
	if os.Getenv("DOTFILES_APPLE_SELECTION_EXIT_FIXTURE") == "1" {
		os.Exit(2)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestAppleCLTSelectionExitBoundary$")
	command.Env = append(os.Environ(), "DOTFILES_APPLE_SELECTION_EXIT_FIXTURE=1")
	err := command.Run()
	diagnostic := appleCLTNoSelectionText + "\n"
	if !appleCLTSelectionMissing(err, nil, []byte(diagnostic)) {
		t.Fatal("exact exit 2 and diagnostic not recognized", err)
	}
	for _, other := range []string{"", strings.TrimSpace(diagnostic), diagnostic + "other error\n", "permission denied\n"} {
		if appleCLTSelectionMissing(err, nil, []byte(other)) {
			t.Fatal("noncanonical failure treated as absence")
		}
	}
	if appleCLTSelectionMissing(err, []byte("partial output"), []byte(diagnostic)) || appleCLTSelectionMissing(errors.New("exit status 2"), nil, []byte(diagnostic)) {
		t.Fatal("unproved native absence accepted")
	}
}

func TestAppleCLTScriptSelectsTheNewestLabelAndKeepsCompetingChanges(t *testing.T) {
	for _, scenario := range []string{"fresh", "external-marker", "no-label", "list-fails", "receipt-query-fails", "historical-receipt", "receipt-unchanged-after-install", "receipt-drift", "racing-receipt", "selection-query-fails", "racing-selection", "install-fails", "no-receipt", "missing-tool", "switch-fails", "racing-clt", "foreign-selection", "sentinel-symlink", "replaced-marker"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAppleCLTFixture(t)
			root := filepath.Dir(f.d.clt)
			writeFixture := func(name, body string) string {
				t.Helper()
				path := filepath.Join(root, name)
				macTestWrite(t, path, "#!/bin/bash\nset -eu\n"+body, 0755)
				return path
			}
			softwareupdate := writeFixture("softwareupdate", `
[[ -z "${DOTFILES_CLT_PRIVATE_TEST_SECRET+x}" ]] || exit 98
case "$1" in
	-l)
		[[ -f "$FIXTURE_ROOT/placeholder" ]] || exit 98
		if [[ "$FIXTURE_MODE" == list-fails ]]; then exit 2; fi
		if [[ "$FIXTURE_MODE" == no-label ]]; then printf 'No new software available.\n'; exit 0; fi
		if [[ "$FIXTURE_MODE" == racing-clt ]]; then /bin/mkdir "$FIXTURE_ROOT/CLT"; fi
		: > "$FIXTURE_ROOT/listed"
		printf '%s\n' 'Software Update Tool' '* Label: Command Line Tools for Xcode-16.9' ' Title: Command Line Tools for Xcode, Version: 16.9,' '* Label: unrelated OS upgrade' ' Title: macOS' '* Label: Command Line Tools for Xcode-16.10' ' Title: Command Line Tools for Xcode, Version: 16.10,'
		;;
	-i)
		[[ "$#" == 2 && "$2" == 'Command Line Tools for Xcode-16.10' ]] || exit 98
		printf '%s\n' "$2" > "$FIXTURE_ROOT/installed-label"
		if [[ "$FIXTURE_MODE" == install-fails ]]; then exit 3; fi
		/bin/mkdir -p "$FIXTURE_ROOT/CLT/usr/bin"
		for name in clang clang++ make; do
			if [[ "$FIXTURE_MODE" == missing-tool && "$name" == make ]]; then continue; fi
			printf '#!/bin/sh\nexit 0\n' > "$FIXTURE_ROOT/CLT/usr/bin/$name"
			/bin/chmod 755 "$FIXTURE_ROOT/CLT/usr/bin/$name"
		done
		if [[ "$FIXTURE_MODE" != no-receipt ]]; then : > "$FIXTURE_ROOT/receipt"; fi
		if [[ "$FIXTURE_MODE" == replaced-marker ]]; then
			printf external-marker > "$FIXTURE_ROOT/replacement"
			/bin/mv "$FIXTURE_ROOT/replacement" "$FIXTURE_ROOT/placeholder"
		fi
		;;
	*) exit 99 ;;
esac
`)
			xcodeSelect := writeFixture("xcode-select", `
case "$1" in
	--print-path)
		if [[ "$FIXTURE_MODE" == selection-query-fails ]]; then printf "unexpected native error\n" >&2; exit 2; fi
		if [[ "$FIXTURE_MODE" == foreign-selection || ( "$FIXTURE_MODE" == racing-selection && -f "$FIXTURE_ROOT/listed" ) ]]; then printf '/private/foreign/Xcode.app/Contents/Developer\n'; exit 0; fi
		if [[ -f "$FIXTURE_ROOT/selected" ]]; then /bin/cat "$FIXTURE_ROOT/selected"; else printf '%s\n' '`+appleCLTNoSelectionText+`' >&2; exit 2; fi
		;;
	--switch)
		[[ "$#" == 2 && "$2" == "$FIXTURE_ROOT/CLT" ]] || exit 98
		if [[ "$FIXTURE_MODE" == switch-fails ]]; then exit 4; fi
		printf '%s\n' "$2" > "$FIXTURE_ROOT/selected"
		;;
	*) exit 99 ;;
esac
`)
			pkgutil := writeFixture("pkgutil", `
case "$1" in
	--pkgs=*) exit 1 ;;
	--pkgs)
		if [[ "$FIXTURE_MODE" == receipt-query-fails && -f "$FIXTURE_ROOT/listed" ]]; then exit 7; fi
		printf 'unrelated.package\ncom.apple.pkg.CLTools_Executables.extra\n'
		if [[ -f "$FIXTURE_ROOT/receipt" || "$FIXTURE_MODE" == historical-receipt || "$FIXTURE_MODE" == receipt-unchanged-after-install || "$FIXTURE_MODE" == receipt-drift || ( "$FIXTURE_MODE" == racing-receipt && -f "$FIXTURE_ROOT/listed" ) ]]; then printf 'com.apple.pkg.CLTools_Executables\n'; fi
		;;
	--pkg-info=com.apple.pkg.CLTools_Executables)
		timestamp=100
		if [[ -f "$FIXTURE_ROOT/receipt" && "$FIXTURE_MODE" != receipt-unchanged-after-install ]]; then timestamp=200; fi
		if [[ "$FIXTURE_MODE" == receipt-drift && -f "$FIXTURE_ROOT/listed" ]]; then timestamp=150; fi
		printf 'package-id: com.apple.pkg.CLTools_Executables\nversion: 16.10\nvolume: /\nlocation: /\ninstall-time: %s\n' "$timestamp"
		;;
	*) exit 99 ;;
esac
`)
			baseline := "none"
			if scenario == "historical-receipt" || scenario == "receipt-unchanged-after-install" || scenario == "receipt-drift" {
				baseline = "16.10@100"
			}
			operation := strings.Repeat("a", 64)
			command, err := f.d.command(operation, baseline, "")
			if err != nil {
				t.Fatal(err)
			}
			script := strings.NewReplacer("/usr/sbin/softwareupdate", softwareupdate, "/usr/bin/xcode-select", xcodeSelect, "/usr/sbin/pkgutil", pkgutil).Replace(appleCLTScript)
			if runtime.GOOS != "darwin" {
				stat := writeFixture("stat", `exec /usr/bin/stat -c '%d:%i' "$3"`)
				script = strings.ReplaceAll(script, "/usr/bin/stat", stat)
			}
			command.Path, command.Elevated, command.Stdin = "/usr/bin/env", false, strings.NewReader(script)
			command.Args = append([]string{"-i", "FIXTURE_ROOT=" + root, "FIXTURE_MODE=" + scenario}, command.Args[3:]...)
			t.Setenv("DOTFILES_CLT_PRIVATE_TEST_SECRET", "public-fixture-not-for-inheritance")
			switch scenario {
			case "external-marker":
				macTestWrite(t, f.d.placeholder, "external-marker", 0600)
			case "sentinel-symlink":
				macTestWrite(t, filepath.Join(root, "external-file"), "external-marker", 0600)
				if err := os.Symlink(filepath.Join(root, "external-file"), f.d.placeholder); err != nil {
					t.Fatal(err)
				}
			}
			output, runErr := platform.Exec{}.Run(context.Background(), command)
			success := scenario == "fresh" || scenario == "external-marker" || scenario == "historical-receipt"
			if (runErr == nil) != success {
				t.Fatalf("unexpected outcome: %v %s", runErr, output)
			}
			if success && !appleCLTMarker(output, "SELECTED", operation) {
				t.Fatal("a successful run lacks its selection marker", string(output))
			}
			if appleCLTMarker(output, "INSTALLED", operation) != (success || scenario == "switch-fails" || scenario == "replaced-marker") {
				t.Fatal("incorrect installation marker", string(output))
			}
			if success {
				label, err := os.ReadFile(filepath.Join(root, "installed-label"))
				if err != nil || string(label) != "Command Line Tools for Xcode-16.10\n" || !strings.Contains(string(output), "DOTFILES_CLT_LABEL:Command Line Tools for Xcode-16.10\n") {
					t.Fatal("the script did not select the newest Command Line Tools label", string(label), err)
				}
				if receipt, err := appleCLTInstalledReceipt(output); err != nil || receipt != "16.10@200" {
					t.Fatal(receipt, err)
				}
			}
			if scenario == "receipt-query-fails" || scenario == "receipt-drift" || scenario == "selection-query-fails" || scenario == "racing-receipt" || scenario == "racing-clt" || scenario == "foreign-selection" || scenario == "racing-selection" || scenario == "no-label" {
				if _, err := os.Lstat(filepath.Join(root, "installed-label")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unproved absence reached native installation", err)
				}
			}
			if scenario == "external-marker" || scenario == "sentinel-symlink" || scenario == "replaced-marker" {
				data, err := os.ReadFile(f.d.placeholder)
				if err != nil || string(data) != "external-marker" {
					t.Fatal("external discovery file changed", err)
				}
			} else if _, err := os.Lstat(f.d.placeholder); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the temporary discovery file stayed", err)
			}
		})
	}
}
