package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

type debPin struct {
	Package, Version, Architecture, URL, SHA256 string
	Size                                        int64
}

func (a *APT) debPin(r catalog.Resource, name string) (debPin, bool, error) {
	if r.ID == "tool.vscode" {
		if name != "code" {
			return debPin{}, false, errors.New("Code requires its fixed native package pin")
		}
		pin, err := a.Pins.LinuxCodePin(a.Arch)
		if err != nil {
			return debPin{}, false, err
		}
		if err := pin.Validate(); err != nil || pin.Architecture != a.Arch {
			return debPin{}, false, errors.Join(errors.New("invalid Linux Code platform pin"), err)
		}
		return debPin{"code", pin.Version, pin.Architecture, pin.URL, pin.SHA256, pin.Size}, true, nil
	}
	if name == "code" {
		return debPin{}, false, errors.New("Code package is bound to an unexpected resource")
	}
	pin, ok := a.Pins.LinuxPackage(r.ID, a.Arch)
	if !ok {
		if name == "tailscale" {
			return debPin{}, false, fmt.Errorf("%s lacks its reviewed native package", r.ID)
		}
		return debPin{}, false, nil
	}
	if err := pin.Validate(); err != nil || pin.Package != name || pin.Architecture != a.Arch {
		return debPin{}, false, errors.Join(errors.New("invalid native application binding"), err)
	}
	return debPin{pin.Package, pin.Version, pin.Architecture, pin.URL, pin.SHA256, pin.Size}, true, nil
}

func (a *APT) debFile(pin debPin) string {
	return filepath.Join(a.Directory, "payloads", pin.SHA256, pin.Package+".deb")
}

func (a *APT) debBound(pin debPin) (string, error) {
	file := a.debFile(pin)
	bound, err := platform.BindDestination(file)
	if err != nil || bound != file {
		return "", errors.Join(fmt.Errorf("%s package input was redirected", pin.Package), err)
	}
	return file, nil
}

func (a *APT) debVerify(pin debPin) error {
	file, err := a.debBound(pin)
	if err != nil {
		return err
	}
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() != pin.Size {
		return errors.Join(fmt.Errorf("%s package input is missing, redirected or damaged", pin.Package), err)
	}
	handle, err := os.Open(file)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, readErr := io.Copy(hash, io.LimitReader(handle, pin.Size+1))
	if err := errors.Join(readErr, handle.Close()); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != pin.SHA256 {
		return fmt.Errorf("%s package input differs from its reviewed SHA-256", pin.Package)
	}
	return nil
}

func (a *APT) debPrepare(ctx context.Context, pin debPin) (result error) {
	file, err := a.debBound(pin)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(file); errors.Is(err, os.ErrNotExist) {
		if a.Download == nil {
			return fmt.Errorf("%s needs the download of its reviewed package, and this run has no downloader", pin.Package)
		}
		parent := filepath.Dir(filepath.Dir(file))
		if err := os.MkdirAll(parent, 0700); err != nil {
			return err
		}
		stage, err := os.MkdirTemp(parent, ".deb-download-")
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, os.RemoveAll(stage)) }()
		payload := filepath.Join(stage, "payload")
		if err := os.Mkdir(payload, 0700); err != nil {
			return err
		}
		if err := a.Download(ctx, pin.URL, pin.SHA256, pin.Size, filepath.Join(payload, filepath.Base(file))); err != nil {
			return err
		}
		if err := os.Rename(payload, filepath.Dir(file)); err != nil {
			return err
		}
	}
	return a.debVerify(pin)
}

func (a *APT) debCleanup(pin debPin) error {
	file, err := a.debBound(pin)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Dir(file))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 1 || len(entries) == 1 && entries[0].Name() != filepath.Base(file) {
		return fmt.Errorf("unexpected %s package input is preserved for inspection", pin.Package)
	}
	if len(entries) == 1 {
		if err := a.debVerify(pin); err != nil {
			return err
		}
		if err := os.Remove(file); err != nil {
			return err
		}
	}
	return os.Remove(filepath.Dir(file))
}

var debCodePublications = []string{
	"/usr/share/code", "/usr/bin/code", "/usr/local/bin/code",
	"/usr/share/appdata/com.microsoft.VSCode.appdata.xml",
	"/usr/share/applications/com.microsoft.VSCode.desktop",
	"/usr/share/applications/com.microsoft.VSCode.UrlHandler.desktop",
	"/usr/share/bash-completion/completions/code", "/usr/share/mime/packages/code-workspace.xml",
	"/usr/share/pixmaps/vscode.png", "/usr/share/zsh/vendor-completions/_code",
}

func debPublications(name string, absent bool) []string {
	switch {
	case name == "code" && absent:
		return debCodePublications
	case name == "code":
		return []string{"/usr/local/bin/code"}
	case name == "tailscale" && absent:
		return []string{"/usr/bin/tailscale", "/usr/sbin/tailscaled", "/lib/systemd/system/tailscaled.service", "/usr/lib/systemd/system/tailscaled.service"}
	}
	return nil
}

func debCollisionScript(paths []string) string {
	script := "for p in"
	for _, path := range paths {
		script += " " + aptShellLiteral(path)
	}
	return script + "; do if [ -e \"$p\" ] || [ -L \"$p\" ]; then printf '%s\\n' \"$p\"; fi; done"
}

func (a *APT) debCollisions(ctx context.Context, name string, absent bool) error {
	paths := debPublications(name, absent)
	if len(paths) == 0 {
		return nil
	}
	output, err := a.Query(ctx, "/bin/sh", "-c", debCollisionScript(paths))
	if err == nil && strings.TrimSpace(string(output)) == "" {
		return nil
	}
	if name == "code" {
		return errors.Join(fmt.Errorf("existing Code publication must be preserved; move or remove the conflicting personal installation before retrying%s", platform.Diagnostic(output)), err)
	}
	return errors.Join(fmt.Errorf("%s has a conflicting or changed native publication; preserve it%s", name, platform.Diagnostic(output)), err)
}

const debCodeHealth = "set -eu\n" +
	"fail() { printf 'Code native health: %s\\n' \"$*\" >&2; exit 1; }\n" +
	"sandbox=$1 cache=$2\n" +
	"shift 2\n" +
	"link=$(readlink /usr/bin/code) || fail '/usr/bin/code target unreadable'\n" +
	"[ \"$link\" = /usr/share/code/bin/code ] || fail \"/usr/bin/code target=$link\"\n" +
	"for p in /usr /usr/share /usr/share/code /usr/share/code/code /usr/share/code/chrome-sandbox; do\n" +
	"  [ ! -L \"$p\" ] || fail \"$p is a symbolic link\"\n" +
	"  owner=$(stat -c %u \"$p\") || fail \"$p owner unreadable\"\n" +
	"  mode=$(stat -c %a \"$p\") || fail \"$p mode unreadable\"\n" +
	"  expected=755\n" +
	"  [ \"$p\" != /usr/share/code/chrome-sandbox ] || expected=$sandbox\n" +
	"  [ \"$owner\" = 0 ] && [ \"$mode\" = \"$expected\" ] || fail \"$p owner=$owner mode=$mode (expected owner=0 mode=$expected)\"\n" +
	"done\n" +
	"if [ -n \"$cache\" ]; then\n" +
	"  changed=$(find \"$cache\" ! -type d ! \\( -type f -user 0 -group 0 -perm 600 \\) -print -quit) || fail \"$cache unreadable\"\n" +
	"  [ -z \"$changed\" ] || fail \"$changed is not a regular root:root 0600 file\"\n" +
	"fi\n" +
	"for unreadable do\n" +
	"  [ -n \"$cache\" ] && case $unreadable in \"$cache\"/*) true ;; *) false ;; esac || fail \"$unreadable is unreadable but not declared root-only\"\n" +
	"done\n"

const debCodeDatabase = "Name: code/add-copilot-command\nTemplate: code/add-copilot-command\nValue: false\nOwners: code\nFlags: seen\n\n" +
	"Name: code/add-microsoft-repo\nTemplate: code/add-microsoft-repo\nValue: false\nOwners: code\nFlags: seen\n\n"

var debCodeDatabaseFiles = []string{"answer.dat", "answer.dat-new", "answer.dat-old"}

type debCodeInput struct{ Parent, Directory, Identity string }

func debCodeDirectory(parent string) string {
	if resolved, err := platform.ResolvePath(parent); err == nil {
		parent = resolved
	}
	return filepath.Join(parent, "dotfiles-code-debconf-"+strconv.Itoa(os.Geteuid()))
}

func debCodeAnswers(directory string) string { return filepath.Join(directory, "answer.dat") }

func debCodeOverride(directory string) string {
	return "DEBCONF_DB_OVERRIDE=File{filename:" + debCodeAnswers(directory) + " accept_name:^code/add-(copilot-command|microsoft-repo)$ mode:0644 backup:no}"
}

func newDebCodeInput(parent string) (*debCodeInput, error) {
	parent, err := platform.ResolvePath(parent)
	if err != nil {
		return nil, err
	}
	directory := debCodeDirectory(parent)
	err = os.Mkdir(directory, 0700)
	if errors.Is(err, os.ErrExist) {
		identity, identityErr := debTemporaryIdentity(directory)
		if identityErr == nil {
			identityErr = debCodeInput{Parent: parent, Directory: directory, Identity: identity}.cleanup()
		}
		if identityErr != nil {
			return nil, fmt.Errorf("the Code temporary input %s of an earlier run is not as setup wrote it; examine it and delete it: %w", directory, identityErr)
		}
		err = os.Mkdir(directory, 0700)
	}
	if err != nil {
		return nil, err
	}
	identity, err := debTemporaryIdentity(directory)
	if err != nil {
		return nil, errors.Join(err, os.Remove(directory))
	}
	input := &debCodeInput{Parent: parent, Directory: directory, Identity: identity}
	if err := input.validate(); err != nil {
		return nil, errors.Join(err, os.Remove(directory))
	}
	file, err := os.OpenFile(debCodeAnswers(directory), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return nil, errors.Join(err, os.Remove(directory))
	}
	_, writeErr := io.WriteString(file, debCodeDatabase)
	if err := errors.Join(writeErr, file.Chmod(0644), file.Sync(), file.Close()); err != nil {
		return nil, fmt.Errorf("the Code temporary input %s is incomplete; examine it and delete it: %w", directory, err)
	}
	return input, nil
}

func (input debCodeInput) validate() error {
	if filepath.Dir(input.Directory) != input.Parent || !strings.HasPrefix(filepath.Base(input.Directory), "dotfiles-code-debconf-") ||
		strings.ContainsAny(input.Directory, " \t\r\n\x00{}") || input.Identity == "" {
		return errors.New("invalid Code temporary input: its folder path must have no space, brace or control character")
	}
	return nil
}

func (input debCodeInput) inspect(absentOK bool) error {
	if err := input.validate(); err != nil {
		return err
	}
	identity, err := debTemporaryIdentity(input.Directory)
	if absentOK && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || identity != input.Identity {
		return errors.Join(errors.New("Code temporary input changed owner, identity or location"), err)
	}
	entries, err := os.ReadDir(input.Directory)
	if err != nil {
		return err
	}
	found := false
	for _, entry := range entries {
		if !slices.Contains(debCodeDatabaseFiles, entry.Name()) {
			return errors.New("unexpected Code temporary input is preserved for inspection")
		}
		data, err := debReadDatabase(filepath.Join(input.Directory, entry.Name()), len(debCodeDatabase))
		if err != nil || string(data) != debCodeDatabase {
			return errors.Join(errors.New("changed Code temporary database bytes are preserved for inspection"), err)
		}
		found = found || entry.Name() == "answer.dat"
	}
	if !found && !absentOK {
		return errors.New("Code temporary database is missing")
	}
	return nil
}

func (input debCodeInput) cleanup() error {
	if err := input.inspect(true); err != nil {
		return err
	}
	for _, name := range debCodeDatabaseFiles {
		if err := os.Remove(filepath.Join(input.Directory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(input.Directory); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
