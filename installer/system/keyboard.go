package system

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

// KeyboardRulePath is the udev rule that gives the user at the local seat
// access to the keyboards and to the virtual input device of xremap. It is
// the only permanent root-owned file that setup writes itself.
const KeyboardRulePath = "/etc/udev/rules.d/70-dotfiles-mac-keyboard.rules"

const keyboardRuleBody = `KERNEL=="uinput", SUBSYSTEM=="misc", TAG+="uaccess", OPTIONS+="static_node=uinput"
SUBSYSTEM=="input", KERNEL=="event*", ENV{ID_INPUT_KEYBOARD}=="1", TAG+="uaccess"
`

// KeyboardRule returns the bytes of the rule that setup installs. The rule
// tags the devices for the seat access lists of logind (uaccess), which
// 73-seat-late.rules applies, so the rule file name must sort before it.
func KeyboardRule() []byte {
	return []byte("# Managed by dotfiles (Mac keyboard shortcuts).\n" +
		"# Lets the user logged in at this seat read keyboards and create a virtual\n" +
		"# keyboard for the xremap key remapper. Unselect the tool to remove it.\n" + keyboardRuleBody)
}

// KeyboardRuleFingerprint returns the value that the target record of the
// rule holds for these bytes.
func KeyboardRuleFingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// KeyboardPrograms holds the absolute paths of the programs of the rule
// recipe.
type KeyboardPrograms struct{ Install, Link, Remove, Move, Udevadm string }

// KeyboardAccess is the provider of system.keyboard-access. It writes the
// rule through sudo without replacing a file, records it as a file target
// and reloads udev. It removes the rule only while the rule matches its
// record.
//
// Directory is a private folder in the state folder for the staged rule.
// Sudo is empty when the process runs as root. Owner is the account that
// must own the rule: root, except in fixtures. Issue is the reason why the
// administrator tools are unavailable.
type KeyboardAccess struct {
	Directory    string
	Sudo         string
	Rules        string
	Programs     KeyboardPrograms
	Owner        int
	Issue        string
	Runner       platform.Runner
	Authenticate func(context.Context) error
}

// NewKeyboardAccess returns the provider. A nil location means that the
// discovery of the administrator tools failed with issue. It is pure.
func NewKeyboardAccess(location *APTLocation, issue, directory string) *KeyboardAccess {
	k := &KeyboardAccess{Directory: directory, Rules: KeyboardRulePath, Runner: platform.Exec{},
		Programs: KeyboardPrograms{Install: "/usr/bin/install", Link: "/usr/bin/ln", Remove: "/usr/bin/rm", Move: "/usr/bin/mv", Udevadm: "/usr/bin/udevadm"}}
	switch {
	case location == nil:
		k.Issue = "keyboard access needs the system administrator tools that APT setup uses"
		if issue != "" {
			k.Issue += ": " + issue
		}
	case !location.Root:
		k.Sudo = location.Sudo
	}
	return k
}

const keyboardStep = "rule"

func (k *KeyboardAccess) stage() string { return filepath.Join(k.Directory, "keyboard-access.rules") }

func (k *KeyboardAccess) sibling(suffix string) string {
	return filepath.Join(filepath.Dir(k.Rules), ".dotfiles-mac-keyboard"+suffix)
}

func (k *KeyboardAccess) validate(r catalog.Resource) error {
	if r.ID != "system.keyboard-access" {
		return errors.New("invalid keyboard access resource")
	}
	for _, path := range []string{k.Directory, k.Rules, k.Programs.Install, k.Programs.Link, k.Programs.Remove, k.Programs.Move, k.Programs.Udevadm} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return errors.New("keyboard access requires canonical absolute paths")
		}
	}
	if k.Sudo != "" && !filepath.IsAbs(k.Sudo) {
		return errors.New("keyboard access requires the observed absolute sudo")
	}
	return nil
}

func (k *KeyboardAccess) command(program string, args ...string) platform.Command {
	if k.Sudo == "" {
		return platform.Command{Path: program, Args: args, Env: []string{"LC_ALL=C"}}
	}
	return platform.Command{Path: k.Sudo, Args: append([]string{"-n", "--", program}, args...), Env: []string{"LC_ALL=C"}, Elevated: true}
}

func (k *KeyboardAccess) writeCommands() []platform.Command {
	tmp := k.sibling(".tmp")
	return []platform.Command{
		k.command(k.Programs.Install, "-m", "0644", "-o", "root", "-g", "root", "-T", k.stage(), tmp),
		k.command(k.Programs.Link, "-T", tmp, k.Rules),
		k.command(k.Programs.Remove, "-f", "--", tmp),
	}
}

func (k *KeyboardAccess) removeCommands() []platform.Command {
	aside := k.sibling(".removing")
	return []platform.Command{k.command(k.Programs.Move, "-T", k.Rules, aside), k.command(k.Programs.Remove, "-f", "--", aside)}
}

func (k *KeyboardAccess) reloadCommands() []platform.Command {
	return []platform.Command{
		k.command(k.Programs.Udevadm, "control", "--reload-rules"),
		k.command(k.Programs.Udevadm, "trigger", "--action=change", "--subsystem-match=misc", "--sysname-match=uinput"),
		k.command(k.Programs.Udevadm, "trigger", "--action=change", "--subsystem-match=input"),
	}
}

func (k *KeyboardAccess) block(title string, commands ...platform.Command) engine.CommandBlock {
	block := engine.CommandBlock{Title: title, By: "setup"}
	for _, c := range commands {
		block.Lines = append(block.Lines, aptShellLine(c))
	}
	return block
}

func keyboardRead(path string) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return nil, info, fmt.Errorf("%s is not a small regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, info, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, 4097))
	if err := errors.Join(readErr, f.Close()); err != nil {
		return nil, info, err
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, after) || int64(len(data)) != info.Size() {
		return nil, info, errors.Join(fmt.Errorf("%s changed while it was read", path), err)
	}
	return data, info, nil
}

type keyboardObservation struct {
	Present, Sound, Recorded bool
	Fingerprint              string
	Record                   state.Record
	Unreadable               string
	exact                    bool
}

func (k *KeyboardAccess) observe(st state.State) keyboardObservation {
	o := keyboardObservation{}
	o.Record, o.Recorded = st.Targets[k.Rules]
	data, info, err := keyboardRead(k.Rules)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		o.Unreadable = err.Error()
	default:
		o.Present, o.Fingerprint, o.exact = true, KeyboardRuleFingerprint(data), bytes.Equal(data, KeyboardRule())
		owner, _, mode, ownerErr := platform.FileOwner(k.Rules)
		o.Sound = ownerErr == nil && int(owner) == k.Owner && mode == 0644 && info.Mode().Perm() == 0644
	}
	return o
}

func (o keyboardObservation) owned() bool {
	if !o.Present || !o.Recorded || !o.Sound {
		return false
	}
	return o.Record.Applied == o.Fingerprint || !o.Record.Release && o.Record.Next == o.Fingerprint
}

func (o keyboardObservation) settled() bool {
	return o.owned() && o.Record.Applied == o.Fingerprint && o.Record.Next == "" && !o.Record.Release
}

// Inspect reads the rule file and its record. A rule that differs from its
// record, or that has no record and is not the exact rule of setup, stays as
// it is.
func (k *KeyboardAccess) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	if err := ctx.Err(); err != nil {
		return engine.Finding{}, err
	}
	if err := k.validate(r); err != nil {
		return engine.Finding{}, err
	}
	if k.Issue != "" {
		return engine.Finding{Status: engine.Unknown, Reason: k.Issue}, nil
	}
	o := k.observe(rec.State())
	token, err := aptDigest(struct {
		Observation keyboardObservation
		Present     bool
	}{o, want.Present})
	if err != nil {
		return engine.Finding{}, err
	}
	f := engine.Finding{Token: token}
	detail := "udev rule " + k.Rules + ": keyboards and the virtual input device for the user logged in at this screen"
	changed := k.Rules + " changed after setup installed it; it is preserved for inspection"
	switch {
	case o.Unreadable != "":
		f.Status, f.Reason = engine.Unknown, "cannot inspect "+k.Rules+"; nothing was changed: "+o.Unreadable
	case !want.Present && !o.Present:
		f.Status, f.Reason = engine.Absent, "the rule is not installed"
		if o.Recorded && o.Record.Release {
			f.Privileged, f.Pending = true, true
			f.Commands = []engine.CommandBlock{k.block("Complete the removal of the keyboard access rule", append(k.removeCommands()[1:], k.reloadCommands()...)...)}
		}
	case !want.Present && o.owned():
		f.Status, f.Privileged, f.Pending = engine.Satisfied, true, true
		f.Commands = []engine.CommandBlock{k.block("Remove the keyboard access rule", append(k.removeCommands(), k.reloadCommands()...)...)}
	case !want.Present && o.Recorded:
		f.Status, f.Reason, f.Preserved = engine.Changed, changed, []string{k.Rules}
	case !want.Present:
		f.Status = engine.Satisfied
	case !o.Present:
		f.Status, f.Reason, f.Privileged, f.Pending = engine.Missing, detail, true, true
		block := k.block("Install the keyboard access rule", append(k.writeCommands(), k.reloadCommands()...)...)
		block.Lines = append([]string{"# Setup first writes the rule to " + k.stage() + ". It deletes the file after the step."}, block.Lines...)
		f.Commands = []engine.CommandBlock{block}
	case o.settled():
		f.Status = engine.Satisfied
	case o.owned():
		f.Status, f.Reason, f.Privileged, f.Pending = engine.Unhealthy, "the rule is in place, and its udev reload did not finish; setup repeats the reload", true, true
		f.Commands = []engine.CommandBlock{k.block("Reload the keyboard access rule", append(k.writeCommands()[2:], k.reloadCommands()...)...)}
	case o.Recorded:
		f.Status, f.Reason, f.Preserved = engine.Changed, changed, []string{k.Rules}
	case o.exact && o.Sound:
		f.Status = engine.Satisfied
		f.Notes = []string{k.Rules + " has the rule already. Setup did not write it and does not remove it."}
	default:
		f.Status = engine.Blocked
		f.Reason = k.Rules + " exists but is not the exact rule dotfiles installs; dotfiles never replaces or removes it. Inspect it, and remove it yourself to let setup install its rule"
	}
	return f, nil
}

func (k *KeyboardAccess) authenticate(ctx context.Context) error {
	if k.Runner == nil {
		return errors.New("keyboard access has no command runner")
	}
	if k.Sudo != "" && k.Authenticate != nil {
		return k.Authenticate(ctx)
	}
	return nil
}

func (k *KeyboardAccess) run(ctx context.Context, rec engine.Records, mark, failure string, commands ...platform.Command) error {
	for _, c := range commands {
		if _, err := k.Runner.Run(ctx, c); err != nil {
			if _, started := platform.ExitCode(err); !started {
				return errors.Join(fmt.Errorf("%s: the command did not start: %w", failure, err), keyboardClear(rec, mark))
			}
			return fmt.Errorf("%s; run setup again and confirm that the command exited: %w", failure, err)
		}
	}
	return nil
}

func keyboardClear(rec engine.Records, mark string) error {
	return rec.Update(func(s *state.State) error { s.Clear(mark); return nil })
}

// Apply writes the rule when it is absent, reloads udev and triggers the
// devices. The record gets the intended value and the pending mark before
// the first privileged command, and the applied value after the verified
// reload, which also clears the mark. A retry repeats the reload and the
// triggers, also when the rule file already matches.
func (k *KeyboardAccess) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	if err := k.validate(r); err != nil {
		return err
	}
	if k.Issue != "" {
		return errors.New(k.Issue)
	}
	o := k.observe(rec.State())
	content := KeyboardRule()
	next := KeyboardRuleFingerprint(content)
	switch {
	case o.Unreadable != "":
		return errors.New("cannot inspect " + k.Rules + "; nothing was changed: " + o.Unreadable)
	case o.owned():
		next = o.Fingerprint
	case o.Present:
		return errors.New(k.Rules + " is not the rule that setup installed; setup cannot repair it in place. Remove it yourself if it changed, then apply again")
	}
	if err := k.authenticate(ctx); err != nil {
		return err
	}
	mark := r.ID + ":" + keyboardStep
	if err := rec.Update(func(s *state.State) error {
		s.Mark(mark)
		return s.BeginTarget(k.Rules, state.Record{Resource: r.ID, Kind: "file", Next: next, Before: "absent"})
	}); err != nil {
		return err
	}
	commands := k.writeCommands()
	if o.Present {
		commands = commands[2:]
	} else {
		if _, err := os.Lstat(k.Rules); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(errors.New(k.Rules+" changed since approval; review a fresh plan"), err, keyboardClear(rec, mark))
		}
		if err := errors.Join(os.MkdirAll(k.Directory, 0700), os.WriteFile(k.stage(), content, 0600)); err != nil {
			return errors.Join(err, keyboardClear(rec, mark))
		}
	}
	if err := k.run(ctx, rec, mark, "the keyboard access rule was not written", commands...); err != nil {
		return err
	}
	if err := k.run(ctx, rec, mark, "the udev reload of the keyboard access rule failed", k.reloadCommands()...); err != nil {
		return err
	}
	after := k.observe(rec.State())
	if !after.Present || !after.Sound || after.Fingerprint != next {
		return errors.New(k.Rules + " is not the rule that this step wrote; it is preserved for inspection")
	}
	if err := rec.Update(func(s *state.State) error {
		s.Clear(mark)
		return s.Promote(k.Rules)
	}); err != nil {
		return err
	}
	if err := os.Remove(k.stage()); err != nil && !errors.Is(err, os.ErrNotExist) {
		engine.ReportDetail(ctx, "Note: the staged rule %s stays: %v", k.stage(), err)
	}
	return nil
}

// Remove deletes the rule only while it matches its record, then reloads
// udev and drops the record. The pending mark covers each privileged
// command of the removal. A rule that changed stays, with its record.
func (k *KeyboardAccess) Remove(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	if err := k.validate(r); err != nil {
		return err
	}
	if k.Issue != "" {
		return errors.New(k.Issue)
	}
	o := k.observe(rec.State())
	switch {
	case !o.Recorded:
		return nil
	case o.Unreadable != "":
		return errors.New("cannot inspect " + k.Rules + "; nothing was changed: " + o.Unreadable)
	case o.Present && !o.owned():
		return errors.New(k.Rules + " is not the rule this setup installed; it is preserved")
	case !o.Present && !o.Record.Release:
		return rec.Update(func(s *state.State) error { s.Drop(k.Rules); return nil })
	}
	if err := k.authenticate(ctx); err != nil {
		return err
	}
	mark := r.ID + ":" + keyboardStep
	expected, commands := o.Record.Applied, k.removeCommands()
	if o.Present {
		expected = o.Fingerprint
	}
	if err := rec.Update(func(s *state.State) error {
		s.Mark(mark)
		if !o.Present {
			return nil
		}
		if record := s.Targets[k.Rules]; record.Applied != expected {
			if err := s.Promote(k.Rules); err != nil {
				return err
			}
		}
		return s.BeginRelease(k.Rules)
	}); err != nil {
		return err
	}
	if o.Present {
		if data, _, err := keyboardRead(k.Rules); err != nil || KeyboardRuleFingerprint(data) != expected {
			return errors.Join(errors.New("the keyboard access rule changed during removal; preserve "+k.Rules), err, keyboardClear(rec, mark))
		}
		if err := k.run(ctx, rec, mark, "the keyboard access rule was not moved", commands[0]); err != nil {
			return err
		}
	}
	aside := k.sibling(".removing")
	data, _, err := keyboardRead(aside)
	if err != nil && !errors.Is(err, os.ErrNotExist) || err == nil && KeyboardRuleFingerprint(data) != expected {
		return errors.Join(errors.New("the keyboard access rule changed during removal; preserve "+aside), err)
	}
	if err == nil {
		if err := k.run(ctx, rec, mark, "the moved keyboard access rule "+aside+" was not deleted", commands[1]); err != nil {
			return err
		}
	}
	if err := k.run(ctx, rec, mark, "the udev reload of the keyboard access rule failed", k.reloadCommands()...); err != nil {
		return err
	}
	if _, err := os.Lstat(k.Rules); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(errors.New("the keyboard access rule is still present after removal"), err)
	}
	return rec.Update(func(s *state.State) error {
		s.Clear(mark)
		s.Drop(k.Rules)
		return nil
	})
}
