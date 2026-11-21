package system

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

const homebrewBootstrapCommit = "c9c975046f78e6e6d662deb801bd461ebd6a54ed"
const homebrewBootstrapSHA256 = "8fd2f1a5287a7c2e3d7362924c677edcc04c5a0e16448c77132d056bc88cf1ed"
const homebrewBootstrapURL = "https://raw.githubusercontent.com/Homebrew/install/" + homebrewBootstrapCommit + "/install.sh"

// HomebrewBootstrap is the provider of infra.homebrew. It installs Homebrew
// only into an absent or empty prefix, with the official installer at a
// pinned commit and SHA-256. It uses a healthy Homebrew as it is and never
// updates, repairs or removes one: a global `brew update` can change packages
// that setup did not install. Discover runs at each inspection, so a new
// Homebrew is visible without a new process. Runner runs `sudo -n mkdir` and
// the installer in the foreground. Setup keeps no record of Homebrew.
type HomebrewBootstrap struct {
	Home        string
	Discover    func(context.Context) (*HomebrewLocation, error)
	Query       MacQuery
	Runner      platform.Runner
	HTTPClient  *http.Client
	prefix      string
	url, sha256 string
}

// NewHomebrewBootstrap returns the provider for the home folder. It is pure.
func NewHomebrewBootstrap(home string) *HomebrewBootstrap {
	return &HomebrewBootstrap{Home: home, Discover: DiscoverHomebrew, Runner: platform.Exec{}, prefix: "/opt/homebrew",
		url: homebrewBootstrapURL, sha256: homebrewBootstrapSHA256}
}

type homebrewObservation struct {
	Location HomebrewLocation
	Kind     string
	Identity string
	Version  string
	Issue    string
}

func (d *HomebrewBootstrap) observe(ctx context.Context, r catalog.Resource) (homebrewObservation, error) {
	if r.ID != "infra.homebrew" || d.Discover == nil || !macosCleanPath(d.Home) || !macosCleanPath(d.prefix) {
		return homebrewObservation{}, errors.New("invalid Homebrew bootstrap resource boundary")
	}
	existing, issue, err := homebrewLocate(ctx, d.Discover)
	if err != nil {
		return homebrewObservation{}, err
	}
	o := homebrewObservation{Location: HomebrewLocation{Program: filepath.Join(d.prefix, "bin", "brew"), Prefix: d.prefix, Cellar: filepath.Join(d.prefix, "Cellar")}}
	if existing != nil {
		o.Location = *existing
	}
	if issue != "" {
		o.Kind, o.Issue = "issue", issue
		return o, nil
	}
	o.Identity, err = homebrewPrefixIdentity(o.Location.Prefix)
	if errors.Is(err, os.ErrNotExist) {
		o.Kind = "absent"
		return o, nil
	}
	if err != nil {
		return o, err
	}
	info, err := os.Stat(o.Location.Program)
	if errors.Is(err, os.ErrNotExist) {
		entries, err := os.ReadDir(o.Location.Prefix)
		if err != nil {
			return o, err
		}
		o.Kind = "occupied"
		if len(entries) == 0 {
			o.Kind = "empty"
		}
		return o, nil
	}
	if err != nil {
		return o, err
	}
	query := d.query(o.Location)
	output, checkErr := query(ctx, "brew", "--version")
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || checkErr != nil || !strings.HasPrefix(string(output), "Homebrew ") {
		o.Kind, o.Issue = "broken", "the retained Homebrew executable needs native repair"
		if checkErr != nil {
			o.Issue = checkErr.Error()
		}
		return o, ctx.Err()
	}
	o.Version, _, _ = strings.Cut(strings.TrimPrefix(string(output), "Homebrew "), "\n")
	for option, expected := range map[string]string{"--prefix": o.Location.Prefix, "--cellar": o.Location.Cellar} {
		data, err := query(ctx, "brew", option)
		if err != nil || strings.TrimSuffix(string(data), "\n") != expected {
			return o, errors.Join(errors.New("Homebrew rediscovery differs from its declared infrastructure"), err)
		}
	}
	o.Kind = "healthy"
	return o, nil
}

// Inspect reports a healthy Homebrew as satisfied and an absent or empty
// prefix as missing. A prefix with other content and no working manager is
// unknown, and a broken manager is blocked: setup keeps both.
func (d *HomebrewBootstrap) Inspect(ctx context.Context, r catalog.Resource, _ engine.Want, _ engine.Records) (engine.Finding, error) {
	o, err := d.observe(ctx, r)
	if err != nil {
		return engine.Finding{}, err
	}
	token, err := macosDigest(o)
	if err != nil {
		return engine.Finding{}, err
	}
	f := engine.Finding{Status: engine.Satisfied, Version: o.Version, Token: token}
	switch o.Kind {
	case "issue":
		f.Status, f.Reason = engine.Unknown, o.Issue
	case "absent", "empty":
		f.Status, f.Privileged, f.Pending, f.Replan = engine.Missing, true, true, true
		f.Reason = "install Homebrew at " + o.Location.Prefix + " with the official installer at the reviewed commit; Homebrew and any installed Command Line Tools stay after a removal"
		lines := []string{macosShellLine(d.reserveCommand(o.Location.Prefix))}
		if o.Kind == "empty" {
			lines = nil
		}
		f.Commands = []engine.CommandBlock{{Title: "Install Homebrew (installer commit " + homebrewBootstrapCommit[:12] + ", SHA-256 " + d.sha256[:12] + ")", By: "setup",
			Lines: append(lines, macosShellLine(d.installCommand(o.Location.Prefix, nil))+" < install.sh")}}
	case "occupied":
		f.Status, f.Reason = engine.Unknown, "the Homebrew prefix "+o.Location.Prefix+" already exists with content and without a working manager; setup keeps it. Examine it, then repair Homebrew there or move its content away, and run setup again. Setup installs into an absent or empty folder"
	case "broken":
		f.Status, f.Reason = engine.Blocked, "the existing Homebrew installation needs native repair before it can be reused: "+o.Issue
	}
	return f, nil
}

func (d *HomebrewBootstrap) source(ctx context.Context, location HomebrewLocation) error {
	data, err := d.query(location)(ctx, "/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "/usr/bin/git", "-C", location.Prefix, "config", "--local", "--get", "remote.origin.url")
	source := strings.TrimSpace(string(data))
	if err != nil || source != "https://github.com/Homebrew/brew" && source != "https://github.com/Homebrew/brew.git" {
		return errors.Join(errors.New("retained Homebrew source is not its original official repository; preserve it"), err)
	}
	return nil
}

func homebrewBootstrapMark(r catalog.Resource) string { return r.ID + ":install" }

// Apply installs Homebrew. It downloads and verifies the installer before
// any privileged command, reserves the prefix, writes the pending mark and
// runs the installer. It clears the mark after the manager passes its checks,
// comes from the official repository and holds no package.
func (d *HomebrewBootstrap) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	o, err := d.observe(ctx, r)
	if err != nil {
		return err
	}
	if approved.Status != engine.Missing || o.Kind != "absent" && o.Kind != "empty" {
		return errors.New("Homebrew bootstrap requires an approved install into an absent prefix; retained manager updates and repairs are not implicit")
	}
	payload, err := d.payload(ctx)
	if err != nil {
		return err
	}
	prefix := o.Location.Prefix
	mark := homebrewBootstrapMark(r)
	if err := rec.Update(func(s *state.State) error { s.Mark(mark); return nil }); err != nil {
		return err
	}
	if o.Kind == "absent" {
		_, started, err := macosRun(ctx, d.Runner, d.reserveCommand(prefix))
		if err != nil && !started {
			return errors.Join(fmt.Errorf("the Homebrew prefix reservation did not start: %w", err), macosClearMark(rec, mark))
		}
		if err != nil {
			return fmt.Errorf("Homebrew infrastructure command failed; authorize administrator access in the foreground or resolve the reported native prerequisite, run setup again and confirm that the command exited: %w", err)
		}
	}
	identity, err := homebrewPrefixIdentity(prefix)
	if err == nil {
		var entries []os.DirEntry
		entries, err = os.ReadDir(prefix)
		if err == nil && (len(entries) != 0 || o.Kind == "empty" && identity != o.Identity) {
			err = errors.New("the prefix has content or is another folder")
		}
	}
	if err != nil {
		return errors.Join(fmt.Errorf("the Homebrew prefix %s changed before the installer started, and the installer did not run: %w. Examine %s. If it holds nothing of yours, remove the empty folder that setup reserved (sudo /bin/rmdir %s), then run setup again. Setup also uses an empty folder there as it is", prefix, err, prefix, macosShellWord(prefix)), macosClearMark(rec, mark))
	}
	_, started, err := macosRun(ctx, d.Runner, d.installCommand(prefix, payload))
	if err != nil && !started {
		return errors.Join(fmt.Errorf("the Homebrew installer did not start: %w", err), macosClearMark(rec, mark))
	}
	if err != nil {
		return fmt.Errorf("Homebrew infrastructure command failed; authorize administrator access in the foreground or resolve the reported native prerequisite, run setup again and confirm that the command exited: %w", err)
	}
	after, err := d.observe(ctx, r)
	if err != nil || after.Kind != "healthy" || after.Identity != identity || after.Location.Prefix != prefix {
		return errors.Join(errors.New("Homebrew bootstrap has not passed native rediscovery and health checks"), err)
	}
	if err := d.source(ctx, after.Location); err != nil {
		return err
	}
	installed, err := brewInventory(ctx, d.query(after.Location))
	if err != nil {
		return err
	}
	if len(installed) != 0 {
		return errors.New("unexpected packages appeared during bootstrap; review them, then run setup again and confirm that the command exited")
	}
	return macosClearMark(rec, mark)
}

// Remove always fails: Homebrew and its packages stay.
func (*HomebrewBootstrap) Remove(context.Context, catalog.Resource, engine.Finding, engine.Records) error {
	return errors.New("Homebrew infrastructure and Command Line Tools are retained; package removal cannot uninstall them")
}
