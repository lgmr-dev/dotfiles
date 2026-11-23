package system

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/state"
)

// CaskIdentity inspects the application bundle at path with codesign and the
// signing requirement of the recipe. present is true for each entry at path.
// healthy is true only for a real folder that passes the signature, resource
// and link checks: a link, such as the application link of release v2, is
// present and not healthy. The check does not change attributes, clear the
// quarantine or sign the bundle again.
func CaskIdentity(ctx context.Context, query MacQuery, s CaskSpec, path string) (present, healthy bool, err error) {
	if err := s.Validate(); err != nil {
		return false, false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if !info.IsDir() {
		return true, false, nil
	}
	bound, err := platform.BindDestination(path)
	if err != nil || bound != path {
		return true, false, errors.Join(errors.New("application parent was redirected"), err)
	}
	_, err = query(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict=symlinks", "-R", "="+s.Requirement(), path)
	if err == nil {
		return true, true, nil
	}
	if ctx.Err() != nil {
		return true, false, ctx.Err()
	}
	if code, exited := platform.ExitCode(err); exited && (code == 1 || code == 3) {
		return true, false, nil
	}
	return true, false, fmt.Errorf("application identity could not be inspected: %w", err)
}

const macApplicationScript = `'use strict';
ObjC.import('AppKit')
function run(argv) {
	const applications = $.NSWorkspace.sharedWorkspace.runningApplications
	for (let i = 0; i < applications.count; i++) {
		if (ObjC.unwrap(applications.objectAtIndex(i).bundleIdentifier) === argv[0]) {
			return 'running'
		}
	}
	return 'not-running'
}`

type caskObservation struct {
	Token, Source, Application                    string
	Installed, Present, Healthy, Recorded, Orphan bool
	Status                                        engine.Status
	Reason                                        string
	Notes                                         []string
}

func (s CaskSpec) destinations(reinstall bool) error {
	for _, a := range s.Artifacts {
		target := s.target(a)
		info, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !a.Active() {
			return errors.New("preserve the existing " + s.Name + " command at " + target + "; Homebrew may unlink it during removal")
		}
		if reinstall && info.Mode()&os.ModeSymlink != 0 {
			if link, err := os.Readlink(target); err == nil && link == s.source(a) {
				continue
			}
		}
		return errors.New("preserve the existing file at " + target + "; the " + s.Name + " cask links its own file there")
	}
	return nil
}

func (s *brewSession) observeCask(ctx context.Context, spec CaskSpec, st state.State) (caskObservation, error) {
	o := caskObservation{Token: spec.Token, Recorded: st.HasSystem("cask", spec.Token), Application: spec.Application}
	installed, err := brewInventory(ctx, s.query)
	if err != nil {
		return o, err
	}
	pkg, inInventory := installed["cask/"+spec.Token]
	o.Installed, o.Source = inInventory, pkg.Source
	if err := spec.Validate(); err != nil {
		o.Status, o.Reason = engine.Blocked, err.Error()
		return o, nil
	}
	unknown := func(err error) (caskObservation, error) {
		if ctx.Err() != nil {
			return o, ctx.Err()
		}
		o.Status, o.Reason = engine.Unknown, "native application identity could not be inspected: "+err.Error()
		return o, nil
	}
	o.Present, o.Healthy, err = CaskIdentity(ctx, s.query, spec, spec.Application)
	if err != nil {
		return unknown(err)
	}
	if !o.Present && !o.Recorded {
		info, statErr := os.Lstat(spec.UserApplication)
		if statErr == nil && info.IsDir() {
			_, healthy, err := CaskIdentity(ctx, s.query, spec, spec.UserApplication)
			if err != nil {
				return unknown(err)
			}
			if healthy {
				o.Present, o.Healthy, o.Application = true, true, spec.UserApplication
			}
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return unknown(statErr)
		}
	}
	switch {
	case inInventory && pkg.Source != spec.Token:
		o.Status, o.Reason = engine.Blocked, "the native app uses a different Homebrew source; preserve it"
	case o.Present && !o.Healthy && inInventory:
		o.Status, o.Reason = engine.Blocked, "the native app changed identity or contains unsigned edits; preserve it and inspect it manually"
	case o.Present && !o.Healthy:
		o.Status, o.Reason = engine.Blocked, "the application target is occupied by an unverified app or link; preserve it"
	case o.Present:
		o.Status = engine.Satisfied
		if o.Recorded && inInventory {
			if err := spec.InstalledRecipe(); err != nil {
				o.Status, o.Reason = engine.Blocked, "preserve the native app: "+err.Error()
			}
		}
		if !inInventory {
			o.Notes = append(o.Notes, "Homebrew has no "+spec.Token+" cask. Setup uses the signed "+spec.Name+" application at "+o.Application+" and does not change it.")
		}
	case inInventory && o.Recorded:
		o.Status, o.Reason = engine.Unhealthy, "the recorded native application is absent"
	case inInventory:
		o.Status, o.Orphan = engine.Blocked, true
		o.Reason = "your Homebrew " + spec.Token + " record has no app; use the Restore command, or use the Remove command and then install the app through setup. Use only one of them"
	default:
		o.Status = engine.Missing
	}
	if o.Status == engine.Missing || o.Status == engine.Unhealthy {
		if err := spec.destinations(o.Status == engine.Unhealthy); err != nil {
			o.Status, o.Reason = engine.Blocked, err.Error()
		}
	}
	if o.Status == engine.Satisfied && inInventory {
		for _, a := range spec.Artifacts {
			if _, err := os.Lstat(spec.target(a)); err == nil && !a.Active() {
				o.Notes = append(o.Notes, "Preserve the existing "+spec.Name+" command at "+spec.target(a)+": Homebrew may unlink it during a removal of the cask.")
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return unknown(err)
			}
		}
	}
	return o, nil
}

func (s CaskSpec) arguments(verb string) []string {
	return []string{verb, "--cask", "--appdir=" + filepath.Dir(s.Application), "--no-binaries", "--require-sha", s.Token}
}

func (s *brewSession) inspectCask(ctx context.Context, spec CaskSpec, want engine.Want, st state.State) (engine.Finding, error) {
	o, err := s.observeCask(ctx, spec, st)
	if err != nil {
		return engine.Finding{}, err
	}
	token, err := macosDigest(struct {
		Observation caskObservation
		Present     bool
	}{o, want.Present})
	if err != nil {
		return engine.Finding{}, err
	}
	f := engine.Finding{Status: o.Status, Reason: o.Reason, Notes: o.Notes, Token: token}
	if !want.Present {
		if o.Status == engine.Unknown {
			return f, nil
		}
		if !o.Installed {
			f.Status, f.Reason, f.Notes = engine.Absent, "the cask is not installed", nil
			return f, nil
		}
		f.Status, f.Reason = engine.Satisfied, ""
		if o.Recorded {
			block, note := s.removalBlock(spec.Token, true)
			f.Commands, f.Notes = []engine.CommandBlock{block}, append(f.Notes, note, "Personal data of "+spec.Name+" stays. Setup never runs zap.")
		}
		return f, nil
	}
	switch o.Status {
	case engine.Missing:
		f.Reason = "install the official native app through Homebrew; normal app updates remain enabled; personal data stays untouched"
		f.Pending = true
		f.Commands = []engine.CommandBlock{{Title: "Install " + spec.Name + " with Homebrew", By: "setup", Lines: []string{s.line(true, spec.arguments("install")...)}}}
	case engine.Unhealthy:
		f.Pending = true
		f.Commands = []engine.CommandBlock{{Title: "Repair " + spec.Name + " with Homebrew", By: "setup", Lines: []string{s.line(true, spec.arguments("reinstall")...)}}}
	case engine.Blocked:
		if o.Orphan {
			f.Commands = []engine.CommandBlock{
				{Title: "Restore " + spec.Name + " with Homebrew", By: "you", Lines: []string{s.line(true, spec.arguments("reinstall")...)}},
				{Title: "Remove " + spec.Name + " with Homebrew", By: "you", Lines: []string{s.line(true, "uninstall", "--cask", spec.Token)}},
			}
		}
	}
	return f, nil
}

func (s *brewSession) checkCaskMutation(ctx context.Context, spec CaskSpec, reinstall bool) error {
	present, _, err := CaskIdentity(ctx, s.query, spec, spec.Application)
	if err != nil {
		return err
	}
	if present {
		return errors.New("refusing to overwrite an existing native app; review a fresh plan")
	}
	if !reinstall {
		if info, err := os.Lstat(spec.UserApplication); err == nil && info.IsDir() {
			_, healthy, err := CaskIdentity(ctx, s.query, spec, spec.UserApplication)
			if err != nil {
				return err
			}
			if healthy {
				return errors.New("an existing user application appeared; reuse it after a fresh preview")
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := spec.destinations(reinstall); err != nil {
		return err
	}
	installed, err := brewInventory(ctx, s.query)
	if err != nil {
		return err
	}
	_, exists := installed["cask/"+spec.Token]
	if exists != reinstall {
		return errors.New("the Homebrew record of " + spec.Token + " changed after the preview; review a fresh plan")
	}
	if exists {
		output, err := s.query(ctx, "/usr/bin/osascript", "-l", "JavaScript", "-e", macApplicationScript, spec.Bundle)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(output)) != "not-running" {
			return errors.New("quit all copies of " + spec.Name + " before changing its installation")
		}
		if err := spec.InstalledRecipe(); err != nil {
			return err
		}
	}
	output, err := s.query(ctx, "brew", "info", "--json=v2", "--cask", spec.Token)
	if err != nil {
		return err
	}
	var doc struct {
		Casks []json.RawMessage `json:"casks"`
	}
	if len(output) > 1<<20 || json.Unmarshal(output, &doc) != nil || len(doc.Casks) != 1 {
		return errors.New("native app metadata is incomplete")
	}
	return ValidateCaskMetadata(doc.Casks[0], spec, false)
}

func (d *Brew) applyCask(ctx context.Context, s *brewSession, r catalog.Resource, spec CaskSpec, approved engine.Finding, rec engine.Records) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	recorded := rec.State().HasSystem("cask", spec.Token)
	verb, action := "install", "install"
	switch approved.Status {
	case engine.Missing:
	case engine.Unhealthy:
		verb, action = "reinstall", "repair"
		if !recorded {
			return errors.New("pre-existing Homebrew package cannot be updated or repaired implicitly")
		}
	default:
		return errors.New("native auto-updating apps are not upgraded by dotfiles")
	}
	if err := s.checkCaskMutation(ctx, spec, verb == "reinstall"); err != nil {
		return err
	}
	mark, provisional := brewMark(r, action), ""
	if !recorded {
		provisional = spec.Token
	}
	if err := d.begin(rec, mark, "cask", provisional); err != nil {
		return err
	}
	if err := d.run(ctx, rec, s.command(true, spec.arguments(verb)...), mark, "cask", provisional); err != nil {
		return err
	}
	present, healthy, err := CaskIdentity(ctx, s.query, spec, spec.Application)
	if err != nil {
		return err
	}
	if !present || !healthy {
		return errors.New("Homebrew cask did not reach its verified application state")
	}
	installed, err := brewInventory(ctx, s.query)
	if err != nil {
		return err
	}
	if pkg, exists := installed["cask/"+spec.Token]; !exists || pkg.Source != spec.Token {
		return errors.New("Homebrew has no record of the cask after its installation")
	}
	if err := spec.InstalledRecipe(); err != nil {
		return fmt.Errorf("preserve the native app: %w", err)
	}
	return macosClearMark(rec, mark)
}

// Application returns the path of the signed application bundle that
// satisfies a cask resource: the bundle in /Applications, or a real bundle
// that the operator installed in the Applications folder of the home folder.
// The managed command link of the resource points into this bundle.
func (d *Brew) Application(ctx context.Context, r catalog.Resource, rec engine.Records) (string, error) {
	s, issue, err := d.session(ctx)
	if err != nil {
		return "", err
	}
	if issue != "" {
		return "", errors.New(issue)
	}
	b, err := s.binding(r)
	if err != nil {
		return "", err
	}
	if b.cask == nil {
		return "", fmt.Errorf("%s is not an application cask", r.ID)
	}
	o, err := s.observeCask(ctx, *b.cask, rec.State())
	if err != nil {
		return "", err
	}
	if !o.Present || !o.Healthy {
		return "", errors.New(b.cask.Name + " has no verified application bundle: " + o.Reason)
	}
	return o.Application, nil
}

// Cask returns the reviewed recipe of a cask resource.
func (d *Brew) Cask(ctx context.Context, r catalog.Resource) (CaskSpec, error) {
	s, issue, err := d.session(ctx)
	if err != nil {
		return CaskSpec{}, err
	}
	if issue != "" {
		return CaskSpec{}, errors.New(issue)
	}
	b, err := s.binding(r)
	if err != nil {
		return CaskSpec{}, err
	}
	if b.cask == nil {
		return CaskSpec{}, fmt.Errorf("%s is not an application cask", r.ID)
	}
	return *b.cask, nil
}
