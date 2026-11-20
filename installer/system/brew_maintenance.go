package system

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

type brewClassification struct {
	Source          string
	Held, Automatic bool
}

func brewClassificationOf(pkg brewPackage) brewClassification {
	return brewClassification{Source: pkg.Source, Held: pkg.Held, Automatic: pkg.Automatic}
}

func brewAffected(root string, before, installed map[string]brewPackage) map[string]bool {
	affected := map[string]bool{root: true}
	for name, pkg := range installed {
		if old, existed := before[name]; !strings.HasPrefix(name, "cask/") && (!existed || old.Version != pkg.Version) {
			affected[name] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for name, pkg := range before {
			if affected[name] {
				continue
			}
			if slices.ContainsFunc(pkg.Dependencies, func(dependency string) bool { return affected[dependency] }) {
				affected[name], changed = true, true
			}
		}
	}
	return affected
}

func (d *Brew) maintainDependents(ctx context.Context, s *brewSession, rec engine.Records, mark string, b brewBinding, before map[string]brewPackage) error {
	attempted := map[string]bool{}
	for {
		installed, err := brewInventory(ctx, s.query)
		if err != nil {
			return err
		}
		affected := brewAffected(b.name, before, installed)
		names := make([]string, 0, len(affected))
		for name := range affected {
			names = append(names, name)
		}
		slices.Sort(names)
		arguments := []string{"linkage", "--test"}
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
			arguments = append(arguments, pkg.Source)
		}
		batchOutput, batchErr := s.query(ctx, "brew", arguments...)
		if batchErr == nil {
			for _, source := range casks {
				engine.ReportDetail(ctx, "External application not exercised: %s", source)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(batchErr, context.DeadlineExceeded) || errors.Is(batchErr, context.Canceled) {
			return fmt.Errorf("Homebrew affected-package linkage inspection interrupted: %w%s", batchErr, platform.Diagnostic(batchOutput))
		}
		repaired := false
		for _, name := range formulae {
			pkg := installed[name]
			output, linkErr := s.query(ctx, "brew", "linkage", "--test", pkg.Source)
			if linkErr == nil {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(linkErr, context.DeadlineExceeded) || errors.Is(linkErr, context.Canceled) {
				return fmt.Errorf("Homebrew affected package %s linkage inspection interrupted: %w%s", name, linkErr, platform.Diagnostic(output))
			}
			if pkg.Held || attempted[name] {
				return fmt.Errorf("Homebrew affected package %s remains broken after repair or is pinned: %w%s", name, linkErr, platform.Diagnostic(output))
			}
			if old, existed := before[name]; existed && brewClassificationOf(old) != brewClassificationOf(pkg) {
				return fmt.Errorf("pre-existing Homebrew maintenance package %s changed classification", pkg.Source)
			}
			attempted[name] = true
			if err := d.run(ctx, rec, s.command(false, "reinstall", "--formula", pkg.Source), mark, "", ""); err != nil {
				return err
			}
			repaired = true
			break
		}
		if !repaired {
			return fmt.Errorf("Homebrew affected-package linkage check failed: %w%s", batchErr, platform.Diagnostic(batchOutput))
		}
	}
}
