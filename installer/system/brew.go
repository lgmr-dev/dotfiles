package system

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

var brewProcessControls = []string{"LC_ALL=C", "HOMEBREW_NO_ANALYTICS=1", "HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_INSTALL_CLEANUP=1", "HOMEBREW_NO_AUTOREMOVE=1", "HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=", "HOMEBREW_NO_INSTALL_UPGRADE=1", "HOMEBREW_NO_ASK=1", "HOMEBREW_NO_COLOR=1"}

// Brew is the provider of the homebrew-formula and homebrew-cask bindings.
// It uses a healthy package that is present. It installs a missing one and
// records its name. It repairs and updates only a recorded formula. An
// application of a cask updates itself, so its inspection compares no
// version. It never removes a package: for a recorded package that the
// selection does not need, the inspection gives the removal command for the
// operator. Each brew command gets the process controls that stop automatic
// updates, cleanup and autoremove. No command uses --force,
// --ignore-dependencies or zap.
//
// Discover runs until it finds Homebrew, so a new Homebrew is visible without
// a new process. Query replaces the inspection commands of the discovered
// Homebrew when it is not nil. Runner runs the brew commands in the
// foreground.
type Brew struct {
	Home     string
	Discover func(context.Context) (*HomebrewLocation, error)
	Query    MacQuery
	Runner   platform.Runner
	appdir   string
	mu       sync.Mutex
	location *HomebrewLocation
}

// NewBrew returns the provider for the home folder. It is pure.
func NewBrew(home string) *Brew {
	return &Brew{Home: home, Discover: DiscoverHomebrew, Runner: platform.Exec{}, appdir: "/Applications"}
}

type brewSession struct {
	location HomebrewLocation
	query    MacQuery
	casks    map[string]CaskSpec
}

func (d *Brew) session(ctx context.Context) (*brewSession, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.location == nil {
		if d.Discover == nil || !macosCleanPath(d.Home) || !macosCleanPath(d.appdir) {
			return nil, "", errors.New("invalid Homebrew provider boundary")
		}
		location, issue, err := homebrewLocate(ctx, d.Discover)
		if err != nil {
			return nil, "", err
		}
		if location == nil && issue == "" {
			issue = "install Homebrew, then run setup again to review its package inventory"
		}
		if issue != "" {
			return nil, issue, nil
		}
		d.location = location
	}
	query := d.Query
	if query == nil {
		query = d.location.Query
	}
	return &brewSession{location: *d.location, query: query, casks: macCasks(d.Home, d.location.Prefix, d.appdir)}, "", nil
}

type brewBinding struct {
	full, name string
	cask       *CaskSpec
}

func (s *brewSession) binding(r catalog.Resource) (brewBinding, error) {
	binding := r.Bindings["darwin"]
	name, err := brewShortName(binding.Package)
	if err != nil {
		return brewBinding{}, err
	}
	b := brewBinding{full: binding.Package, name: name}
	switch binding.Provider {
	case "homebrew-formula":
		if _, ok := brewChecks[r.ID]; !ok {
			return b, fmt.Errorf("%s lacks its native command health check", r.ID)
		}
		return b, nil
	case "homebrew-cask":
		spec, ok := s.casks[r.ID]
		if !ok || spec.Token != binding.Package {
			return b, fmt.Errorf("unsupported native application %s", r.ID)
		}
		b.cask = &spec
		return b, nil
	}
	return b, fmt.Errorf("%s has no Homebrew binding", r.ID)
}

var brewChecks = map[string][]string{"tool.git": {"git", "--version"}, "tool.tmux": {"tmux", "-V"}, "tool.tailscale": {"tailscale", "version"}}

func (s *brewSession) check(ctx context.Context, r catalog.Resource) error {
	arguments := brewChecks[r.ID]
	output, err := s.query(ctx, filepath.Join(s.location.Prefix, "bin", arguments[0]), arguments[1:]...)
	if err != nil {
		return fmt.Errorf("Homebrew %s failed its native command check: %w%s", r.Name, err, platform.Diagnostic(output))
	}
	return nil
}

func (s *brewSession) command(cask bool, args ...string) platform.Command {
	environment := slices.Clone(brewProcessControls)
	if cask {
		environment = append(environment, "HOMEBREW_CASK_OPTS=", "HOMEBREW_NO_SUDO=1")
	}
	return platform.Command{Path: s.location.Program, Args: args, Env: environment}
}

func (s *brewSession) line(cask bool, args ...string) string {
	return macosShellLine(s.command(cask, args...))
}

func (s *brewSession) removalBlock(name string, cask bool) (engine.CommandBlock, string) {
	line := s.line(false, "uninstall", name)
	if cask {
		line = s.line(true, "uninstall", "--cask", name)
	}
	return engine.CommandBlock{Title: "Remove " + name + " with Homebrew (setup never removes a system package)", By: "you", Lines: []string{line}},
		"Examine what Homebrew proposes to remove before you confirm. Setup deletes its record of " + name + " after the package is absent."
}

type brewFormulaObservation struct {
	Name, Version, Source           string
	Present, Linked, Held, Recorded bool
	Checked, Outdated               bool
	Issue, Current                  string
	Broken                          []brewBrokenDependent
}

type brewBrokenDependent struct {
	Name, Source string
	Repair       bool
}

func (s *brewSession) brokenDependents(ctx context.Context, installed map[string]brewPackage, root string, st state.State) ([]brewBrokenDependent, error) {
	formulae := []string{}
	for _, name := range slices.Sorted(maps.Keys(brewRelated(root, installed))) {
		if !strings.HasPrefix(name, "cask/") {
			formulae = append(formulae, name)
		}
	}
	failures, err := s.failedLinkage(ctx, installed, formulae)
	var broken []brewBrokenDependent
	for _, failure := range failures {
		pkg := installed[failure.Name]
		broken = append(broken, brewBrokenDependent{Name: pkg.Name, Source: pkg.Source, Repair: !pkg.Held && brewRecorded(st, pkg)})
	}
	return broken, err
}

func (s *brewSession) observeFormula(ctx context.Context, r catalog.Resource, b brewBinding, st state.State, want engine.Want) (brewFormulaObservation, error) {
	installed, err := brewInventory(ctx, s.query)
	if err != nil {
		return brewFormulaObservation{}, err
	}
	pkg, present := installed[b.name]
	o := brewFormulaObservation{Name: b.name, Version: pkg.Version, Source: pkg.Source, Present: present, Linked: pkg.Healthy, Held: pkg.Held, Recorded: st.HasSystem("brew", b.full)}
	if !present {
		return o, nil
	}
	if !o.Linked {
		o.Issue = "Homebrew does not link an installed version of " + b.name
		return o, nil
	}
	if err := s.check(ctx, r); err != nil {
		if ctx.Err() != nil {
			return o, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return o, err
		}
		o.Issue = err.Error()
		return o, nil
	}
	o.Checked = true
	if want.Present && brewLinkageDue(st, r.ID) {
		if o.Broken, err = s.brokenDependents(ctx, installed, b.name, st); err != nil {
			return o, err
		}
	}
	if want.Update && want.Present && o.Recorded {
		o.Current, o.Outdated, err = brewOutdated(ctx, s.query, b.full)
	}
	return o, err
}

func (o brewFormulaObservation) token(want engine.Want) (string, error) {
	o.Issue = ""
	return macosDigest(struct {
		Observation brewFormulaObservation
		Want        engine.Want
	}{o, want})
}

func (s *brewSession) inspectFormula(ctx context.Context, r catalog.Resource, b brewBinding, want engine.Want, st state.State) (engine.Finding, error) {
	o, err := s.observeFormula(ctx, r, b, st, want)
	if err != nil {
		return engine.Finding{}, err
	}
	token, err := o.token(want)
	if err != nil {
		return engine.Finding{}, err
	}
	f := engine.Finding{Status: engine.Satisfied, Version: o.Version, Token: token}
	if !want.Present {
		if !o.Present {
			f.Status, f.Reason = engine.Absent, "the package is not installed"
			return f, nil
		}
		if o.Recorded {
			block, note := s.removalBlock(b.full, false)
			f.Commands, f.Notes = []engine.CommandBlock{block}, []string{note}
		}
		return f, nil
	}
	setup := func(verb string) {
		f.Pending = true
		f.Commands = []engine.CommandBlock{{Title: map[string]string{"install": "Install ", "reinstall": "Repair ", "upgrade": "Update "}[verb] + b.name + " with Homebrew", By: "setup",
			Lines: []string{s.line(false, verb, "--formula", b.full)}}}
		f.Notes = append(f.Notes, "Homebrew can install or upgrade the formulae that "+b.name+" depends on. Setup then checks the linkage of the installed formulae that use them. It reinstalls one that fails only when setup installed it, and stops with the repair command for each other one.")
	}
	foreign := o.Present && o.Source != b.full
	switch {
	case !o.Present:
		f.Status = engine.Missing
		setup("install")
	case !o.Linked || !o.Checked:
		switch {
		case !o.Recorded:
			f.Status, f.Reason = engine.Blocked, o.Issue+"; setup did not install this package and does not change it: repair it yourself"
			f.Commands = []engine.CommandBlock{{Title: "Repair " + b.name + " with Homebrew", By: "you", Lines: []string{s.line(false, "reinstall", "--formula", o.Source)}}}
		case o.Held:
			f.Status, f.Reason = engine.Blocked, o.Issue+"; the Homebrew package is pinned, and setup keeps the pin: repair it yourself"
		case foreign:
			f.Status, f.Reason = engine.Blocked, o.Issue+"; the Homebrew package changed its source to "+o.Source+", and setup keeps it: repair it yourself"
		default:
			f.Status, f.Reason = engine.Unhealthy, o.Issue
			setup("reinstall")
		}
	case len(o.Broken) != 0:
		names := []string{}
		for _, broken := range o.Broken {
			names = append(names, broken.Name)
			by := "you"
			if broken.Repair {
				by, f.Pending = "setup", true
			}
			f.Commands = append(f.Commands, engine.CommandBlock{Title: "Repair " + broken.Name + " with Homebrew", By: by, Lines: []string{s.line(false, "reinstall", "--formula", broken.Source)}})
		}
		f.Status, f.Reason = engine.Unhealthy, "after setup changed "+b.name+", these installed Homebrew formulae fail their linkage check: "+strings.Join(names, ", ")
		f.Notes = append(f.Notes, "Setup reinstalls only a formula that it installed. Repair each other formula yourself, then run setup again.")
	case o.Outdated && o.Held:
		f.Notes = append(f.Notes, fmt.Sprintf("Homebrew has version %s of %s. The package is pinned, and setup keeps the pin.", o.Current, b.name))
	case o.Outdated && foreign:
		f.Notes = append(f.Notes, fmt.Sprintf("Homebrew has version %s of %s. The package changed its source to %s, and setup does not update it.", o.Current, b.name, o.Source))
	case o.Outdated:
		f.Status, f.Reason = engine.Outdated, fmt.Sprintf("Homebrew has version %s; version %s is installed", o.Current, o.Version)
		setup("upgrade")
	}
	return f, nil
}

func brewUnavailable(reason string) (engine.Finding, error) {
	token, err := macosDigest(reason)
	return engine.Finding{Status: engine.Unknown, Reason: reason, Token: token}, err
}

// Inspect reports the package of the resource. Without Homebrew, or with a
// Homebrew that setup cannot use, the result is unknown. After setup changed
// a formula, Inspect checks the linkage of the installed formulae that use
// the formula or its dependencies, until such a check in Apply passes.
// A failed check makes the formula unhealthy.
func (d *Brew) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	s, issue, err := d.session(ctx)
	if err != nil {
		return engine.Finding{}, err
	}
	if issue != "" {
		return brewUnavailable(issue)
	}
	b, err := s.binding(r)
	if err != nil {
		return engine.Finding{}, err
	}
	if b.cask != nil {
		return s.inspectCask(ctx, *b.cask, want, rec.State())
	}
	return s.inspectFormula(ctx, r, b, want, rec.State())
}

func brewMark(r catalog.Resource, action string) string { return r.ID + ":brew-" + action }

func (d *Brew) run(ctx context.Context, rec engine.Records, c platform.Command, mark, provider, provisional string) error {
	_, started, err := macosRun(ctx, d.Runner, c)
	if err != nil && !started {
		return errors.Join(fmt.Errorf("Homebrew did not start: %w", err), rec.Update(func(s *state.State) error {
			s.Clear(mark)
			if provisional != "" {
				s.RemoveSystem(provider, provisional)
			}
			return nil
		}))
	}
	if err != nil {
		return fmt.Errorf("Homebrew native command failed; examine its output, run setup again and confirm that the command exited: %w", err)
	}
	return nil
}

func (d *Brew) begin(rec engine.Records, mark, provider, provisional string) error {
	return rec.Update(func(s *state.State) error {
		s.Mark(mark)
		if provisional != "" {
			s.AddSystem(provider, provisional)
		}
		return nil
	})
}

func (d *Brew) applyFormula(ctx context.Context, s *brewSession, r catalog.Resource, b brewBinding, approved engine.Finding, rec engine.Records) error {
	before, err := brewInventory(ctx, s.query)
	if err != nil {
		return err
	}
	pkg, present := before[b.name]
	st := rec.State()
	recorded, due := st.HasSystem("brew", b.full), brewLinkageDue(st, r.ID)
	verb, action := "", ""
	switch approved.Status {
	case engine.Missing:
		verb, action = "install", "install"
		if present {
			return errors.New("Homebrew root appeared before installation; preserve it and review a fresh plan")
		}
	case engine.Unhealthy:
		verb, action = "reinstall", "repair"
		if due && pkg.Healthy {
			checkErr := s.check(ctx, r)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if checkErr == nil {
				verb = ""
			}
		}
	case engine.Outdated:
		verb, action = "upgrade", "update"
	default:
		return errors.New("Homebrew has no approved change for " + b.name)
	}
	if verb == "reinstall" || verb == "upgrade" {
		if !present || !recorded {
			return errors.New("pre-existing Homebrew package cannot be updated or repaired implicitly")
		}
		if pkg.Source != b.full {
			return errors.New("Homebrew root changed its source; preserve the current package")
		}
		if pkg.Held {
			return errors.New("Homebrew package is pinned; preserve the pin and review it before mutation")
		}
	}
	mark, provisional := brewMark(r, action), ""
	if verb == "install" && !recorded {
		provisional = b.full
	}
	root := func() (bool, error) {
		installed, err := brewInventory(ctx, s.query)
		if err != nil {
			return false, err
		}
		pkg, present := installed[b.name]
		if !present || pkg.Source != b.full {
			return false, errors.New("Homebrew formula did not pass native activation checks")
		}
		return pkg.Healthy, nil
	}
	if verb != "" {
		if err := rec.Update(func(st *state.State) error {
			st.Mark(mark)
			if !due {
				st.Linkage = append(st.Linkage, r.ID)
			}
			if provisional != "" {
				st.AddSystem("brew", provisional)
			}
			return nil
		}); err != nil {
			return err
		}
		if err := d.run(ctx, rec, s.command(false, verb, "--formula", b.full), mark, "brew", provisional); err != nil {
			if !due && !slices.Contains(rec.State().Pending, mark) {
				return errors.Join(err, rec.Update(func(st *state.State) error { brewDropLinkage(st, r.ID); return nil }))
			}
			return err
		}
		healthy, err := root()
		if err == nil && !healthy && verb == "reinstall" {
			if err := d.run(ctx, rec, s.command(false, "link", "--formula", b.full), mark, "", ""); err != nil {
				return err
			}
			healthy, err = root()
		}
		if err != nil || !healthy {
			return errors.Join(errors.New("Homebrew formula did not pass native activation checks"), err)
		}
		if err := s.check(ctx, r); err != nil {
			return err
		}
	}
	if err := d.maintainDependents(ctx, s, rec, mark, b, before, due); err != nil {
		return err
	}
	if healthy, err := root(); err != nil || !healthy {
		return errors.Join(errors.New("Homebrew root changed during dependent maintenance"), err)
	}
	if err := s.check(ctx, r); err != nil {
		return err
	}
	return rec.Update(func(st *state.State) error {
		st.Clear(mark)
		brewDropLinkage(st, r.ID)
		return nil
	})
}

func brewDropLinkage(st *state.State, id string) {
	st.Linkage = slices.DeleteFunc(st.Linkage, func(due string) bool { return due == id })
}

// Apply installs a missing package, or repairs or updates a recorded
// formula. It writes the pending mark, the linkage check of the dependents of
// a formula, and for a new install the provisional system record, before brew
// starts. It clears the mark and the linkage check after the package and its
// dependents pass their checks. For a healthy formula with broken dependents,
// a repair reinstalls only the recorded dependents.
func (d *Brew) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	s, issue, err := d.session(ctx)
	if err != nil {
		return err
	}
	if issue != "" {
		return errors.New(issue)
	}
	b, err := s.binding(r)
	if err != nil {
		return err
	}
	if b.cask != nil {
		return d.applyCask(ctx, s, r, *b.cask, approved, rec)
	}
	return d.applyFormula(ctx, s, r, b, approved, rec)
}

// Remove never removes a package. It deletes the system record of the
// package only after it verified that Homebrew does not hold the package.
func (d *Brew) Remove(ctx context.Context, r catalog.Resource, _ engine.Finding, rec engine.Records) error {
	s, issue, err := d.session(ctx)
	if err != nil {
		return err
	}
	if issue != "" {
		return errors.New(issue)
	}
	b, err := s.binding(r)
	if err != nil {
		return err
	}
	installed, err := brewInventory(ctx, s.query)
	if err != nil {
		return err
	}
	provider, name := "brew", b.name
	if b.cask != nil {
		provider, name = "cask", "cask/"+b.name
	}
	if _, present := installed[name]; present {
		block, _ := s.removalBlock(b.full, b.cask != nil)
		return fmt.Errorf("%s is installed, and setup never removes a system package; remove it with `%s` if nothing else needs it", b.full, block.Lines[0])
	}
	return rec.Update(func(st *state.State) error {
		st.RemoveSystem(provider, b.full)
		brewDropLinkage(st, r.ID)
		return nil
	})
}
