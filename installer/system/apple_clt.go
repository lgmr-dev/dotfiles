package system

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

// AppleCLT is the provider of infra.apple-clt. It installs the Apple Command
// Line Tools only when no developer tools are present, with the package that
// softwareupdate offers, and selects them. It uses healthy selected Xcode or
// Command Line Tools as they are. It never updates or removes Apple tools and
// never changes another selected developer directory. The discovery file
// that softwareupdate needs is temporary. Runner runs one `sudo -n` command
// in the foreground. Setup keeps no record of the tools.
type AppleCLT struct {
	Query            MacQuery
	Runner           platform.Runner
	clt, placeholder string
}

// NewAppleCLT returns the provider. It is pure.
func NewAppleCLT() *AppleCLT {
	return &AppleCLT{Query: MacPrerequisiteQuery, Runner: platform.Exec{}, clt: "/Library/Developer/CommandLineTools",
		placeholder: "/tmp/.com.apple.dt.CommandLineTools.installondemand.in-progress"}
}

type appleCLTObservation struct {
	Status              engine.Status
	Reason, Version     string
	Baseline, Selection string
	Fingerprint         string
}

func (d *AppleCLT) observe(ctx context.Context, r catalog.Resource) (appleCLTObservation, error) {
	if r.ID != "infra.apple-clt" || d.Query == nil || !macosCleanPath(d.clt) || !macosCleanPath(d.placeholder) {
		return appleCLTObservation{}, errors.New("invalid Apple CLT prerequisite boundary")
	}
	selected, err := appleCLTSelection(ctx, d.Query)
	if err != nil {
		return appleCLTObservation{}, err
	}
	evidence := map[string]string{}
	for _, id := range []string{"tool.compiler", "tool.make"} {
		if selected == "" {
			evidence = nil
			break
		}
		check, err := macosToolchain(ctx, d.Query, id)
		if err != nil {
			return appleCLTObservation{}, err
		}
		if check.Status != engine.Satisfied {
			evidence = nil
			break
		}
		evidence[id] = check.Fingerprint
	}
	if evidence != nil {
		fingerprint, err := macosDigest(evidence)
		return appleCLTObservation{Status: engine.Satisfied, Selection: selected, Fingerprint: fingerprint}, err
	}
	if _, err := os.Lstat(d.clt); err == nil {
		return appleCLTObservation{Status: engine.Unknown, Selection: selected,
			Reason: "existing Apple Command Line Tools need native repair or selection; preserve them, complete Apple setup and check again"}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return appleCLTObservation{}, err
	}
	if selected != "" && selected != d.clt {
		return appleCLTObservation{Status: engine.Unknown, Selection: selected,
			Reason: "the selected Xcode needs license/setup or repair; preserve the chosen developer directory and complete Apple setup"}, nil
	}
	baseline, err := AppleCLTReceipt(ctx, d.Query)
	if err != nil {
		return appleCLTObservation{}, err
	}
	o := appleCLTObservation{Status: engine.Missing, Reason: appleCLTDisclosure(baseline), Baseline: baseline, Selection: selected}
	if baseline != "none" {
		o.Version, _, _ = strings.Cut(baseline, "@")
	}
	o.Fingerprint, err = macosDigest([]string{baseline, selected})
	return o, err
}

// Inspect reports healthy selected developer tools as satisfied, a missing
// payload as missing, and each other condition as unknown: it never proves
// absence from a failed query.
func (d *AppleCLT) Inspect(ctx context.Context, r catalog.Resource, _ engine.Want, _ engine.Records) (engine.Finding, error) {
	o, err := d.observe(ctx, r)
	if err != nil {
		return engine.Finding{}, err
	}
	token, err := macosDigest(o)
	f := engine.Finding{Status: o.Status, Reason: o.Reason, Version: o.Version, Token: token}
	if o.Status == engine.Missing {
		f.Privileged, f.Pending, f.Replan = true, true, true
		f.Commands = []engine.CommandBlock{{Title: "Install the Apple Command Line Tools", By: "setup", Lines: []string{
			"softwareupdate -l",
			"sudo softwareupdate -i \"<newest Command Line Tools label>\"",
			"sudo xcode-select --switch " + d.clt,
		}}}
	}
	return f, err
}

func appleCLTMark(r catalog.Resource) string { return r.ID + ":install" }

// Apply installs the missing Command Line Tools. It writes the pending mark
// before the command starts and clears it after the receipt, the selection
// and the compiler, GNU Make and SDK checks pass.
func (d *AppleCLT) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	o, err := d.observe(ctx, r)
	if err != nil {
		return err
	}
	if approved.Status != engine.Missing || o.Status != engine.Missing {
		return errors.New("Apple developer tools are not proven absent; preserve them and review a new plan. Setup does not update or remove Apple tools")
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	operation := hex.EncodeToString(seed)
	command, err := d.command(operation, o.Baseline, o.Selection)
	if err != nil {
		return err
	}
	mark := appleCLTMark(r)
	if err := rec.Update(func(s *state.State) error { s.Mark(mark); return nil }); err != nil {
		return err
	}
	output, started, err := macosRun(ctx, d.Runner, command)
	if err != nil && !started {
		return errors.Join(fmt.Errorf("the Apple CLT command did not start: %w", err), macosClearMark(rec, mark))
	}
	if err != nil {
		return fmt.Errorf("Apple CLT native action failed; administrator access must be authorized in the foreground; inspect the native diagnostic, run setup again and confirm that the command exited: %w", err)
	}
	installed, err := appleCLTInstalledReceipt(output)
	if err != nil || !appleCLTMarker(output, "INSTALLED", operation) || !appleCLTMarker(output, "SELECTED", operation) || !appleCLTReceiptChanged(o.Baseline, installed) {
		return errors.Join(errors.New("Apple CLT lacks successful installation and final selection evidence"), err)
	}
	actual, err := AppleCLTReceipt(ctx, d.Query)
	if err != nil || actual != installed {
		return errors.Join(errors.New("Apple CLT receipt differs from the installed receipt; preserve it"), err)
	}
	selected, err := appleCLTSelection(ctx, d.Query)
	if err != nil || selected != d.clt {
		return errors.Join(errors.New("Apple CLT is not the selected developer directory; preserve the current selection"), err)
	}
	after, err := d.observe(ctx, r)
	if err != nil || after.Status != engine.Satisfied {
		return errors.Join(errors.New("Apple CLT has not passed compiler, GNU Make and SDK health checks"), err)
	}
	return macosClearMark(rec, mark)
}

// Remove always fails: the Apple developer tools stay.
func (*AppleCLT) Remove(context.Context, catalog.Resource, engine.Finding, engine.Records) error {
	return errors.New("Apple developer infrastructure is retained and cannot be removed by dotfiles")
}
