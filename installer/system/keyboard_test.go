//go:build darwin || linux

package system

import (
	"context"
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
	"github.com/lgmr-dev/dotfiles/installer/state"
)

var keyboardFixtureResource = catalog.Resource{ID: "system.keyboard-access", Name: "Keyboard access", Action: "keyboard-access", Scope: "user"}

const keyboardFixtureMark = "system.keyboard-access:rule"

var keyboardFixtureReload = []string{
	"udevadm control --reload-rules",
	"udevadm trigger --action=change --subsystem-match=misc --sysname-match=uinput",
	"udevadm trigger --action=change --subsystem-match=input",
}

type keyboardFixture struct {
	t              *testing.T
	k              *KeyboardAccess
	rec            *aptFixtureRecords
	log            []string
	authentication int
	commands       []platform.Command
	fail           map[string]error
	during         func(name string)
}

func newKeyboardFixture(t *testing.T) *keyboardFixture {
	t.Helper()
	root, err := platform.ResolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rules := filepath.Join(root, "etc", "udev", "rules.d")
	if err := os.MkdirAll(rules, 0755); err != nil {
		t.Fatal(err)
	}
	f := &keyboardFixture{t: t, rec: newAPTFixtureRecords(), fail: map[string]error{}}
	f.k = NewKeyboardAccess(&APTLocation{Sudo: "/fake/sudo"}, "", filepath.Join(root, "state"))
	f.k.Rules, f.k.Owner, f.k.Runner = filepath.Join(rules, "70-dotfiles-mac-keyboard.rules"), os.Getuid(), f
	f.k.Authenticate = func(context.Context) error { f.authentication++; return nil }
	return f
}

func (f *keyboardFixture) Run(ctx context.Context, c platform.Command) ([]byte, error) {
	if c.Path != "/fake/sudo" || len(c.Args) < 3 || c.Args[0] != "-n" || c.Args[1] != "--" || !slices.Equal(c.Env, []string{"LC_ALL=C"}) {
		f.t.Fatal("each command must use the non-interactive sudo line", c)
	}
	f.commands = append(f.commands, c)
	name, args := filepath.Base(c.Args[2]), c.Args[3:]
	if filepath.Dir(c.Args[2]) != "/usr/bin" {
		f.t.Fatal("the recipe must use the fixed system programs", c.Args[2])
	}
	f.log = append(f.log, name+" "+strings.Join(args, " "))
	if err := f.fail[name]; err != nil {
		return nil, err
	}
	if f.during != nil {
		f.during(name)
	}
	switch name {
	case "install":
		if !slices.Equal(args[:6], []string{"-m", "0644", "-o", "root", "-g", "root"}) || args[6] != "-T" {
			f.t.Fatal(args)
		}
		data, err := os.ReadFile(args[7])
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(args[8], data, 0644); err != nil {
			return nil, err
		}
		return nil, os.Chmod(args[8], 0644)
	case "ln":
		if _, err := os.Lstat(args[2]); err == nil {
			return []byte("ln: failed to create hard link"), aptFixtureExit(1)
		}
		return nil, os.Link(args[1], args[2])
	case "rm":
		if err := os.Remove(args[2]); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	case "mv":
		if !slices.Equal(args[:2], []string{"-n", "-T"}) {
			f.t.Fatal("the move must never replace a file", args)
		}
		if _, err := os.Lstat(args[3]); err == nil {
			return nil, nil
		}
		return nil, os.Rename(args[2], args[3])
	}
	return nil, nil
}

func (f *keyboardFixture) inspect(present bool) engine.Finding {
	f.t.Helper()
	found, err := f.k.Inspect(context.Background(), keyboardFixtureResource, engine.Want{Present: present}, f.rec)
	if err != nil {
		f.t.Fatal(err)
	}
	return found
}

func (f *keyboardFixture) entries() []string {
	f.t.Helper()
	entries, err := os.ReadDir(filepath.Dir(f.k.Rules))
	if err != nil {
		f.t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestKeyboardRuleBytes(t *testing.T) {
	want := "# Managed by dotfiles (Mac keyboard shortcuts).\n" +
		"# Lets the user logged in at this seat read keyboards and create a virtual\n" +
		"# keyboard for the xremap key remapper. Unselect the tool to remove it.\n" +
		"KERNEL==\"uinput\", SUBSYSTEM==\"misc\", TAG+=\"uaccess\", OPTIONS+=\"static_node=uinput\"\n" +
		"SUBSYSTEM==\"input\", KERNEL==\"event*\", ENV{ID_INPUT_KEYBOARD}==\"1\", TAG+=\"uaccess\"\n"
	rule := string(KeyboardRule())
	if rule != want {
		t.Fatalf("the rule bytes changed:\n%s", rule)
	}
	if strings.Contains(rule, "GROUP=") || strings.Contains(rule, "MODE") || strings.Contains(rule, "operation") {
		t.Fatal("access must follow the seat (uaccess), never a group or an open mode, and the header names no operation", rule)
	}
	if KeyboardRulePath != "/etc/udev/rules.d/70-dotfiles-mac-keyboard.rules" {
		t.Fatal("the rule must sort before 73-seat-late.rules", KeyboardRulePath)
	}
	if got := KeyboardRuleFingerprint([]byte(want)); !strings.HasPrefix(got, "sha256:") || len(got) != 71 || got == KeyboardRuleFingerprint([]byte(want+"\n")) {
		t.Fatal(got)
	}
}

func TestKeyboardAccessRuleLifecycle(t *testing.T) {
	f := newKeyboardFixture(t)
	ctx := context.Background()
	r := keyboardFixtureResource
	found := f.inspect(true)
	if found.Status != engine.Missing || !found.Privileged || !found.Pending || len(found.Commands) != 1 || found.Commands[0].By != "setup" || len(found.Commands[0].Lines) != 9 ||
		!strings.HasPrefix(found.Commands[0].Lines[2], "LC_ALL=C /fake/sudo -n -- /usr/bin/install -m 0644 -o root -g root -T ") || found.Commands[0].Lines[5] != "LC_ALL=C /fake/sudo -n -- /usr/bin/udevadm control --reload-rules" {
		t.Fatal("an absent rule must be installable with administrator rights", found)
	}
	fingerprint := KeyboardRuleFingerprint(KeyboardRule())
	f.during = func(name string) {
		st := f.rec.State()
		record := st.Targets[f.k.Rules]
		if record.Resource != r.ID || record.Kind != "file" || record.Next != fingerprint || record.Applied != "" || record.Before != "absent" {
			t.Fatal("the record must hold the intended value before the first command", name, record)
		}
		if !slices.Equal(st.Pending, []string{keyboardFixtureMark}) {
			t.Fatal("the pending mark must cover the write, the reload and the triggers", name, st.Pending)
		}
	}
	if err := f.k.Apply(ctx, r, found, f.rec); err != nil {
		t.Fatal(err)
	}
	stage, tmp := filepath.Join(f.k.Directory, "keyboard-access.rules"), filepath.Join(filepath.Dir(f.k.Rules), ".dotfiles-mac-keyboard.tmp")
	if !slices.Equal(f.log, append([]string{
		"install -m 0644 -o root -g root -T " + stage + " " + tmp,
		"ln -T " + tmp + " " + f.k.Rules,
		"rm -f -- " + tmp,
	}, keyboardFixtureReload...)) || f.authentication != 1 {
		t.Fatal("the write and the reload must run in the fixed order after one authentication", f.log, f.authentication)
	}
	data, err := os.ReadFile(f.k.Rules)
	if info, statErr := os.Stat(f.k.Rules); err != nil || statErr != nil || string(data) != string(KeyboardRule()) || info.Mode().Perm() != 0644 {
		t.Fatal("the rule must be the exact content, mode 0644", string(data), err)
	}
	if got := f.entries(); len(got) != 1 {
		t.Fatal("the temporary copy must not remain beside the rule", got)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the private input must be removed after completion", err)
	}
	st := f.rec.State()
	if record := st.Targets[f.k.Rules]; record.Applied != fingerprint || record.Next != "" || record.Before != "absent" || len(st.Pending) != 0 {
		t.Fatal("the verified reload must promote the record and clear the mark", record, st.Pending)
	}
	if found := f.inspect(true); found.Status != engine.Satisfied || len(found.Commands) != 0 {
		t.Fatal(found)
	}
	f.during = nil

	if err := os.WriteFile(f.k.Rules, append(data, "# local\n"...), 0644); err != nil {
		t.Fatal(err)
	}
	for _, present := range []bool{true, false} {
		if found := f.inspect(present); found.Status != engine.Changed || !strings.Contains(found.Reason, "changed after setup installed it") || !slices.Equal(found.Preserved, []string{f.k.Rules}) {
			t.Fatal("a rule changed outside setup must be preserved", found)
		}
	}
	f.log = nil
	if err := f.k.Remove(ctx, r, engine.Finding{}, f.rec); err == nil || len(f.log) != 0 {
		t.Fatal("removed a changed rule", err, f.log)
	}
	if err := f.k.Apply(ctx, r, engine.Finding{Replace: true}, f.rec); err == nil || len(f.log) != 0 {
		t.Fatal("replaced a changed rule", err, f.log)
	}
	if changed, err := os.ReadFile(f.k.Rules); err != nil || !strings.HasSuffix(string(changed), "# local\n") {
		t.Fatal("the changed rule was not preserved", err)
	}
	if err := os.WriteFile(f.k.Rules, data, 0644); err != nil {
		t.Fatal(err)
	}

	found = f.inspect(false)
	if found.Status != engine.Satisfied || len(found.Commands) != 1 || !strings.HasPrefix(found.Commands[0].Lines[0], "LC_ALL=C /fake/sudo -n -- /usr/bin/mv -n -T ") {
		t.Fatal(found)
	}
	f.during = func(name string) {
		st := f.rec.State()
		if record := st.Targets[f.k.Rules]; !record.Release || record.Next != "absent" || record.Applied != fingerprint {
			t.Fatal("the record must hold the unfinished removal before the first command", name, record)
		}
		if !slices.Equal(st.Pending, []string{keyboardFixtureMark}) {
			t.Fatal(name, st.Pending)
		}
	}
	if err := f.k.Remove(ctx, r, found, f.rec); err != nil {
		t.Fatal(err)
	}
	aside := filepath.Join(filepath.Dir(f.k.Rules), ".dotfiles-mac-keyboard.removing")
	if !slices.Equal(f.log, append([]string{"mv -n -T " + f.k.Rules + " " + aside, "rm -f -- " + aside}, keyboardFixtureReload...)) {
		t.Fatal("the removal must move, delete and reload in order", f.log)
	}
	st = f.rec.State()
	if got := f.entries(); len(got) != 0 || len(st.Targets) != 0 || len(st.Pending) != 0 {
		t.Fatal("removal left the rule, its moved copy, its record or its mark", got, st.Targets, st.Pending)
	}
	if found := f.inspect(false); found.Status != engine.Absent {
		t.Fatal(found)
	}
}

func TestKeyboardAccessNeverReplacesAnotherRule(t *testing.T) {
	f := newKeyboardFixture(t)
	ctx := context.Background()
	if err := os.WriteFile(f.k.Rules, []byte("# personal rule\n"), 0644); err != nil {
		t.Fatal(err)
	}
	found := f.inspect(true)
	if found.Status != engine.Blocked || !strings.Contains(found.Reason, "never replaces or removes it") {
		t.Fatal(found)
	}
	if err := f.k.Apply(ctx, keyboardFixtureResource, found, f.rec); err == nil || len(f.log) != 0 || len(f.rec.State().Targets) != 0 {
		t.Fatal("setup wrote over a personal rule or recorded it", err, f.log)
	}
	if err := f.k.Remove(ctx, keyboardFixtureResource, found, f.rec); err != nil || len(f.log) != 0 {
		t.Fatal("setup removed a rule without a record", err, f.log)
	}
	if data, err := os.ReadFile(f.k.Rules); err != nil || string(data) != "# personal rule\n" {
		t.Fatal("the personal rule changed", string(data), err)
	}

	if err := os.WriteFile(f.k.Rules, KeyboardRule(), 0644); err != nil {
		t.Fatal(err)
	}
	if found := f.inspect(true); found.Status != engine.Satisfied || len(found.Notes) != 1 || len(f.rec.State().Targets) != 0 {
		t.Fatal("equal bytes must be reused without ownership", found)
	}

	if err := os.Remove(f.k.Rules); err != nil {
		t.Fatal(err)
	}
	found = f.inspect(true)
	f.during = func(name string) {
		if name == "ln" {
			if err := os.WriteFile(f.k.Rules, []byte("# appeared\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := f.k.Apply(ctx, keyboardFixtureResource, found, f.rec); err == nil {
		t.Fatal("the link step replaced a rule that appeared after approval")
	}
	if data, err := os.ReadFile(f.k.Rules); err != nil || string(data) != "# appeared\n" || slices.Contains(f.log, keyboardFixtureReload[0]) {
		t.Fatal("the appeared rule changed, or udev reloaded without a write", string(data), f.log)
	}
	if found := f.inspect(true); found.Status != engine.Changed {
		t.Fatal("a file that differs from the intended value must stay", found)
	}
}

func TestKeyboardAccessRetryRepeatsTheReloadWhenTheBytesMatch(t *testing.T) {
	f := newKeyboardFixture(t)
	ctx := context.Background()
	r := keyboardFixtureResource
	f.during = func(name string) {
		if name == "udevadm" && len(f.log) == 5 {
			f.fail["udevadm"] = aptFixtureExit(1)
		}
	}
	if err := f.k.Apply(ctx, r, f.inspect(true), f.rec); err == nil || !strings.Contains(err.Error(), "confirm that the command exited") {
		t.Fatal(err)
	}
	st := f.rec.State()
	fingerprint := KeyboardRuleFingerprint(KeyboardRule())
	if record := st.Targets[f.k.Rules]; record.Next != fingerprint || record.Applied != "" || !slices.Equal(st.Pending, []string{keyboardFixtureMark}) {
		t.Fatal("a failed trigger must leave the write unfinished and the mark in place", record, st.Pending)
	}
	if err := f.rec.Update(func(s *state.State) error { s.Clear(keyboardFixtureMark); return nil }); err != nil {
		t.Fatal(err)
	}
	f.fail, f.during, f.log = map[string]error{}, nil, nil
	found := f.inspect(true)
	if found.Status != engine.Unhealthy || !strings.Contains(found.Reason, "repeats the reload") {
		t.Fatal(found)
	}
	if err := f.k.Apply(ctx, r, found, f.rec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.log, keyboardFixtureReload) {
		t.Fatal("the retry must repeat the reload and the triggers and must not write the rule again", f.log)
	}
	st = f.rec.State()
	if record := st.Targets[f.k.Rules]; record.Applied != fingerprint || record.Next != "" || len(st.Pending) != 0 || f.inspect(true).Status != engine.Satisfied {
		t.Fatal(record, st.Pending)
	}

	f = newKeyboardFixture(t)
	f.fail["udevadm"] = errors.New("fork/exec /fake/sudo: no such file or directory")
	if err := f.k.Apply(ctx, r, f.inspect(true), f.rec); err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatal(err)
	}
	st = f.rec.State()
	if record := st.Targets[f.k.Rules]; record.Next != fingerprint || len(st.Pending) != 0 {
		t.Fatal("a reload that did not start must clear its mark and keep the unfinished write", record, st.Pending)
	}
}

func TestKeyboardAccessKeepsARecordedRuleOfAnEarlierRelease(t *testing.T) {
	f := newKeyboardFixture(t)
	ctx := context.Background()
	earlier := []byte("# Managed by dotfiles (Mac keyboard shortcuts), operation " + strings.Repeat("a", 64) + ".\n" + keyboardRuleBody)
	if err := os.WriteFile(f.k.Rules, earlier, 0644); err != nil {
		t.Fatal(err)
	}
	if found := f.inspect(true); found.Status != engine.Blocked {
		t.Fatal("equal rule lines with another header give no ownership without a record", found)
	}
	f.rec.st.Targets[f.k.Rules] = state.Record{Resource: keyboardFixtureResource.ID, Kind: "file", Applied: KeyboardRuleFingerprint(earlier), Before: "absent"}
	if found := f.inspect(true); found.Status != engine.Satisfied {
		t.Fatal("a rule that matches its record must stay as it is", found)
	}
	if err := f.k.Remove(ctx, keyboardFixtureResource, f.inspect(false), f.rec); err != nil {
		t.Fatal(err)
	}
	if got := f.entries(); len(got) != 0 || len(f.rec.State().Targets) != 0 {
		t.Fatal(got)
	}
}

func TestKeyboardAccessCompletesAnInterruptedRemoval(t *testing.T) {
	f := newKeyboardFixture(t)
	ctx := context.Background()
	r := keyboardFixtureResource
	if err := f.k.Apply(ctx, r, f.inspect(true), f.rec); err != nil {
		t.Fatal(err)
	}
	f.fail["rm"] = aptFixtureExit(1)
	if err := f.k.Remove(ctx, r, f.inspect(false), f.rec); err == nil {
		t.Fatal("the removal must stop at the failed delete")
	}
	aside := filepath.Join(filepath.Dir(f.k.Rules), ".dotfiles-mac-keyboard.removing")
	if got := f.entries(); !slices.Equal(got, []string{filepath.Base(aside)}) || !f.rec.State().Targets[f.k.Rules].Release {
		t.Fatal(got)
	}
	f.fail, f.log = map[string]error{}, nil
	found := f.inspect(false)
	if found.Status != engine.Absent || len(found.Commands) != 1 {
		t.Fatal(found)
	}
	if err := f.k.Remove(ctx, r, found, f.rec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.log, append([]string{"rm -f -- " + aside}, keyboardFixtureReload...)) || len(f.entries()) != 0 || len(f.rec.State().Targets) != 0 {
		t.Fatal("the retry must delete the moved copy, reload and drop the record", f.log)
	}
}

func TestKeyboardAccessReportsMissingAdministratorTools(t *testing.T) {
	k := NewKeyboardAccess(nil, "sudo is unavailable", "/state/keyboard-access")
	found, err := k.Inspect(context.Background(), keyboardFixtureResource, engine.Want{Present: true}, newAPTFixtureRecords())
	if err != nil || found.Status != engine.Unknown || !strings.Contains(found.Reason, "sudo is unavailable") {
		t.Fatal(found, err)
	}
	if err := k.Apply(context.Background(), keyboardFixtureResource, found, newAPTFixtureRecords()); err == nil {
		t.Fatal("a provider without administrator tools must not change anything")
	}
	root := NewKeyboardAccess(&APTLocation{Root: true}, "", "/state/keyboard-access")
	if c := root.reloadCommands()[0]; c.Path != "/usr/bin/udevadm" || !slices.Equal(c.Args, []string{"control", "--reload-rules"}) {
		t.Fatal("root must run the recipe without sudo", c)
	}
}

func TestKeyboardAccessMarksBeforeTheFirstPrivilegedCommand(t *testing.T) {
	f := newKeyboardFixture(t)
	ctx := context.Background()
	r := keyboardFixtureResource
	seen := []string{}
	f.during = func(name string) {
		if st := f.rec.State(); len(st.Pending) != 1 {
			t.Fatal("each privileged command of the step must run under the pending mark", name, st.Pending)
		}
		seen = append(seen, name)
	}
	found := f.inspect(true)
	if err := f.k.Apply(ctx, r, found, f.rec); err != nil || !slices.Equal(seen, []string{"install", "ln", "rm", "udevadm", "udevadm", "udevadm"}) || len(f.rec.State().Pending) != 0 {
		t.Fatal(err, seen, f.rec.State().Pending)
	}
	if shown := aptFixtureShellLines(found.Commands[0]); len(shown) != len(f.commands) {
		t.Fatal(shown)
	} else {
		for i, c := range f.commands {
			if shown[i] != aptFixtureLine(c) {
				t.Fatal("the shown line differs from the command that ran", shown[i], aptFixtureLine(c))
			}
		}
	}
	seen, f.commands = nil, nil
	found = f.inspect(false)
	if err := f.k.Remove(ctx, r, found, f.rec); err != nil || !slices.Equal(seen, []string{"mv", "rm", "udevadm", "udevadm", "udevadm"}) || len(f.rec.State().Pending) != 0 {
		t.Fatal(err, seen, f.rec.State().Pending)
	}
	for i, c := range f.commands {
		if found.Commands[0].Lines[i] != aptFixtureLine(c) {
			t.Fatal("the shown line differs from the command that ran", found.Commands[0].Lines[i], aptFixtureLine(c))
		}
	}

	f = newKeyboardFixture(t)
	f.fail["install"] = errors.New("fork/exec /fake/sudo: no such file or directory")
	if err := f.k.Apply(ctx, r, f.inspect(true), f.rec); err == nil || len(f.rec.State().Pending) != 0 {
		t.Fatal("a write that did not start must clear its mark", err, f.rec.State().Pending)
	}
	f.fail["install"] = aptFixtureExit(1)
	if err := f.k.Apply(ctx, r, f.inspect(true), f.rec); err == nil || len(f.rec.State().Pending) != 1 {
		t.Fatal("a write that started and failed must keep its mark", err, f.rec.State().Pending)
	}
}

func TestKeyboardAccessShownInstallBlockStagesTheRuleThatApplyStages(t *testing.T) {
	f := newKeyboardFixture(t)
	lines := f.inspect(true).Commands[0].Lines
	privileged := func(line string) bool { return strings.Contains(line, "/fake/sudo") }
	first, after := slices.IndexFunc(lines, privileged), len(lines)
	for after > 0 && !privileged(lines[after-1]) {
		after--
	}
	shell := func(lines []string) error {
		for _, line := range lines {
			if strings.HasPrefix(line, "#") {
				continue
			}
			cmd := exec.Command("/bin/sh", "-c", line)
			cmd.Dir = t.TempDir()
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("%s: %w: %s", line, err, out)
			}
		}
		return nil
	}
	stage := filepath.Join(f.k.Directory, "keyboard-access.rules")
	if err := shell(lines[:first]); err != nil {
		t.Fatal(err)
	}
	shown, err := os.ReadFile(stage)
	if err != nil {
		t.Fatal("the shown block must create the staged rule before the first privileged command", lines, err)
	}
	shownFile, _ := os.Stat(stage)
	shownDirectory, _ := os.Stat(f.k.Directory)
	if err := os.WriteFile(stage, []byte("# personal\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := shell(lines[:first]); err == nil {
		t.Fatal("the shown block must never overwrite an existing staged file")
	}
	if data, err := os.ReadFile(stage); err != nil || string(data) != "# personal\n" {
		t.Fatal("the shown block overwrote an existing staged file", string(data), err)
	}
	if err := shell(lines[after:]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the shown block must delete the staged rule after the privileged commands", err)
	}
	if err := os.Remove(f.k.Directory); err != nil {
		t.Fatal(err)
	}
	var staged []byte
	var stagedFile, stagedDirectory os.FileInfo
	f.during = func(name string) {
		if name == "install" {
			staged, _ = os.ReadFile(stage)
			stagedFile, _ = os.Stat(stage)
			stagedDirectory, _ = os.Stat(f.k.Directory)
		}
	}
	if err := f.k.Apply(context.Background(), keyboardFixtureResource, f.inspect(true), f.rec); err != nil {
		t.Fatal(err)
	}
	if string(shown) != string(staged) || shownFile.Mode() != stagedFile.Mode() || shownDirectory.Mode() != stagedDirectory.Mode() {
		t.Fatal("the shown block must stage the bytes and modes that setup stages", string(shown), string(staged), shownFile.Mode(), stagedFile.Mode(), shownDirectory.Mode(), stagedDirectory.Mode())
	}
}

func TestKeyboardAccessNeverOverwritesAnUnrecordedStagingName(t *testing.T) {
	ctx := context.Background()
	r := keyboardFixtureResource

	f := newKeyboardFixture(t)
	tmp := filepath.Join(filepath.Dir(f.k.Rules), ".dotfiles-mac-keyboard.tmp")
	if err := os.WriteFile(tmp, []byte("# personal\n"), 0644); err != nil {
		t.Fatal(err)
	}
	found := f.inspect(true)
	if found.Status != engine.Blocked || !strings.Contains(found.Reason, tmp) {
		t.Fatal("an unrecorded file at the temporary name must stop the step and be named", found)
	}
	if err := f.k.Apply(ctx, r, found, f.rec); err == nil || !strings.Contains(err.Error(), tmp) || len(f.log) != 0 || len(f.rec.State().Pending) != 0 {
		t.Fatal(err, f.log)
	}
	if data, err := os.ReadFile(tmp); err != nil || string(data) != "# personal\n" {
		t.Fatal("setup overwrote an unrecorded file", string(data), err)
	}

	f = newKeyboardFixture(t)
	if err := f.k.Apply(ctx, r, f.inspect(true), f.rec); err != nil {
		t.Fatal(err)
	}
	rule, _ := os.ReadFile(f.k.Rules)
	aside := filepath.Join(filepath.Dir(f.k.Rules), ".dotfiles-mac-keyboard.removing")
	if err := os.WriteFile(aside, append(slices.Clone(rule), "# local\n"...), 0644); err != nil {
		t.Fatal(err)
	}
	f.log = nil
	found = f.inspect(false)
	if found.Status != engine.Blocked || !strings.Contains(found.Reason, aside) {
		t.Fatal("a changed moved copy must stop the removal and be named", found)
	}
	if err := f.k.Remove(ctx, r, found, f.rec); err == nil || !strings.Contains(err.Error(), aside) || len(f.log) != 0 || len(f.rec.State().Pending) != 0 {
		t.Fatal(err, f.log)
	}
	if data, err := os.ReadFile(aside); err != nil || !strings.HasSuffix(string(data), "# local\n") {
		t.Fatal("setup overwrote the preserved moved copy", string(data), err)
	}
	if data, err := os.ReadFile(f.k.Rules); err != nil || string(data) != string(rule) {
		t.Fatal("the rule must stay while the moved copy blocks its removal", err)
	}

	f = newKeyboardFixture(t)
	f.fail["ln"] = aptFixtureExit(1)
	if err := f.k.Apply(ctx, r, f.inspect(true), f.rec); err == nil {
		t.Fatal("the link step must fail")
	}
	if err := f.rec.Update(func(s *state.State) error { s.Clear(keyboardFixtureMark); return nil }); err != nil {
		t.Fatal(err)
	}
	f.fail, f.log = map[string]error{}, nil
	found = f.inspect(true)
	if found.Status != engine.Missing {
		t.Fatal("a temporary copy that equals the recorded intended value must be recovered", found)
	}
	if err := f.k.Apply(ctx, r, found, f.rec); err != nil || f.log[0] != "rm -f -- "+tmpOf(f) || len(f.entries()) != 1 {
		t.Fatal(err, f.log, f.entries())
	}
}

func tmpOf(f *keyboardFixture) string {
	return filepath.Join(filepath.Dir(f.k.Rules), ".dotfiles-mac-keyboard.tmp")
}
