package system

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var brewPackageName = regexp.MustCompile(`^[a-z0-9][a-z0-9+_.@-]{0,199}$`)

func brewShortName(full string) (string, error) {
	parts := strings.Split(full, "/")
	if len(parts) != 1 && len(parts) != 3 {
		return "", errors.New("invalid Homebrew package identity")
	}
	for _, part := range parts {
		if !brewPackageName.MatchString(part) {
			return "", errors.New("invalid Homebrew package identity")
		}
	}
	return parts[len(parts)-1], nil
}

func brewSortedUnique(values []string) []string {
	result := append([]string{}, values...)
	slices.Sort(result)
	return slices.Compact(result)
}

type brewPackage struct {
	Name         string
	Version      string
	Automatic    bool
	Held         bool
	Healthy      bool
	Source       string
	Dependencies []string
}

func brewInventory(ctx context.Context, query MacQuery) (map[string]brewPackage, error) {
	data, err := query(ctx, "brew", "info", "--json=v2", "--installed")
	if err != nil {
		return nil, fmt.Errorf("inspect installed Homebrew packages: %w", err)
	}
	if len(data) > 8<<20 {
		return nil, errors.New("Homebrew inventory exceeds 8 MiB")
	}
	var document struct {
		Formulae []struct {
			Name      string `json:"name"`
			FullName  string `json:"full_name"`
			Pinned    bool   `json:"pinned"`
			KegOnly   bool   `json:"keg_only"`
			Linked    string `json:"linked_keg"`
			Installed []struct {
				Version      string `json:"version"`
				OnRequest    *bool  `json:"installed_on_request"`
				Dependencies *[]struct {
					FullName string `json:"full_name"`
				} `json:"runtime_dependencies"`
			} `json:"installed"`
		} `json:"formulae"`
		Casks []struct {
			Token     string `json:"token"`
			FullToken string `json:"full_token"`
			Installed string `json:"installed"`
			DependsOn *struct {
				Formula []string `json:"formula"`
			} `json:"depends_on"`
		} `json:"casks"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if document.Formulae == nil || document.Casks == nil {
		return nil, errors.New("Homebrew omitted installed inventory collections")
	}
	installed := map[string]brewPackage{}
	queryDependencies := func(kind, name string) ([]string, error) {
		data, err := query(ctx, "brew", "deps", "--installed", "--"+kind, name)
		if err != nil {
			return nil, fmt.Errorf("inspect installed dependencies of %s: %w", name, err)
		}
		if len(data) > 1<<20 {
			return nil, errors.New("Homebrew dependency list exceeds its bound")
		}
		return strings.Fields(string(data)), nil
	}
	add := func(pkg brewPackage) error {
		if _, duplicate := installed[pkg.Name]; duplicate || pkg.Version == "" {
			return errors.New("duplicate or incomplete Homebrew package identity")
		}
		for i, full := range pkg.Dependencies {
			name, err := brewShortName(full)
			if err != nil {
				return err
			}
			pkg.Dependencies[i] = name
		}
		pkg.Dependencies = brewSortedUnique(pkg.Dependencies)
		installed[pkg.Name] = pkg
		return nil
	}
	for _, formula := range document.Formulae {
		name, err := brewShortName(formula.FullName)
		if err != nil || name != formula.Name || len(formula.Installed) == 0 {
			return nil, errors.New("invalid installed Homebrew formula")
		}
		pkg := brewPackage{Name: name, Source: formula.FullName, Automatic: true, Held: formula.Pinned}
		versions := []string{}
		missing := false
		for _, keg := range formula.Installed {
			if keg.Version == "" || len(keg.Version) > 256 || strings.ContainsAny(keg.Version, "\x00\r\n") {
				return nil, errors.New("invalid Homebrew keg version")
			}
			versions = append(versions, keg.Version)
			pkg.Automatic = pkg.Automatic && keg.OnRequest != nil && !*keg.OnRequest
			if keg.Dependencies == nil {
				missing = true
				continue
			}
			for _, dep := range *keg.Dependencies {
				pkg.Dependencies = append(pkg.Dependencies, dep.FullName)
			}
		}
		if missing {
			deps, err := queryDependencies("formula", formula.FullName)
			if err != nil {
				return nil, err
			}
			pkg.Dependencies = append(pkg.Dependencies, deps...)
		}
		pkg.Version = strings.Join(brewSortedUnique(versions), ",")
		pkg.Healthy = formula.KegOnly || formula.Linked != "" && slices.Contains(versions, formula.Linked)
		if err := add(pkg); err != nil {
			return nil, err
		}
	}
	for _, cask := range document.Casks {
		name, err := brewShortName(cask.FullToken)
		if err != nil || name != cask.Token || cask.Installed == "" {
			return nil, errors.New("invalid installed Homebrew cask")
		}
		pkg := brewPackage{Name: "cask/" + name, Source: cask.FullToken, Version: cask.Installed, Healthy: true}
		if cask.DependsOn != nil {
			pkg.Dependencies = cask.DependsOn.Formula
		} else {
			pkg.Dependencies, err = queryDependencies("cask", cask.FullToken)
			if err != nil {
				return nil, err
			}
		}
		if err := add(pkg); err != nil {
			return nil, err
		}
	}
	return installed, nil
}

func brewOutdated(ctx context.Context, query MacQuery, full string) (current string, outdated bool, err error) {
	data, queryErr := query(ctx, "brew", "outdated", "--json=v2", "--formula", full)
	var document struct {
		Formulae []struct {
			Name    string `json:"name"`
			Current string `json:"current_version"`
		} `json:"formulae"`
	}
	if len(data) > 1<<20 || json.Unmarshal(data, &document) != nil || document.Formulae == nil {
		return "", false, errors.Join(errors.New("Homebrew gave no list of outdated formulae"), queryErr)
	}
	name, err := brewShortName(full)
	if err != nil {
		return "", false, err
	}
	for _, formula := range document.Formulae {
		if formula.Name == name || formula.Name == full {
			return formula.Current, true, nil
		}
	}
	return "", false, nil
}
