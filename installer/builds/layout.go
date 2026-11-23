// Package builds owns the private tool builds under the state folder: it
// downloads the pinned archive of a resource, prepares the build at its final
// path, checks it, writes its manifest and publishes the current link.
package builds

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/fetch"
	"github.com/lgmr-dev/dotfiles/installer/platform"
)

const (
	recipeRevision = 1
	manifestSchema = 1
	manifestSuffix = ".manifest.json"
)

var (
	resourceID = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,79}$`)
	sha256Text = regexp.MustCompile(`^[0-9a-f]{64}$`)
	buildName  = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}-[0-9a-f]{12}-[1-9][0-9]{0,8}$`)
)

// Manifest is the record that setup writes beside a build after its check
// passes: the file "<build>.manifest.json". Digest covers the effective pin,
// the recipe revision and the exact path of each build in Uses. Tree is the
// fingerprint of the build folder. A build without a manifest is unfinished.
type Manifest struct {
	Schema   int               `json:"schema"`
	Resource string            `json:"resource"`
	Version  string            `json:"version"`
	Digest   string            `json:"digest"`
	Tree     platform.Snapshot `json:"tree"`
	Uses     []string          `json:"uses,omitempty"`
}

// ToolsDir returns the folder that holds the private builds of each resource.
func ToolsDir(stateDir string) string { return filepath.Join(stateDir, "tools") }

// ResourceDir returns the folder that holds the builds of one resource.
func ResourceDir(stateDir, id string) string { return filepath.Join(ToolsDir(stateDir), id) }

// CurrentDir returns the stable link to the active build of a resource. The
// link can be absent. The path is also the state key of its link record.
func CurrentDir(stateDir, id string) string {
	return filepath.Join(ResourceDir(stateDir, id), "current")
}

// CommandPath returns the stable path of a command of a resource, through the
// current link. It is empty when the pin has no such command.
func CommandPath(stateDir, id, command string, pin catalog.ArchivePin) string {
	name, ok := pin.Commands[command]
	if !ok {
		return ""
	}
	return filepath.Join(CurrentDir(stateDir, id), filepath.FromSlash(name))
}

// BinaryDirectories returns the stable command folders of a resource, through
// the current link, in pin order.
func BinaryDirectories(stateDir, id string, pin catalog.ArchivePin) []string {
	paths := make([]string, len(pin.BinDirs))
	for i, dir := range pin.BinDirs {
		paths[i] = filepath.Join(CurrentDir(stateDir, id), filepath.FromSlash(dir))
	}
	return paths
}

// RequiredFilePath returns the stable path of a required file of a resource
// that is not a command. It is empty when the pin does not declare the file.
func RequiredFilePath(stateDir, id, name string, pin catalog.ArchivePin) string {
	if !slices.Contains(pin.RequiredFiles, name) {
		return ""
	}
	return filepath.Join(CurrentDir(stateDir, id), filepath.FromSlash(name))
}

// BuildPath returns the exact folder of the active build of a resource: the
// target of its current link. A build that uses another build calls this
// path, so a later change of the link does not change it.
func BuildPath(stateDir, id string) (string, error) {
	target, err := linkTarget(CurrentDir(stateDir, id))
	if err != nil {
		return "", err
	}
	if target == "" {
		return "", fmt.Errorf("%s has no private build", id)
	}
	if !ownBuild(stateDir, id, target) {
		return "", fmt.Errorf("the current link of %s points outside its builds; examine %s", id, CurrentDir(stateDir, id))
	}
	return target, nil
}

// HostPins returns the effective archive pin of each family that has one for
// the host, by resource ID.
func HostPins(pins catalog.Pins, host platform.Host) (map[string]catalog.ArchivePin, error) {
	result := map[string]catalog.ArchivePin{}
	for family := range pins.Archives {
		pin, err := pins.Archive(family, host)
		if errors.Is(err, catalog.ErrNoArchive) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result[family] = pin
	}
	return result, nil
}

// Digest returns the identity of a build: the SHA-256 of the effective pin,
// the recipe revision and the exact paths of the builds that it uses. The
// hash of the Windows Rust launcher executable is not part of it, because a
// new installer build must not make a verified toolchain outdated.
func Digest(pin catalog.ArchivePin, uses []string) string {
	if pin.WindowsRustLauncher != nil {
		launcher := *pin.WindowsRustLauncher
		launcher.SHA256 = ""
		pin.WindowsRustLauncher = &launcher
	}
	data, err := json.Marshal(struct {
		Recipe int      `json:"recipe"`
		Pin    string   `json:"pin"`
		Uses   []string `json:"uses"`
	}{recipeRevision, catalog.ArchivePinID(pin), append([]string{}, uses...)})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ownBuild(stateDir, id, path string) bool {
	return filepath.Dir(path) == ResourceDir(stateDir, id) && buildName.MatchString(filepath.Base(path))
}

func versionLabel(version string) string {
	label := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._+-", r) {
			return r
		}
		return '_'
	}, version)
	if len(label) > 64 {
		label = label[:64]
	}
	return label
}

func buildPrefix(version, digest string) string {
	return versionLabel(version) + "-" + digest[:12] + "-"
}

func manifestPath(build string) string { return build + manifestSuffix }

func linkTarget(path string) (string, error) {
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

func bound(path string) error {
	actual, err := platform.BindDestination(path)
	if err != nil || actual != path {
		return errors.Join(fmt.Errorf("a parent folder of %s was redirected; examine it", path), err)
	}
	return nil
}

func readManifest(stateDir, id, build string) (Manifest, error) {
	var manifest Manifest
	if !ownBuild(stateDir, id, build) {
		return manifest, fmt.Errorf("%s is not a build of %s", build, id)
	}
	path := manifestPath(build)
	if err := bound(path); err != nil {
		return manifest, err
	}
	data, err := platform.ReadDocument(path)
	if err != nil {
		return manifest, err
	}
	if err := platform.Decode(data, &manifest); err != nil {
		return manifest, err
	}
	if manifest.Schema != manifestSchema || manifest.Resource != id || !sha256Text.MatchString(manifest.Digest) || manifest.Tree.Kind != "directory" ||
		!strings.HasPrefix(filepath.Base(build), buildPrefix(manifest.Version, manifest.Digest)) {
		return manifest, fmt.Errorf("invalid build manifest %s", path)
	}
	for _, used := range manifest.Uses {
		if !filepath.IsAbs(used) || filepath.Clean(used) != used || !ownBuild(stateDir, filepath.Base(filepath.Dir(used)), used) {
			return manifest, fmt.Errorf("invalid build manifest %s", path)
		}
	}
	return manifest, nil
}

func snapshotBuild(build string) (platform.Snapshot, error) {
	if err := bound(build); err != nil {
		return platform.Snapshot{}, err
	}
	return platform.SnapshotTreeBounded(build, fetch.MaxPackageBytes, fetch.MaxPackageEntries)
}

func nextBuild(dir, version, digest string) (string, error) {
	entries, err := platform.ReadPlainDirectory(dir)
	if err != nil {
		return "", err
	}
	prefix, serial := buildPrefix(version, digest), 0
	for _, entry := range entries {
		rest, ok := strings.CutPrefix(strings.TrimSuffix(entry.Name(), manifestSuffix), prefix)
		if !ok {
			continue
		}
		number, err := strconv.Atoi(rest)
		if err != nil {
			return "", fmt.Errorf("unexpected entry %s; move it away", filepath.Join(dir, entry.Name()))
		}
		serial = max(serial, number)
	}
	name := prefix + strconv.Itoa(serial+1)
	if !buildName.MatchString(name) {
		return "", fmt.Errorf("no free build name remains in %s", dir)
	}
	return filepath.Join(dir, name), nil
}

// LinkTarget returns the cleaned absolute target of the directory link at
// path, or an empty text when path does not exist.
func LinkTarget(path string) (string, error) { return linkTarget(path) }

// Bound fails when a parent folder of path is a link or was redirected.
func Bound(path string) error { return bound(path) }

// ReadManifest reads and validates the manifest of a build of a resource.
func ReadManifest(stateDir, id, build string) (Manifest, error) {
	return readManifest(stateDir, id, build)
}

// WriteManifest saves the manifest beside a build. A recipe calls it after
// the check of the build passed.
func WriteManifest(build string, manifest Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return platform.SaveDocument(manifestPath(build), append(data, '\n'))
}

// SnapshotBuild returns the tree fingerprint of a build within the bounds of
// one extracted payload.
func SnapshotBuild(build string) (platform.Snapshot, error) { return snapshotBuild(build) }

// NextBuild returns the folder of a new build of a resource: the version, the
// first 12 characters of the digest and the next free serial number. It
// creates nothing.
func NextBuild(stateDir, id, version, digest string) (string, error) {
	return nextBuild(ResourceDir(stateDir, id), version, digest)
}

// Candidates returns the build folders of a resource that have the name of
// the version and the digest, oldest first.
func Candidates(stateDir, id, version, digest string) ([]string, error) {
	dir := ResourceDir(stateDir, id)
	entries, err := platform.ReadPlainDirectory(dir)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), buildPrefix(version, digest)) {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	for i, name := range names {
		names[i] = filepath.Join(dir, name)
	}
	return names, nil
}
