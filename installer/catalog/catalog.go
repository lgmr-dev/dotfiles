package catalog

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/luisgui1757/dotfiles/installer/platform"
)

// Catalog is the validated content of every tool folder. Resources holds the
// menu entries and the hidden resources, in folder order.
type Catalog struct {
	Resources []Resource
	Targets   []Target
	Shell     []Fragment
	Pins      Pins
}

var resourceID = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,79}$`)
var folderName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
var aptArgument = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*(?::[a-z0-9][a-z0-9-]*)?$`)
var brewPackageName = regexp.MustCompile(`^[a-z0-9][a-z0-9+_.@-]{0,199}$`)

var systems = []string{"darwin", "linux", "windows"}

var contexts = []platform.Context{{OS: "darwin", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}, {OS: "windows", Arch: "amd64"}}

// Available reports if the resource exists on the context. An empty
// architecture examines the system only.
func (r Resource) Available(c platform.Context) bool {
	return (len(r.Platforms) == 0 || slices.Contains(r.Platforms, c.OS)) &&
		(c.Arch == "" || len(r.PlatformArchitectures[c.OS]) == 0 || slices.Contains(r.PlatformArchitectures[c.OS], c.Arch))
}

// Dependencies returns the sorted requirements of the resource on the context.
func (r Resource) Dependencies(c platform.Context) []string {
	deps := append([]string{}, r.Requires...)
	deps = append(deps, r.PlatformRequires[c.OS]...)
	sort.Strings(deps)
	return slices.Compact(deps)
}

// Path returns the fragment source relative to the root of the tool folders.
func (f Fragment) Path() string {
	return path.Join(f.Folder, f.Source)
}

// Load decodes each tool.json and pin file of the tool folders in fsys and
// validates the result. An unknown field, file or schema fails.
func Load(fsys fs.FS) (*Catalog, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	c := &Catalog{Pins: Pins{Archives: map[string]map[string]ArchivePin{}}}
	home := map[string]string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		folder := entry.Name()
		if !folderName.MatchString(folder) {
			return nil, fmt.Errorf("invalid tool folder name %q: use lowercase words and hyphens", folder)
		}
		data, err := fs.ReadFile(fsys, path.Join(folder, "tool.json"))
		if err != nil {
			return nil, fmt.Errorf("tool folder %s: %w", folder, err)
		}
		var tool Tool
		if err := platform.Decode(data, &tool); err != nil {
			return nil, fmt.Errorf("%s/tool.json: %w", folder, err)
		}
		if tool.Schema != 1 {
			return nil, fmt.Errorf("%s/tool.json: unsupported tool schema %d", folder, tool.Schema)
		}
		if len(tool.Resources) == 0 {
			return nil, fmt.Errorf("%s/tool.json: a tool folder needs a resource", folder)
		}
		for _, r := range tool.Resources {
			if previous, exists := home[r.ID]; exists {
				return nil, fmt.Errorf("duplicate resource %q in the tool folders %s and %s", r.ID, previous, folder)
			}
			home[r.ID] = folder
		}
		for _, target := range tool.Targets {
			if home[target.Resource] != folder {
				return nil, fmt.Errorf("%s/tool.json: the target %s belongs to the resource %s, which is not in this folder", folder, target.Path, target.Resource)
			}
		}
		for i := range tool.Shell {
			fragment := &tool.Shell[i]
			fragment.Folder = folder
			if home[fragment.Resource] != folder {
				return nil, fmt.Errorf("%s/tool.json: the shell fragment %s belongs to the resource %s, which is not in this folder", folder, fragment.Source, fragment.Resource)
			}
			if !slices.Contains([]string{"zsh", "bash", "posix", "powershell"}, fragment.Shell) || !canonicalRelative(fragment.Source) || !strings.HasPrefix(fragment.Source, "shell/") {
				return nil, fmt.Errorf("%s/tool.json: invalid shell fragment %s", folder, fragment.Source)
			}
			info, err := fs.Stat(fsys, fragment.Path())
			if err != nil || !info.Mode().IsRegular() {
				return nil, errors.Join(fmt.Errorf("%s/tool.json: the shell fragment %s has no source file", folder, fragment.Source), err)
			}
		}
		c.Resources = append(c.Resources, tool.Resources...)
		c.Targets = append(c.Targets, tool.Targets...)
		c.Shell = append(c.Shell, tool.Shell...)
		if err := c.Pins.load(fsys, folder, home); err != nil {
			return nil, err
		}
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func validSystem(os string) bool {
	return slices.Contains(systems, os)
}

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

func isHomebrewPackage(provider string) bool {
	return provider == "homebrew-formula" || provider == "homebrew-cask"
}

func (c *Catalog) validate() error {
	seen := make(map[string]bool, len(c.Resources))
	orders := map[int]string{}
	packageOwners := map[string]string{}
	grouped := slices.ContainsFunc(c.Resources, func(r Resource) bool { return r.Category != "" })
	for _, r := range c.Resources {
		if !resourceID.MatchString(r.ID) || r.Name == "" {
			return fmt.Errorf("invalid resource %q", r.ID)
		}
		if seen[r.ID] {
			return fmt.Errorf("duplicate resource %q", r.ID)
		}
		if grouped && r.Capability != (r.Category != "") || strings.TrimSpace(r.Category) != r.Category || strings.ContainsFunc(r.Category, unicode.IsControl) {
			return fmt.Errorf("invalid category for %s: categorize every capability with a plain, nonempty label, and no hidden resources", r.ID)
		}
		if r.Capability != (r.Order > 0) || r.Order < 0 {
			return fmt.Errorf("invalid menu position for %s: give each capability, and no hidden resource, a positive order", r.ID)
		}
		if previous, exists := orders[r.Order]; exists && r.Capability {
			return fmt.Errorf("%s and %s have the same menu position %d", previous, r.ID, r.Order)
		}
		if r.Capability {
			orders[r.Order] = r.ID
		}
		if r.Scope != "" && r.Scope != "user" && r.Scope != "machine" {
			return fmt.Errorf("invalid scope for %s", r.ID)
		}
		if r.ReplanAfter && (!r.Retain || r.Action == "" || r.Capability || len(r.Bindings) != 0) {
			return fmt.Errorf("replan boundary %s must be retained bootstrap infrastructure", r.ID)
		}
		for _, os := range r.Platforms {
			if !validSystem(os) {
				return fmt.Errorf("invalid platform %q", os)
			}
		}
		for os := range r.PlatformRequires {
			if !validSystem(os) {
				return fmt.Errorf("invalid dependency platform %q", os)
			}
		}
		for os, architectures := range r.PlatformArchitectures {
			if len(architectures) == 0 || !r.Available(platform.Context{OS: os}) {
				return fmt.Errorf("invalid architecture restriction for %s", r.ID)
			}
			for _, arch := range architectures {
				if err := (platform.Context{OS: os, Arch: arch}).Validate(); err != nil {
					return err
				}
			}
		}
		for os, binding := range r.Bindings {
			if !validSystem(os) || !r.Available(platform.Context{OS: os}) || binding.Package == "" || r.Action == "" {
				return fmt.Errorf("invalid package binding for %s on %s", r.ID, os)
			}
			if binding.Provider != "archive" && !isHomebrewPackage(binding.Provider) && binding.Provider != "apt" && binding.Provider != "windows-app" {
				return fmt.Errorf("invalid package provider %q for %s", binding.Provider, r.ID)
			}
			if binding.Provider == "apt" && (os != "linux" || !aptArgument.MatchString(binding.Package)) {
				return fmt.Errorf("invalid APT binding for %s", r.ID)
			}
			if isHomebrewPackage(binding.Provider) {
				if _, err := brewShortName(binding.Package); err != nil || os != "darwin" || !slices.Contains(r.Dependencies(platform.Context{OS: os}), "infra.homebrew") {
					return fmt.Errorf("invalid Homebrew formula binding for %s", r.ID)
				}
			}
			if binding.Provider == "homebrew-cask" && map[string]string{"app.zen": "zen", "app.t3-code": "t3-code"}[r.ID] != binding.Package {
				return fmt.Errorf("unsupported native application %s", r.ID)
			}
			if binding.Provider == "windows-app" && (os != "windows" || r.ID != "app.t3-code-windows" || binding.Package != "t3-code" || r.Scope != "user") {
				return fmt.Errorf("unsupported Windows application %s", r.ID)
			}
			key := os + "/" + binding.Provider + "/" + binding.Package
			if previous, exists := packageOwners[key]; exists {
				return fmt.Errorf("package %s is assigned to both %s and %s", key, previous, r.ID)
			}
			packageOwners[key] = r.ID
		}
		seen[r.ID] = true
	}
	for _, r := range c.Resources {
		requirements := append([]string{}, r.Requires...)
		for _, ids := range r.PlatformRequires {
			requirements = append(requirements, ids...)
		}
		for _, id := range requirements {
			if !seen[id] {
				return fmt.Errorf("%s requires the unknown resource %q", r.ID, id)
			}
		}
	}
	for _, ctx := range contexts {
		roots := []string{}
		for _, r := range c.Resources {
			if r.Available(ctx) {
				roots = append(roots, r.ID)
			}
		}
		if _, err := c.Closure(roots, ctx); err != nil {
			return err
		}
	}
	if err := c.validateTargets(); err != nil {
		return err
	}
	return c.Pins.validate(c)
}

// Resource returns the resource with the ID.
func (c *Catalog) Resource(id string) (Resource, bool) {
	for _, r := range c.Resources {
		if r.ID == id {
			return r, true
		}
	}
	return Resource{}, false
}

// Menu returns the capabilities that are available on the context, in menu
// order.
func (c *Catalog) Menu(ctx platform.Context) []Resource {
	result := []Resource{}
	for _, r := range c.Resources {
		if r.Capability && r.Available(ctx) {
			result = append(result, r)
		}
	}
	slices.SortStableFunc(result, func(a, b Resource) int { return a.Order - b.Order })
	return result
}

// Closure returns the roots and their requirements, each requirement before
// its consumer. It rejects an unknown or unavailable resource and a cycle.
func (c *Catalog) Closure(roots []string, ctx platform.Context) ([]string, error) {
	result, marks := []string{}, map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		r, ok := c.Resource(id)
		if !ok {
			return fmt.Errorf("unknown resource %q", id)
		}
		if !r.Available(ctx) {
			return fmt.Errorf("%s is unavailable on %s", r.Name, ctx.OS)
		}
		if marks[id] == 1 {
			return fmt.Errorf("dependency cycle at %s", id)
		}
		if marks[id] == 2 {
			return nil
		}
		marks[id] = 1
		for _, dependency := range r.Dependencies(ctx) {
			if err := visit(dependency); err != nil {
				return fmt.Errorf("%s requires %s: %w", id, dependency, err)
			}
		}
		marks[id] = 2
		result = append(result, id)
		return nil
	}
	ordered := append([]string{}, roots...)
	sort.Strings(ordered)
	for _, root := range ordered {
		if err := visit(root); err != nil {
			return nil, err
		}
	}
	return result, nil
}
