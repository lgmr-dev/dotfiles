package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/fetch"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

// Provider is the engine.Provider of each resource that has an archive
// binding. StateDir is the physical state folder. Pins holds the effective
// pin of each resource for the host, as HostPins returns them. Runner runs
// the preparation commands of the Python tools and of PortableGit. DpkgDeb is
// the absolute path of dpkg-deb for the Debian packages. Launcher is the
// installer executable and LauncherSHA256 its hash, for the Windows Rust
// launcher. A nil Client and a zero Transfer use the defaults of fetch.
type Provider struct {
	StateDir       string
	Pins           map[string]catalog.ArchivePin
	Client         *http.Client
	Runner         platform.Runner
	DpkgDeb        string
	Launcher       string
	LauncherSHA256 string
	Transfer       fetch.Transfer
}

type observation struct {
	Pin      string
	Record   state.Record
	Recorded bool
	Link     string
	LinkErr  string
	Manifest *Manifest
	Tree     platform.Snapshot
	Problem  string
}

func (p *Provider) pin(id string) (catalog.ArchivePin, error) {
	pin, ok := p.Pins[id]
	if !ok || !resourceID.MatchString(id) || !filepath.IsAbs(p.StateDir) || filepath.Clean(p.StateDir) != p.StateDir {
		return pin, fmt.Errorf("%s has no archive pin for this system, or the state folder is not an absolute path", id)
	}
	if err := bound(CurrentDir(p.StateDir, id)); err != nil {
		return pin, err
	}
	return pin, pin.Validate()
}

func (p *Provider) observe(id string, pin catalog.ArchivePin, st state.State, deep bool) observation {
	o := observation{Pin: Digest(pin, nil)}
	o.Record, o.Recorded = st.Targets[CurrentDir(p.StateDir, id)]
	link, err := linkTarget(CurrentDir(p.StateDir, id))
	if err != nil {
		o.LinkErr = err.Error()
		return o
	}
	o.Link = link
	if !deep || link == "" || !o.Recorded || link != o.Record.Applied && link != o.Record.Next {
		return o
	}
	manifest, err := readManifest(p.StateDir, id, link)
	if err != nil {
		o.Problem = "the manifest of the current build is not valid: " + err.Error()
		return o
	}
	o.Manifest = &manifest
	o.Tree, err = snapshotBuild(link)
	if err != nil {
		o.Problem = "setup cannot read the current build: " + err.Error()
	} else if o.Tree != manifest.Tree {
		o.Problem = "the current build differs from its manifest"
	}
	return o
}

func (o observation) token() string {
	data, err := json.Marshal(o)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (p *Provider) effective(id string, pin catalog.ArchivePin) (digest string, uses []string, waiting string, err error) {
	for _, base := range baseResources(pin) {
		basePin, ok := p.Pins[base]
		if !ok {
			return "", nil, "", fmt.Errorf("%s uses %s, which has no archive pin for this system", id, base)
		}
		path, err := BuildPath(p.StateDir, base)
		if err != nil {
			waiting = base + " has no usable private build"
			continue
		}
		uses = append(uses, path)
		want, _, baseWaiting, err := p.effective(base, basePin)
		if err != nil {
			return "", nil, "", err
		}
		manifest, err := readManifest(p.StateDir, base, path)
		if err != nil || baseWaiting != "" || manifest.Digest != want {
			waiting = "the build of " + base + " changes with its reviewed pin"
		}
	}
	return Digest(pin, uses), uses, waiting, nil
}

// Inspect reads the current link, its record and the manifest and the tree of
// the current build. It reports Missing without a current build, Outdated when
// the digest of the reviewed pin differs from the current build, Unhealthy
// when the current build fails its manifest or its file check, Changed when
// the link is not the recorded one, and Blocked for an unrecorded link.
func (p *Provider) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	if err := ctx.Err(); err != nil {
		return engine.Finding{}, err
	}
	pin, err := p.pin(r.ID)
	if err != nil {
		return engine.Finding{}, err
	}
	current := CurrentDir(p.StateDir, r.ID)
	o := p.observe(r.ID, pin, rec.State(), true)
	f := engine.Finding{Token: o.token()}
	if o.Manifest != nil {
		f.Version = o.Manifest.Version
	}
	switch {
	case o.LinkErr != "":
		f.Status, f.Reason = engine.Blocked, o.LinkErr
	case o.Link == "":
		f.Status = engine.Missing
		if !want.Present && !o.Recorded {
			f.Status = engine.Absent
		}
		if o.Recorded {
			f.Reason = "the current link is missing"
		}
	case !o.Recorded:
		f.Status, f.Reason = engine.Blocked, "an unrecorded link exists at "+current+"; move it away, then run setup again"
	case o.Link != o.Record.Applied && o.Link != o.Record.Next:
		f.Status, f.Reason, f.Preserved = engine.Changed, "the current link points at "+o.Link+", which setup did not publish", []string{current}
	case o.Problem != "":
		f.Status, f.Reason = engine.Unhealthy, o.Problem
		if !want.Present {
			f.Status, f.Preserved = engine.Changed, []string{o.Link}
		}
	case o.Link != o.Record.Applied || o.Record.Next != "" || o.Record.Release:
		f.Status, f.Reason = engine.Unhealthy, "the publication of the current link is not complete"
	default:
		digest, _, waiting, err := p.effective(r.ID, pin)
		if err != nil {
			return engine.Finding{}, err
		}
		switch {
		case waiting != "":
			f.Status, f.Reason = engine.Outdated, waiting
		case o.Manifest.Digest != digest:
			f.Status, f.Reason = engine.Outdated, "the reviewed pin is version "+pin.Version
			if o.Manifest.Version == pin.Version {
				f.Reason = "the reviewed recipe or a build that it uses changed"
			}
		default:
			if err := checkPayload(o.Link, pin); err != nil {
				f.Status, f.Reason = engine.Unhealthy, err.Error()
			} else {
				f.Status = engine.Satisfied
			}
		}
	}
	if note := routeNote(r.ID, pin); note != "" && want.Present && (f.Status == engine.Missing || f.Status == engine.Outdated || f.Status == engine.Unhealthy) {
		f.Notes = []string{note}
	}
	return f, nil
}

func routeNote(id string, pin catalog.ArchivePin) string {
	switch {
	case id == "tool.openwhispr" && pin.Format == "tar.gz":
		return "official portable launcher; on Linux with restricted user namespaces upstream automatically disables Chromium's sandbox; personal models and settings are retained"
	case id == "tool.obsidian-portable":
		return "official portable build; its launcher starts it without Chromium's sandbox, which needs user namespaces that Ubuntu restricts by default; vaults and settings are retained"
	case id == "tool.t3-code-app":
		return "official .deb unpacked privately without its install scripts; its launcher starts it without Chromium's sandbox and without automatic updates, and Update tools moves it to the reviewed release; T3 settings and threads are retained"
	}
	return ""
}

// Apply gives the resource a verified build with the digest of the reviewed
// pin and publishes the current link. It reuses a build that has a manifest,
// the same digest and an unchanged tree. Otherwise it downloads and prepares a
// new build at its final path, checks it there and writes its manifest. An
// unfinished build has no manifest and is never reused. A link that setup did
// not publish stays unless the approved finding permits its replacement.
func (p *Provider) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	pin, err := p.pin(r.ID)
	if err != nil {
		return err
	}
	o := p.observe(r.ID, pin, rec.State(), false)
	if o.LinkErr != "" {
		return errors.New(o.LinkErr)
	}
	foreign := o.Link != "" && (!o.Recorded || o.Link != o.Record.Applied && o.Link != o.Record.Next)
	if foreign && !(approved.Replace && o.Recorded) {
		return fmt.Errorf("the link %s is not the one that setup published; it stays", CurrentDir(p.StateDir, r.ID))
	}
	dir := ResourceDir(p.StateDir, r.ID)
	for _, folder := range []string{p.StateDir, ToolsDir(p.StateDir), dir} {
		if err := platform.PrepareDirectory(folder); err != nil {
			return err
		}
	}
	digest, uses, _, err := p.effective(r.ID, pin)
	if err != nil {
		return err
	}
	if len(uses) != len(baseResources(pin)) {
		return fmt.Errorf("%s uses %s, which has no private build; apply that resource first", r.ID, strings.Join(baseResources(pin), ", "))
	}
	candidate, err := p.reusable(r.ID, pin, digest)
	if err != nil {
		return err
	}
	if candidate == "" {
		candidate, err = p.build(ctx, r.ID, pin, digest, uses)
		if err != nil {
			return err
		}
	}
	return Publish(p.StateDir, r.ID, candidate, rec)
}

func (p *Provider) verified(id string, pin catalog.ArchivePin, digest, build string) bool {
	manifest, err := readManifest(p.StateDir, id, build)
	if err != nil || manifest.Digest != digest {
		return false
	}
	tree, err := snapshotBuild(build)
	return err == nil && tree == manifest.Tree && checkPayload(build, pin) == nil
}

func (p *Provider) reusable(id string, pin catalog.ArchivePin, digest string) (string, error) {
	candidates, err := Candidates(p.StateDir, id, pin.Version, digest)
	if err != nil {
		return "", err
	}
	if link, err := linkTarget(CurrentDir(p.StateDir, id)); err == nil && slices.Contains(candidates, link) && p.verified(id, pin, digest, link) {
		return link, nil
	}
	for _, build := range slices.Backward(candidates) {
		if p.verified(id, pin, digest, build) {
			return build, nil
		}
	}
	return "", nil
}

func (p *Provider) build(ctx context.Context, id string, pin catalog.ArchivePin, digest string, uses []string) (string, error) {
	build, err := nextBuild(ResourceDir(p.StateDir, id), pin.Version, digest)
	if err != nil {
		return "", err
	}
	if pin.WindowsRustLauncher != nil {
		launcher := *pin.WindowsRustLauncher
		launcher.SHA256 = p.LauncherSHA256
		pin.WindowsRustLauncher = &launcher
	}
	platform.Detail(ctx, "Building %s %s at %s", id, pin.Version, build)
	if err := p.prepare(ctx, pin, build, uses); err != nil {
		if _, statErr := os.Lstat(build); statErr == nil {
			return "", fmt.Errorf("%w; the unfinished build stays at %s and the next run uses a new folder", err, build)
		}
		return "", err
	}
	tree, err := snapshotBuild(build)
	if err != nil {
		return "", err
	}
	if err := WriteManifest(build, Manifest{Schema: manifestSchema, Resource: id, Version: pin.Version, Digest: digest, Tree: tree, Uses: uses}); err != nil {
		return "", err
	}
	return build, nil
}

// Remove releases the current link of the resource and its record. The link
// goes only while it is the one that setup published. The builds stay for
// Sweep.
func (p *Provider) Remove(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := p.pin(r.ID); err != nil {
		return err
	}
	return Unpublish(p.StateDir, r.ID, rec)
}
