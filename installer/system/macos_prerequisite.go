package system

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

// MacPrerequisites is the provider of tool.compiler, tool.make and
// tool.clipboard on macOS. It uses the Apple developer tools that
// xcode-select names and the clipboard commands of the system. It installs,
// updates and removes nothing: infra.apple-clt installs missing Command Line
// Tools. Its inspection never runs a compiler shim in /usr/bin while no
// developer directory is selected, because the shim can open an installation
// dialog.
type MacPrerequisites struct {
	Query             MacQuery
	clipboardCommands []string
}

// NewMacPrerequisites returns the provider. It is pure.
func NewMacPrerequisites() *MacPrerequisites {
	return &MacPrerequisites{Query: MacPrerequisiteQuery, clipboardCommands: []string{"/usr/bin/pbcopy", "/usr/bin/pbpaste"}}
}

type macosCheck struct {
	Status      engine.Status
	Reason      string
	Fingerprint string
	Notes       []string
}

func (c macosCheck) finding() (engine.Finding, error) {
	token, err := macosDigest(c)
	return engine.Finding{Status: c.Status, Reason: c.Reason, Notes: c.Notes, Token: token}, err
}

// Inspect reports the selected developer tools or the clipboard commands.
func (d *MacPrerequisites) Inspect(ctx context.Context, r catalog.Resource, _ engine.Want, _ engine.Records) (engine.Finding, error) {
	if err := ctx.Err(); err != nil {
		return engine.Finding{}, err
	}
	if d.Query == nil {
		return engine.Finding{}, errors.New("Apple prerequisite provider requires a read-only query boundary")
	}
	var check macosCheck
	var err error
	switch r.ID {
	case "tool.compiler", "tool.make":
		check, err = macosToolchain(ctx, d.Query, r.ID)
	case "tool.clipboard":
		check, err = d.clipboard(ctx)
	default:
		return engine.Finding{}, errors.New("resource is not an Apple prerequisite")
	}
	if err != nil {
		return engine.Finding{}, err
	}
	return check.finding()
}

func macosToolchain(ctx context.Context, query MacQuery, id string) (macosCheck, error) {
	unknown := func(reason string) (macosCheck, error) {
		return macosCheck{Status: engine.Unknown, Reason: reason}, nil
	}
	blocked := func(reason string, err error) (macosCheck, error) {
		if err != nil {
			reason += ": " + err.Error()
		}
		return macosCheck{Status: engine.Blocked, Reason: reason}, nil
	}
	data, err := query(ctx, "/usr/bin/xcode-select", "--print-path")
	if err != nil {
		if ctx.Err() != nil {
			return macosCheck{}, ctx.Err()
		}
		return unknown("Apple developer tools are not selected; install Command Line Tools with xcode-select --install, or select your intended Xcode with xcode-select --switch, then check again")
	}
	selected, err := macosDeveloperPath(data)
	if err != nil {
		return macosCheck{}, err
	}
	root, err := platform.ResolvePath(selected)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return macosCheck{}, err
		}
		return unknown("the selected Apple developer directory is missing; install Command Line Tools or select your existing Xcode, then check again")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return macosCheck{}, errors.Join(errors.New("selected Apple developer path is not a directory"), err)
	}
	tools := []string{"make"}
	if id == "tool.compiler" {
		tools = []string{"clang", "clang++"}
	}
	evidence := map[string]string{"developer_directory": root}
	for _, name := range tools {
		data, err := query(ctx, "/usr/bin/xcrun", "--no-cache", "--find", name)
		if err != nil {
			if ctx.Err() != nil {
				return macosCheck{}, ctx.Err()
			}
			return unknown("the selected Apple developer tools cannot resolve " + name + "; complete any Xcode license/setup action or repair the selected tools, then check again")
		}
		path, err := macosDeveloperPath(data)
		if err != nil {
			return macosCheck{}, err
		}
		path, err = platform.ResolvePath(path)
		if err != nil {
			return macosCheck{}, err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return macosCheck{}, errors.Join(errors.New("xcrun resolved a command outside its selected developer directory"), err)
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return macosCheck{}, errors.Join(errors.New("selected Apple developer command is missing or not executable"), err)
		}
		data, err = query(ctx, path, "--version")
		if ctx.Err() != nil {
			return macosCheck{}, ctx.Err()
		}
		prefix := "Apple clang version "
		if name == "make" {
			prefix = "GNU Make "
		}
		if err != nil || !strings.HasPrefix(string(data), prefix) {
			return blocked("the selected Apple "+name+" command failed its native health check; repair the selected developer tools", err)
		}
		version, _, _ := strings.Cut(string(data), "\n")
		evidence[name], evidence[name+"_version"] = path, version
	}
	if id == "tool.compiler" {
		data, err := query(ctx, "/usr/bin/xcrun", "--no-cache", "--sdk", "macosx", "--show-sdk-path")
		if err != nil {
			if ctx.Err() != nil {
				return macosCheck{}, ctx.Err()
			}
			return blocked("the selected Apple compiler has no usable macOS SDK; complete or repair its developer-tools installation", nil)
		}
		sdk, err := macosDeveloperPath(data)
		if err != nil {
			return macosCheck{}, err
		}
		info, err := os.Stat(sdk)
		if err != nil || !info.IsDir() {
			return blocked("the selected Apple macOS SDK directory is missing; repair the selected developer tools", nil)
		}
		evidence["sdk"] = sdk
	}
	fingerprint, err := macosDigest(evidence)
	return macosCheck{Status: engine.Satisfied, Fingerprint: fingerprint}, err
}

func macosDeveloperPath(data []byte) (string, error) {
	path := strings.TrimSuffix(string(data), "\n")
	if len(path) > 4096 || path == string(filepath.Separator) || !macosCleanPath(path) {
		return "", errors.New("Apple developer tools returned an invalid absolute path")
	}
	return path, nil
}

func macosSystemFile(path string, byteLimit int64) (string, bool, error) {
	snapshot, err := platform.SnapshotTreeBounded(path, byteLimit, 1)
	if err != nil {
		return "", false, err
	}
	info, err := os.Stat(path)
	if snapshot.Kind != "file" || err != nil || info.Mode().Perm()&0111 == 0 {
		return "", false, nil
	}
	return snapshot.Hash, true, nil
}

func (d *MacPrerequisites) clipboard(ctx context.Context) (macosCheck, error) {
	if len(d.clipboardCommands) != 2 {
		return macosCheck{}, errors.New("macOS clipboard requires both OS commands")
	}
	evidence := map[string]string{}
	for _, path := range d.clipboardCommands {
		if err := ctx.Err(); err != nil {
			return macosCheck{}, err
		}
		hash, healthy, err := macosSystemFile(path, 8<<20)
		if err != nil {
			return macosCheck{}, err
		}
		if !healthy {
			return macosCheck{Status: engine.Blocked, Reason: "macOS pbcopy/pbpaste is missing or damaged; repair the operating-system component"}, nil
		}
		evidence[path] = hash
	}
	fingerprint, err := macosDigest(evidence)
	return macosCheck{Status: engine.Satisfied, Fingerprint: fingerprint,
		Notes: []string{"Not exercised: macOS graphical-session clipboard round-trip (clipboard contents were not read or changed)"}}, err
}

// Apply always fails: setup does not own the Apple developer tools or the
// clipboard commands.
func (*MacPrerequisites) Apply(context.Context, catalog.Resource, engine.Finding, engine.Records) error {
	return errors.New("Apple developer tools and clipboard commands are reused infrastructure; install missing CLT through its prerequisite action")
}

// Remove always fails: setup never removes an Apple component.
func (*MacPrerequisites) Remove(context.Context, catalog.Resource, engine.Finding, engine.Records) error {
	return errors.New("dotfiles cannot remove Apple's developer tools or clipboard commands")
}
