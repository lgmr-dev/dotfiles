package catalog

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

// ResolvedTarget is one configuration target with absolute paths. Mode is
// link, copy or junction.
type ResolvedTarget struct {
	Source, Destination string
	Directory           bool
	Mode                string
}

func canonicalRelative(value string) bool {
	return value != "." && !path.IsAbs(value) && path.Clean(value) == value &&
		filepath.IsLocal(filepath.FromSlash(value)) && !strings.ContainsAny(value, "\\\x00\r\n:")
}

func (c *Catalog) validateTargets() error {
	seen := map[string]bool{}
	owners := map[string]bool{}
	for _, target := range c.Targets {
		r, found := c.Resource(target.Resource)
		if !found || r.Action != "config" || !canonicalRelative(target.Source) || !canonicalRelative(target.Path) {
			return fmt.Errorf("invalid configuration mapping for %s", target.Resource)
		}
		if target.Mode != "" && target.Mode != "copy" && (target.Mode != "junction" || !target.Directory || len(target.Platforms) != 1 || target.Platforms[0] != "windows") {
			return errors.New("explicit configuration modes require a copy or Windows-only directory junction")
		}
		if !slices.Contains([]string{"home", "config", "local_app_data", "app_data", "documents"}, target.Folder) {
			return fmt.Errorf("unknown configuration folder %s", target.Folder)
		}
		for _, os := range target.Platforms {
			if !validSystem(os) || !r.Available(platform.Context{OS: os}) {
				return fmt.Errorf("configuration target has invalid platform %s", os)
			}
		}
		for _, os := range systems {
			if !r.Available(platform.Context{OS: os}) || len(target.Platforms) != 0 && !slices.Contains(target.Platforms, os) {
				continue
			}
			if os != "windows" && slices.Contains([]string{"local_app_data", "app_data", "documents"}, target.Folder) {
				return errors.New("Windows known folder mapped to a POSIX target")
			}
			key := os + "/" + target.Folder + "/" + strings.ToLower(target.Path)
			if seen[key] {
				return fmt.Errorf("duplicate configuration destination %s", key)
			}
			seen[key], owners[os+"/"+r.ID] = true, true
		}
	}
	for _, r := range c.Resources {
		for _, os := range systems {
			if r.Action == "config" && r.Available(platform.Context{OS: os}) && !owners[os+"/"+r.ID] {
				return fmt.Errorf("configuration resource %s has no target on %s", r.ID, os)
			}
		}
	}
	return nil
}

// TargetsFor resolves the configuration targets of a resource to the known
// folders and the checkout. It does not create or read a destination.
func (c *Catalog) TargetsFor(resource string, ctx platform.Context, f platform.Folders, checkout string) ([]ResolvedTarget, error) {
	r, exists := c.Resource(resource)
	if !exists || r.Action != "config" || !r.Available(ctx) || !filepath.IsAbs(checkout) || filepath.Clean(checkout) != checkout {
		return nil, errors.New("configuration resolution requires an available resource and canonical source root")
	}
	roots := map[string]string{"home": f.Home, "config": f.Config, "local_app_data": f.LocalAppData, "app_data": f.AppData, "documents": f.Documents}
	resolved := []ResolvedTarget{}
	for _, item := range c.Targets {
		if item.Resource != resource || len(item.Platforms) != 0 && !slices.Contains(item.Platforms, ctx.OS) {
			continue
		}
		root := roots[item.Folder]
		if !filepath.IsAbs(root) || filepath.Clean(root) != root || strings.ContainsAny(root, "\x00\r\n") {
			return nil, fmt.Errorf("selected configuration requires the actual %s known folder", item.Folder)
		}
		source := filepath.Join(checkout, filepath.FromSlash(item.Source))
		destination := filepath.Join(root, filepath.FromSlash(item.Path))
		for _, pair := range [][2]string{{source, destination}, {destination, source}, {destination, checkout}, {checkout, destination}} {
			relative, err := filepath.Rel(pair[0], pair[1])
			if err == nil && (relative == "." || filepath.IsLocal(relative)) {
				return nil, fmt.Errorf("source and destination overlap for %s", resource)
			}
		}
		mode := "link"
		if ctx.OS == "windows" {
			mode = "copy"
		}
		if item.Mode != "" {
			mode = item.Mode
		}
		resolved = append(resolved, ResolvedTarget{source, destination, item.Directory, mode})
	}
	return resolved, nil
}
