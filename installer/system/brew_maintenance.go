package system

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

type brewClassification struct {
	Source          string
	Held, Automatic bool
}

func brewClassificationOf(pkg brewPackage) brewClassification {
	return brewClassification{Source: pkg.Source, Held: pkg.Held, Automatic: pkg.Automatic}
}

// BrewDependentError reports an installed formula that uses a formula that
// setup changed and that now fails its linkage check. Setup has no record of
// the formula, so setup does not repair it. Block holds the copy-ready
// reinstall command for the operator.
type BrewDependentError struct {
	Name, Reason string
	Block        engine.CommandBlock
}

func (err *BrewDependentError) Error() string {
	return "Homebrew formula " + err.Name + " fails its linkage check after this change, and setup did not install it: " + err.Reason +
		"\nRepair it with this command, then run setup again and confirm that the command exited:\n" + strings.Join(err.Block.Lines, "\n")
}

func brewUsers(seeds map[string]bool, inventory map[string]brewPackage) map[string]bool {
	for changed := true; changed; {
		changed = false
		for name, pkg := range inventory {
			if seeds[name] {
				continue
			}
			if slices.ContainsFunc(pkg.Dependencies, func(dependency string) bool { return seeds[dependency] }) {
				seeds[name], changed = true, true
			}
		}
	}
	return seeds
}

func brewAffected(root string, before, installed map[string]brewPackage) map[string]bool {
	affected := map[string]bool{root: true}
	for name, pkg := range installed {
		if old, existed := before[name]; !strings.HasPrefix(name, "cask/") && (!existed || old.Version != pkg.Version) {
			affected[name] = true
		}
	}
	return brewUsers(affected, before)
}

func brewRelated(root string, installed map[string]brewPackage) map[string]bool {
	related := map[string]bool{root: true}
	for _, dependency := range installed[root].Dependencies {
		related[dependency] = true
	}
	return brewUsers(related, installed)
}

func brewLinkageDue(st state.State, id string) bool { return slices.Contains(st.Linkage, id) }

type brewLinkageFailure struct{ Name, Detail string }

func brewInterrupted(ctx context.Context, err error, what string, output []byte) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Errorf("Homebrew %s linkage inspection interrupted: %w%s", what, err, platform.Diagnostic(output))
	}
	return nil
}

func (s *brewSession) failedLinkage(ctx context.Context, installed map[string]brewPackage, formulae []string) ([]brewLinkageFailure, error) {
	arguments := []string{"linkage", "--test"}
	for _, name := range formulae {
		arguments = append(arguments, installed[name].Source)
	}
	batchOutput, batchErr := s.query(ctx, "brew", arguments...)
	if batchErr == nil {
		return nil, nil
	}
	if err := brewInterrupted(ctx, batchErr, "affected-package", batchOutput); err != nil {
		return nil, err
	}
	failures := []brewLinkageFailure{}
	for _, name := range formulae {
		output, linkErr := s.query(ctx, "brew", "linkage", "--test", installed[name].Source)
		if linkErr == nil {
			continue
		}
		if err := brewInterrupted(ctx, linkErr, "affected package "+name, output); err != nil {
			return nil, err
		}
		failures = append(failures, brewLinkageFailure{Name: name, Detail: strings.TrimSpace(linkErr.Error() + platform.Diagnostic(output))})
	}
	if len(failures) == 0 {
		return nil, fmt.Errorf("Homebrew affected-package linkage check failed: %w%s", batchErr, platform.Diagnostic(batchOutput))
	}
	return failures, nil
}

func brewRecorded(st state.State, pkg brewPackage) bool {
	return st.HasSystem("brew", pkg.Source) || st.HasSystem("brew", pkg.Name)
}

func (d *Brew) maintainDependents(ctx context.Context, s *brewSession, rec engine.Records, mark string, b brewBinding, before map[string]brewPackage, due bool) error {
	attempted := map[string]bool{}
	for {
		installed, err := brewInventory(ctx, s.query)
		if err != nil {
			return err
		}
		affected := brewAffected(b.name, before, installed)
		if due {
			maps.Copy(affected, brewRelated(b.name, installed))
		}
		names := slices.Sorted(maps.Keys(affected))
		formulae, casks := []string{}, []string{}
		for _, name := range names {
			pkg, present := installed[name]
			if !present {
				return fmt.Errorf("Homebrew affected package %s is missing", name)
			}
			if strings.HasPrefix(name, "cask/") {
				baseline := before[name]
				if baseline.Version != pkg.Version || brewClassificationOf(baseline) != brewClassificationOf(pkg) {
					return fmt.Errorf("pre-existing Homebrew application %s changed during formula maintenance", name)
				}
				for _, dependency := range brewSortedUnique(append(slices.Clone(baseline.Dependencies), pkg.Dependencies...)) {
					if current, exists := installed[dependency]; !exists || !current.Healthy {
						return fmt.Errorf("dependency %s of Homebrew application %s is missing or inactive; restore it and retry", dependency, name)
					}
				}
				casks = append(casks, pkg.Source)
				continue
			}
			formulae = append(formulae, name)
		}
		failures, err := s.failedLinkage(ctx, installed, formulae)
		if err != nil {
			return err
		}
		if len(failures) == 0 {
			for _, source := range casks {
				engine.ReportDetail(ctx, "External application not exercised: %s", source)
			}
			return nil
		}
		name := failures[0].Name
		pkg := installed[name]
		if pkg.Held || attempted[name] {
			return fmt.Errorf("Homebrew affected package %s remains broken after repair or is pinned: %s", name, failures[0].Detail)
		}
		if old, existed := before[name]; existed && brewClassificationOf(old) != brewClassificationOf(pkg) {
			return fmt.Errorf("pre-existing Homebrew maintenance package %s changed classification", pkg.Source)
		}
		if !brewRecorded(rec.State(), pkg) {
			return &BrewDependentError{Name: name, Reason: failures[0].Detail,
				Block: engine.CommandBlock{Title: "Repair " + name + " with Homebrew", By: "you", Lines: []string{s.line(false, "reinstall", "--formula", pkg.Source)}}}
		}
		attempted[name] = true
		if err := d.begin(rec, mark, "", ""); err != nil {
			return err
		}
		if err := d.run(ctx, rec, s.command(false, "reinstall", "--formula", pkg.Source), mark, "", ""); err != nil {
			return err
		}
	}
}
