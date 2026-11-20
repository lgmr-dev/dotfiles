package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/builds"
	"github.com/luisgui1757/dotfiles/installer/fetch"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

const nvimManifestSuffix = ".manifest.json"

var nvimBuildName = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}-[0-9a-f]{12}-[1-9][0-9]{0,8}$`)

func nvimOwnBuild(stateDir, path string) bool {
	return filepath.Dir(path) == builds.ResourceDir(stateDir, NvimResource) && nvimBuildName.MatchString(filepath.Base(path))
}

func nvimBuildPrefix(version, digest string) string { return version + "-" + digest[:12] + "-" }

func nvimLinkTarget(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0 {
		return "", fmt.Errorf("%s is not a directory link; move it away", path)
	}
	target, err := os.Readlink(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		return "", fmt.Errorf("%s is not an absolute link; move it away", path)
	}
	return filepath.Clean(target), nil
}

func nvimBound(path string) error {
	actual, err := platform.BindDestination(path)
	if err != nil || actual != path {
		return errors.Join(fmt.Errorf("a parent folder of %s was redirected; examine it", path), err)
	}
	return nil
}

func nvimReadManifest(stateDir, build string) (builds.Manifest, error) {
	var manifest builds.Manifest
	if !nvimOwnBuild(stateDir, build) {
		return manifest, fmt.Errorf("%s is not a Neovim runtime of setup", build)
	}
	path := build + nvimManifestSuffix
	if err := nvimBound(path); err != nil {
		return manifest, err
	}
	data, err := platform.ReadDocument(path)
	if err != nil {
		return manifest, err
	}
	if err := platform.Decode(data, &manifest); err != nil {
		return manifest, err
	}
	if manifest.Schema != 1 || manifest.Resource != NvimResource || len(manifest.Digest) != 64 || manifest.Tree.Kind != "directory" ||
		!strings.HasPrefix(filepath.Base(build), nvimBuildPrefix(manifest.Version, manifest.Digest)) {
		return manifest, fmt.Errorf("invalid build manifest %s", path)
	}
	return manifest, nil
}

func nvimWriteManifest(build string, manifest builds.Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return platform.SaveDocument(build+nvimManifestSuffix, append(data, '\n'))
}

func nvimSnapshot(build string) (platform.Snapshot, error) {
	if err := nvimBound(build); err != nil {
		return platform.Snapshot{}, err
	}
	return platform.SnapshotTreeBounded(build, fetch.MaxPackageBytes, fetch.MaxPackageEntries)
}

func nvimBuilds(dir, version, digest string) (names []string, serial int, err error) {
	entries, err := platform.ReadPlainDirectory(dir)
	if err != nil {
		return nil, 0, err
	}
	prefix := nvimBuildPrefix(version, digest)
	for _, entry := range entries {
		rest, ok := strings.CutPrefix(strings.TrimSuffix(entry.Name(), nvimManifestSuffix), prefix)
		if !ok {
			continue
		}
		number, err := strconv.Atoi(rest)
		if err != nil {
			return nil, 0, fmt.Errorf("unexpected entry %s; move it away", filepath.Join(dir, entry.Name()))
		}
		serial = max(serial, number)
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, serial, nil
}

func nvimStagedLink(dir, kind, build string) string {
	return filepath.Join(dir, "."+kind+"-"+filepath.Base(build))
}

func nvimSwitchLink(dir, build string) error {
	current := filepath.Join(dir, "current")
	previous, next := nvimStagedLink(dir, "previous", build), nvimStagedLink(dir, "next", build)
	actual, err := nvimLinkTarget(current)
	if err != nil {
		return err
	}
	if actual == build {
		return nil
	}
	if actual != "" {
		if _, err := os.Lstat(previous); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(fmt.Errorf("an earlier link stays at %s; examine it and move it away", previous), err)
		}
		if err := platform.MoveNoReplace(current, previous); err != nil {
			return err
		}
	}
	staged, err := nvimLinkTarget(next)
	if err != nil {
		return err
	}
	if staged == "" {
		if err := platform.CreateDirectoryLink(build, next); err != nil {
			return err
		}
		if err := platform.SyncDirectory(dir); err != nil {
			return err
		}
	} else if staged != build {
		return fmt.Errorf("the staged link %s changed; examine it and move it away", next)
	}
	if err := platform.MoveNoReplace(next, current); err != nil {
		return err
	}
	return platform.SyncDirectory(dir)
}

func nvimRemoveStagedLinks(stateDir string) error {
	dir := builds.ResourceDir(stateDir, NvimResource)
	entries, err := platform.ReadPlainDirectory(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".previous-") && !strings.HasPrefix(entry.Name(), ".next-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		target, err := nvimLinkTarget(path)
		if err != nil || !nvimOwnBuild(stateDir, target) {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}
