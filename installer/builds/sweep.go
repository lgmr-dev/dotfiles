package builds

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/fetch"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

// Sweep removes the unused private builds. The roots are the targets of the
// current links and of each staged link, the builds that a recorded target
// names, and the builds that the manifests of those builds use. A recorded
// target names a build in its record values, in the live target of a recorded
// link and in the live content, up to 1 MiB, of a recorded file, block or
// field. When Sweep cannot read a recorded target that exists, it removes no
// build and returns the reason. It
// removes an unused build only while its tree still equals its manifest, and
// then the manifest and an empty resource folder. It leaves a changed build, a
// folder without a valid manifest, a staged link that no unfinished
// publication records and each entry that it does not know, and returns them
// in left. An entry that appears during the removal also stays.
func Sweep(ctx context.Context, stateDir string, st state.State) (removed, left []string, err error) {
	return sweep(ctx, stateDir, st, true)
}

// Leftovers returns what Sweep would leave, and removes nothing. Check uses
// it.
func Leftovers(ctx context.Context, stateDir string, st state.State) ([]string, error) {
	_, left, err := sweep(ctx, stateDir, st, false)
	return left, err
}

type resourceFolder struct {
	id        string
	builds    []string
	manifests []string
}

func sweep(ctx context.Context, stateDir string, st state.State, remove bool) (removed, left []string, err error) {
	tools := ToolsDir(stateDir)
	if err := bound(filepath.Join(tools, "current")); err != nil {
		return nil, nil, err
	}
	ids, err := platform.ReadPlainDirectory(tools)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	folders := []resourceFolder{}
	roots := []string{}
	for _, entry := range ids {
		dir := filepath.Join(tools, entry.Name())
		if !entry.IsDir() || !resourceID.MatchString(entry.Name()) {
			left = append(left, dir)
			continue
		}
		folder := resourceFolder{id: entry.Name()}
		entries, err := platform.ReadPlainDirectory(dir)
		if err != nil {
			return nil, nil, err
		}
		for _, item := range entries {
			name, path := item.Name(), filepath.Join(dir, item.Name())
			switch {
			case name == "current", strings.HasPrefix(name, ".previous-"), strings.HasPrefix(name, ".next-"):
				target, err := linkTarget(path)
				if err != nil {
					return nil, nil, fmt.Errorf("setup cannot read which build %s uses, so it removed no build: %w", path, err)
				}
				roots = append(roots, target)
				if name != "current" && !recordedStagedLink(dir, path, st.Targets[CurrentDir(stateDir, folder.id)]) {
					left = append(left, path)
				}
			case item.IsDir() && buildName.MatchString(name):
				folder.builds = append(folder.builds, path)
			case !item.IsDir() && buildName.MatchString(strings.TrimSuffix(name, manifestSuffix)) && strings.HasSuffix(name, manifestSuffix):
				folder.manifests = append(folder.manifests, path)
			default:
				left = append(left, path)
			}
		}
		folders = append(folders, folder)
	}
	all := []string{}
	for _, folder := range folders {
		all = append(all, folder.builds...)
	}
	for key, record := range st.Targets {
		text, err := recordedReferences(key, record)
		if err != nil {
			return nil, nil, fmt.Errorf("setup cannot read the recorded target %s, which can name a build, so it removed no build: %w", key, err)
		}
		for _, build := range all {
			if slices.ContainsFunc(text, func(value string) bool { return namesBuild(value, build) }) {
				roots = append(roots, build)
			}
		}
	}
	used := map[string]bool{}
	for len(roots) != 0 {
		build := roots[0]
		roots = roots[1:]
		id := filepath.Base(filepath.Dir(build))
		if used[build] || !ownBuild(stateDir, id, build) {
			continue
		}
		used[build] = true
		if _, err := os.Lstat(build); errors.Is(err, os.ErrNotExist) {
			continue
		}
		manifest, err := readManifest(stateDir, id, build)
		if err != nil {
			return nil, nil, fmt.Errorf("setup cannot read which builds %s uses, so it removed no build: %w", build, err)
		}
		roots = append(roots, manifest.Uses...)
	}
	for _, folder := range folders {
		if err := ctx.Err(); err != nil {
			return removed, sortedUnique(left), err
		}
		dir := ResourceDir(stateDir, folder.id)
		for _, build := range folder.builds {
			if used[build] {
				continue
			}
			kept, err := discardBuild(stateDir, folder.id, build, remove)
			if err != nil {
				return removed, sortedUnique(append(left, build)), err
			}
			if len(kept) == 0 && remove {
				removed = append(removed, build)
			}
			left = append(left, kept...)
		}
		for _, path := range folder.manifests {
			build := strings.TrimSuffix(path, manifestSuffix)
			if used[build] {
				continue
			}
			if _, err := os.Lstat(build); !errors.Is(err, os.ErrNotExist) {
				continue
			}
			if !remove {
				continue
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return removed, sortedUnique(left), err
			}
		}
		if !remove {
			continue
		}
		if _, err := platform.RemoveEmptyDirectory(dir); err != nil {
			return removed, sortedUnique(left), err
		}
	}
	return removed, sortedUnique(left), nil
}

const maxReferenceBytes = 1 << 20

func recordedReferences(key string, record state.Record) ([]string, error) {
	text := []string{record.Applied, record.Next, record.Before}
	if !slices.Contains([]string{"link", "file", "block", "field"}, record.Kind) {
		return text, nil
	}
	path := key
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if cut := strings.LastIndex(key, "#"); cut > 0 {
			path = key[:cut]
			info, err = os.Lstat(path)
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		return text, nil
	}
	if err != nil {
		return nil, err
	}
	if record.Kind == "link" || info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		text = append(text, filepath.Clean(target))
		if record.Kind == "link" {
			return text, nil
		}
		info, err = os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return text, nil
		}
		if err != nil {
			return nil, err
		}
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("it is not a regular file")
	}
	if info.Size() > maxReferenceBytes {
		return text, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxReferenceBytes))
	if err := errors.Join(err, file.Close()); err != nil {
		return nil, err
	}
	return append(text, string(data)), nil
}

func namesBuild(text, build string) bool {
	for _, spelling := range []string{build, filepath.ToSlash(build), strings.ReplaceAll(build, `\`, `\\`)} {
		for rest := text; ; {
			at := strings.Index(rest, spelling)
			if at < 0 {
				break
			}
			rest = rest[at+len(spelling):]
			if rest == "" || !strings.ContainsRune("0123456789", rune(rest[0])) {
				return true
			}
		}
	}
	return false
}

func sortedUnique(values []string) []string {
	result := slices.Clone(values)
	slices.Sort(result)
	return slices.Compact(result)
}

func discardBuild(stateDir, id, build string, remove bool) ([]string, error) {
	manifest, err := readManifest(stateDir, id, build)
	if err != nil {
		return []string{build}, nil
	}
	if err := bound(build); err != nil {
		return []string{build}, nil
	}
	entries, err := platform.InspectTree(build, fetch.MaxPackageBytes, fetch.MaxPackageEntries)
	if err != nil {
		return []string{build}, nil
	}
	tree, err := platform.SnapshotEntries(entries)
	if err != nil || tree != manifest.Tree {
		return []string{build}, nil
	}
	if !remove {
		return nil, nil
	}
	return discardRecordedTree(build, entries, fetch.MaxPackageBytes, fetch.MaxPackageEntries)
}

func preserved(path string, kept []string) bool {
	for _, keep := range kept {
		relative, err := filepath.Rel(keep, path)
		if err == nil && (relative == "." || filepath.IsLocal(relative)) {
			return true
		}
	}
	return false
}

func discardRecordedTree(path string, entries []platform.Entry, byteLimit int64, entryLimit int) ([]string, error) {
	if err := validateCleanupEntries(entries, entryLimit); err != nil {
		return nil, err
	}
	if err := bound(path); err != nil {
		return []string{path}, nil
	}
	actual, err := platform.InspectTree(path, byteLimit, entryLimit)
	if err != nil {
		return nil, err
	}
	wanted := map[string]platform.Entry{}
	for _, entry := range entries {
		wanted[entry.Path] = entry
	}
	kept := []string{}
	for _, entry := range actual {
		if want, ok := wanted[entry.Path]; !ok || want != entry {
			kept = append(kept, filepath.Join(path, entry.Path))
		}
	}
	for _, entry := range slices.Backward(actual) {
		name := filepath.Join(path, entry.Path)
		if preserved(name, kept) {
			continue
		}
		if err := bound(name); err != nil {
			kept = append(kept, name)
			continue
		}
		current, err := platform.InspectTree(name, byteLimit, entryLimit)
		if err != nil {
			return sortedUnique(kept), err
		}
		if len(current) == 0 {
			continue
		}
		want := entry
		want.Path = "."
		if current[0] != want {
			kept = append(kept, name)
			continue
		}
		if len(current) != 1 {
			for _, child := range current[1:] {
				kept = append(kept, filepath.Join(name, child.Path))
			}
			continue
		}
		if err := platform.RemoveOwned(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return sortedUnique(kept), fmt.Errorf("cleanup remains at %s; retry after releasing it: %w", name, err)
		}
	}
	return sortedUnique(kept), nil
}

func validateCleanupEntries(entries []platform.Entry, limit int) error {
	if len(entries) == 0 || len(entries) > limit || entries[0].Path != "." {
		return errors.New("invalid cleanup entry list")
	}
	seen := map[string]platform.Entry{}
	for i, entry := range entries {
		if !filepath.IsLocal(entry.Path) || filepath.Clean(entry.Path) != entry.Path || entry.Mode > 0777 {
			return errors.New("cleanup entry has an invalid path or mode")
		}
		if _, exists := seen[entry.Path]; exists {
			return errors.New("duplicate cleanup entry")
		}
		if i > 0 && seen[filepath.Dir(entry.Path)].Kind != "directory" {
			return errors.New("cleanup entry parent is not a recorded directory")
		}
		switch entry.Kind {
		case "file":
			if !sha256Text.MatchString(entry.Content) {
				return errors.New("invalid cleanup content hash")
			}
		case "directory":
			if entry.Content != "" {
				return errors.New("invalid cleanup directory")
			}
		case "link", "junction":
			if entry.Content == "" || len(entry.Content) > 32768 || entry.Mode != 0 {
				return errors.New("invalid cleanup link")
			}
		default:
			return errors.New("invalid cleanup entry kind")
		}
		seen[entry.Path] = entry
	}
	return nil
}
