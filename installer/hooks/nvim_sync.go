package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/builds"
	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

// NvimResource is the resource of the NvimSync provider. NvimExecutable is
// the resource whose private build holds the nvim command.
const (
	NvimResource   = "nvim.sync"
	NvimExecutable = "tool.nvim"
)

const nvimRecipe = 1

var nvimEnvironment = []string{"HOME", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "PATH", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "TEMP", "TMP",
	"INCLUDE", "LIB", "LIBPATH", "VCINSTALLDIR", "VCToolsInstallDir", "WindowsSdkDir", "WindowsSDKVersion", "RUSTUP_HOME"}

// NvimSync is the engine.Provider of NvimResource: the plugins, the
// Tree-sitter parsers and the Mason language tools of Neovim. The runtime is
// a private build at <StateDir>/tools/nvim.sync/<version>-<digest>-<serial>
// with the layout and the manifest of the builds package. Its digest covers
// the snapshot of the nvim folder of the checkout, the plugin lock file, the
// Mason registry snapshot and the exact build path of each private tool that
// the sync uses. Three headless Neovim phases fill a new build at its final
// path in an isolated environment: lazy.nvim restores the locked plugins,
// nvim-treesitter installs the parsers and Mason installs the language tools
// from nvim/mason-registry. A clean headless session then checks the
// candidate from its final path. After the check, setup writes the manifest,
// publishes the current link of the build and points the public runtime link
// of Neovim, <RuntimeDirectory>/nvim.sync/current, at the build. A failed
// build has no manifest, and the next run uses a new folder.
//
// Checkout is the physical path of the repository. RuntimeDirectory is the
// result of NvimRuntimeDirectory. Pins holds the effective archive pin of
// each resource of the host. Tools names the resources with a private build
// that the sync uses, in dependency order, with NvimExecutable among them:
// their command folders start PATH. Directories are the other command
// folders, in order. Environment adds variables to each Neovim command, for
// example RUSTUP_HOME. Runner runs the three phases. Query runs the
// verification; nil uses NvimQuery. CompilerEnvironment, when set, returns
// the Microsoft C++ variables at the time a phase runs, because the same run
// can install the compiler first.
type NvimSync struct {
	Context             platform.Context
	Checkout            string
	StateDir            string
	RuntimeDirectory    string
	Pins                map[string]catalog.ArchivePin
	Tools               []string
	Directories         []string
	Environment         []string
	Runner              platform.Runner
	Query               func(context.Context, platform.Command) ([]byte, error)
	CompilerEnvironment func(context.Context) (map[string]string, error)
}

func (p *NvimSync) writer() *targets.Writer { return &targets.Writer{StateDir: p.StateDir} }
func (p *NvimSync) directory() string       { return builds.ResourceDir(p.StateDir, NvimResource) }
func (p *NvimSync) current() string         { return builds.CurrentDir(p.StateDir, NvimResource) }
func (p *NvimSync) public() string {
	return filepath.Join(p.RuntimeDirectory, NvimResource, "current")
}

func (p *NvimSync) validate(r catalog.Resource) error {
	if r.ID != NvimResource {
		return errors.New("invalid Neovim resource identity")
	}
	for _, path := range []string{p.Checkout, p.StateDir, p.RuntimeDirectory} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return errors.New("Neovim sync requires explicit absolute canonical paths")
		}
	}
	for _, env := range p.Environment {
		key, value, ok := strings.Cut(env, "=")
		if !ok || !slices.Contains(nvimEnvironment, key) || strings.ContainsRune(env, 0) {
			return errors.New("unexpected Neovim sync environment control")
		}
		if key == "RUSTUP_HOME" && (!filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsAny(value, "\r\n")) {
			return errors.New("Neovim sync requires an explicit absolute managed Rust home")
		}
	}
	for _, directory := range p.Directories {
		if !filepath.IsAbs(directory) || strings.ContainsAny(directory, "\x00\r\n"+string(os.PathListSeparator)) {
			return errors.New("Neovim command directories must be absolute native paths")
		}
	}
	for _, path := range []string{p.current(), p.public()} {
		if err := builds.Bound(path); err != nil {
			return errors.Join(errors.New("Neovim runtime parent was redirected"), err)
		}
	}
	return nil
}

type nvimSource struct {
	Recipe                 int
	Source, Lock, Registry string
}

func (p *NvimSync) source() (nvimSource, error) {
	source := nvimSource{Recipe: nvimRecipe}
	for _, part := range []struct {
		path, kind string
		hash       *string
	}{{"", "directory", &source.Source}, {"lazy-lock.json", "file", &source.Lock}, {"mason-registry", "directory", &source.Registry}} {
		snapshot, err := platform.SnapshotTree(filepath.Join(p.Checkout, "nvim", part.path))
		if err != nil || snapshot.Kind != part.kind {
			return source, errors.Join(errors.New("Neovim configuration source is unavailable: "+filepath.Join(p.Checkout, "nvim", part.path)), err)
		}
		*part.hash = snapshot.Hash
	}
	return source, nil
}

type nvimTools struct {
	uses       []string
	path       string
	executable string
	waiting    string
	outdated   string
}

func (p *NvimSync) stale(build string, seen map[string]bool) bool {
	if seen[build] {
		return false
	}
	seen[build] = true
	id := filepath.Base(filepath.Dir(build))
	pin, ok := p.Pins[id]
	if !ok {
		return false
	}
	data, err := platform.ReadDocument(build + ".manifest.json")
	var manifest builds.Manifest
	if err != nil || platform.Decode(data, &manifest) != nil {
		return true
	}
	if manifest.Digest != builds.Digest(pin, manifest.Uses) {
		return true
	}
	for _, used := range manifest.Uses {
		current, err := builds.BuildPath(p.StateDir, filepath.Base(filepath.Dir(used)))
		if err != nil || current != used || p.stale(used, seen) {
			return true
		}
	}
	return false
}

func (p *NvimSync) tools() (nvimTools, error) {
	var t nvimTools
	if !slices.Contains(p.Tools, NvimExecutable) {
		return t, errors.New("Neovim sync requires the private build of " + NvimExecutable)
	}
	directories, seen := []string{}, map[string]bool{}
	for _, id := range p.Tools {
		pin, ok := p.Pins[id]
		if !ok {
			return t, fmt.Errorf("%s has no archive pin for this system", id)
		}
		build, err := builds.BuildPath(p.StateDir, id)
		if err != nil {
			if t.waiting == "" {
				t.waiting = id + " has no usable private build"
			}
			continue
		}
		t.uses = append(t.uses, build)
		if t.outdated == "" && p.stale(build, seen) {
			t.outdated = "the build of " + id + " changes with its reviewed pin"
		}
		for _, directory := range pin.BinDirs {
			directories = append(directories, filepath.Join(build, filepath.FromSlash(directory)))
		}
		if id == NvimExecutable {
			name, ok := pin.Commands["nvim"]
			if !ok {
				return t, errors.New("the archive pin of " + NvimExecutable + " does not declare the command nvim")
			}
			t.executable = filepath.Join(build, filepath.FromSlash(name))
		}
	}
	unique := []string{}
	for _, directory := range append(directories, p.Directories...) {
		if strings.ContainsAny(directory, "\x00\r\n"+string(os.PathListSeparator)) {
			return t, errors.New("Neovim command directories must be absolute native paths")
		}
		if !slices.Contains(unique, directory) {
			unique = append(unique, directory)
		}
	}
	t.path = strings.Join(unique, string(os.PathListSeparator))
	return t, nil
}

func nvimDigest(source nvimSource, uses []string) string {
	data, err := json.Marshal(struct {
		Source nvimSource
		Uses   []string
	}{source, append([]string{}, uses...)})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func nvimNullDevice() string { return strings.ToLower(os.DevNull) }

func (p *NvimSync) command(t nvimTools, payload string, step int, check bool) platform.Command {
	rustupHome := filepath.Join(payload, ".rustup")
	for _, value := range p.Environment {
		if explicit, ok := strings.CutPrefix(value, "RUSTUP_HOME="); ok {
			rustupHome = explicit
		}
	}
	args := []string{"--headless", "-i", "NONE", "--cmd", "lua vim.opt.rtp:prepend(vim.env.DOTFILES_NVIM_CONFIG)"}
	env := append(slices.Clone(p.Environment), "PATH="+t.path, "DOTFILES_NVIM_CONFIG="+filepath.Join(p.Checkout, "nvim"), "DOTFILES_NVIM_RUNTIME="+payload,
		"DOTFILES_NVIM_LOCKFILE="+filepath.Join(payload, ".lazy-lock-0.json"), "DOTFILES_TREESITTER_SYNC_INSTALL=", "NVIM_APPNAME=nvim", "NVIM_LOG_FILE="+os.DevNull,
		"VIMINIT=", "EXINIT=", "NVIM=", "HOME="+filepath.Join(payload, ".home"), "USERPROFILE="+filepath.Join(payload, ".home"), "LOCALAPPDATA="+filepath.Join(payload, ".local"),
		"APPDATA="+filepath.Join(payload, ".roaming"), "XDG_CONFIG_HOME="+p.Checkout, "XDG_DATA_HOME="+filepath.Join(payload, ".data"), "XDG_STATE_HOME="+filepath.Join(payload, ".state"),
		"XDG_CACHE_HOME="+filepath.Join(payload, ".cache"), "NPM_CONFIG_CACHE="+filepath.Join(payload, ".cache", "npm"), "NPM_CONFIG_USERCONFIG="+filepath.Join(payload, ".home", ".npmrc"),
		"CARGO_HOME="+filepath.Join(payload, ".cargo"), "RUSTUP_HOME="+rustupHome, "PIP_CONFIG_FILE="+nvimNullDevice(), "PIP_CACHE_DIR="+filepath.Join(payload, ".cache", "pip"),
		"PIP_TARGET=", "PIP_PREFIX=", "PYTHONDONTWRITEBYTECODE=1")
	if check {
		return platform.Command{Path: t.executable, Env: append(env, "DOTFILES_NVIM_SYNC="),
			Args: append(args, "--clean", "+lua require('util.sync_check').run()", "+cquit 1")}
	}
	env = append(env, "DOTFILES_NVIM_SYNC=1")
	args = append(args, "-u", filepath.Join(p.Checkout, "nvim", "init.lua"))
	switch step {
	case 0:
		args = append(args, "+Lazy! restore", "+lua if vim.v.errmsg ~= '' then vim.cmd('cquit 1') end", "+qa")
	case 1:
		env = append(env, "DOTFILES_TREESITTER_SYNC_INSTALL=1")
		args = append(args, "+lua require('lazy').load({ plugins = { 'nvim-treesitter' } })", "+lua if vim.v.errmsg ~= '' then vim.cmd('cquit 1') end", "+qa")
	case 2:
		args = append(args, "+lua require('util.mason_tools').run_checked('MasonToolsInstallSync')", "+cquit 1")
	}
	return platform.Command{Path: t.executable, Args: args, Env: env}
}

func (p *NvimSync) check(ctx context.Context, t nvimTools, payload string) error {
	query := p.Query
	if query == nil {
		query = NvimQuery
	}
	_, err := query(ctx, p.command(t, payload, 0, true))
	return err
}

type nvimObservation struct {
	Source   nvimSource
	Record   state.Record
	Recorded bool
	Link     string
	LinkErr  string
	Public   targets.Live
	Manifest *builds.Manifest
	Tree     platform.Snapshot
	Problem  string
}

func (o nvimObservation) token() string {
	data, err := json.Marshal(o)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (p *NvimSync) observe(rec engine.Records, source nvimSource, deep bool) (nvimObservation, error) {
	o := nvimObservation{Source: source}
	o.Record, o.Recorded = rec.State().Targets[p.current()]
	link, err := builds.LinkTarget(p.current())
	if err != nil {
		o.LinkErr = err.Error()
		return o, nil
	}
	o.Link = link
	if _, has := rec.State().Targets[p.public()]; has {
		if o.Public, err = p.writer().InspectRecord(p.public(), rec); err != nil {
			return o, err
		}
	} else if snapshot, err := platform.SnapshotTree(p.public()); err != nil {
		o.Public = targets.Live{State: targets.StateUnreadable, Detail: err.Error()}
	} else if snapshot.Kind != "absent" {
		o.Public = targets.Live{State: targets.StateChanged, Fingerprint: snapshot.Kind + ":" + snapshot.Hash}
	}
	if !deep || link == "" || !o.Recorded || link != o.Record.Applied && link != o.Record.Next {
		return o, nil
	}
	manifest, err := builds.ReadManifest(p.StateDir, NvimResource, link)
	if err != nil {
		o.Problem = "the manifest of the current Neovim runtime is not valid: " + err.Error()
		return o, nil
	}
	o.Manifest = &manifest
	if o.Tree, err = builds.SnapshotBuild(link); err != nil {
		o.Problem = "setup cannot read the current Neovim runtime: " + err.Error()
	} else if o.Tree != manifest.Tree {
		o.Problem = "the current Neovim runtime differs from its manifest"
	}
	return o, nil
}

// Inspect implements engine.Provider. It reads the current link, its record,
// the manifest and the tree of the current runtime and the public runtime
// link, and it runs the read-only verification of a runtime that matches the
// configuration. It reports Missing without a runtime, Blocked for a link
// without a record or an unavailable configuration source, Changed for a link
// that setup did not publish, Unhealthy for a runtime that differs from its
// manifest, from the configuration or from the builds of its tools, or that
// fails its verification, and Outdated when the reviewed pin of a tool that
// it uses changes the build of that tool.
func (p *NvimSync) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	if err := ctx.Err(); err != nil {
		return engine.Finding{}, err
	}
	if err := p.validate(r); err != nil {
		return engine.Finding{}, err
	}
	source, sourceErr := p.source()
	if !want.Present {
		source, sourceErr = nvimSource{}, nil
	}
	o, err := p.observe(rec, source, true)
	if err != nil {
		return engine.Finding{}, err
	}
	f := engine.Finding{Token: o.token()}
	if o.Manifest != nil {
		f.Version = o.Manifest.Version
	}
	_, publicRecorded := rec.State().Targets[p.public()]
	switch {
	case sourceErr != nil:
		f.Status, f.Reason = engine.Blocked, sourceErr.Error()
	case o.LinkErr != "":
		f.Status, f.Reason = engine.Blocked, o.LinkErr
	case o.Public.State == targets.StateUnreadable:
		f.Status, f.Reason = engine.Unknown, o.Public.Detail
	case o.Public.State == targets.StateChanged && (want.Present && o.Public.Fingerprint != p.publicWant(o.Link) || !want.Present && publicRecorded):
		f.Status, f.Reason, f.Preserved = engine.Changed, "the public runtime link of Neovim holds content that setup did not write", []string{p.public()}
	case o.Link == "":
		f.Status = engine.Missing
		if !want.Present {
			f.Status = engine.Satisfied
			if !o.Recorded && !publicRecorded {
				f.Status = engine.Absent
			}
		}
		if o.Recorded {
			f.Reason = "the current link of the Neovim runtime is missing"
		}
	case !o.Recorded:
		f.Status, f.Reason = engine.Blocked, "refusing to acquire a pre-existing Neovim runtime: an unrecorded link exists at "+p.current()+"; move it away, then run setup again"
	case o.Link != o.Record.Applied && o.Link != o.Record.Next:
		f.Status, f.Reason, f.Preserved = engine.Changed, "the current link points at "+o.Link+", which setup did not publish", []string{p.current()}
	case o.Problem != "":
		f.Status, f.Reason = engine.Unhealthy, o.Problem
		if !want.Present {
			f.Status, f.Preserved = engine.Changed, []string{o.Link}
		}
	case !want.Present:
		f.Status = engine.Satisfied
	case o.Link != o.Record.Applied || o.Record.Next != "" || o.Record.Release:
		f.Status, f.Reason = engine.Unhealthy, "the publication of the Neovim runtime is not complete"
	default:
		t, err := p.tools()
		if err != nil {
			return engine.Finding{}, err
		}
		public, err := p.writer().Inspect(p.publicLink(o.Link), rec)
		if err != nil {
			return engine.Finding{}, err
		}
		switch {
		case t.waiting != "":
			f.Status, f.Reason = engine.Unhealthy, t.waiting
		case o.Manifest.Digest != nvimDigest(source, t.uses):
			f.Status, f.Reason = engine.Unhealthy, "Neovim runtime does not match the current configuration"
		case !public.Satisfied:
			f.Status, f.Reason = engine.Unhealthy, "the public runtime link of Neovim does not point at the current runtime"
		case t.outdated != "":
			f.Status, f.Reason = engine.Outdated, t.outdated
		default:
			f.Status = engine.Satisfied
			if err := p.check(ctx, t, o.Link); err != nil {
				f.Status, f.Reason = engine.Unhealthy, err.Error()
			}
		}
	}
	return f, nil
}

func (p *NvimSync) publicLink(build string) targets.Desired {
	return targets.Desired{Resource: NvimResource, Kind: "link", Path: p.public(), Link: build}
}

func (p *NvimSync) publicWant(build string) string {
	want, err := targets.FileKind("link").Fingerprint(p.publicLink(build))
	if err != nil {
		return ""
	}
	return want
}

func (p *NvimSync) reusable(ctx context.Context, t nvimTools, version, digest string) (string, error) {
	candidates, err := builds.Candidates(p.StateDir, NvimResource, version, digest)
	if err != nil {
		return "", err
	}
	for _, build := range slices.Backward(candidates) {
		manifest, err := builds.ReadManifest(p.StateDir, NvimResource, build)
		if err != nil || manifest.Digest != digest {
			continue
		}
		if tree, err := builds.SnapshotBuild(build); err != nil || tree != manifest.Tree {
			continue
		}
		if p.check(ctx, t, build) == nil {
			return build, nil
		}
	}
	return "", nil
}

func (p *NvimSync) build(ctx context.Context, t nvimTools, source nvimSource, version, digest string) (string, error) {
	if p.Runner == nil {
		return "", errors.New("Neovim sync requires a command runner")
	}
	build, err := builds.NextBuild(p.StateDir, NvimResource, version, digest)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(build, 0700); err != nil {
		return "", fmt.Errorf("cannot reserve fresh Neovim runtime: %w", err)
	}
	unfinished := func(err error) (string, error) {
		return "", fmt.Errorf("%w; the unfinished runtime stays at %s and the next run uses a new folder", err, build)
	}
	if err := builds.Bound(filepath.Join(build, "lazy")); err != nil {
		return unfinished(errors.Join(errors.New("Neovim stage was redirected"), err))
	}
	platform.Detail(ctx, "Building the Neovim runtime at %s", build)
	lockfile := filepath.Join(build, ".lazy-lock-0.json")
	if err := platform.CopyTree(filepath.Join(p.Checkout, "nvim", "lazy-lock.json"), lockfile); err != nil {
		return unfinished(err)
	}
	if locked, err := platform.SnapshotTree(lockfile); err != nil || locked.Kind != "file" || locked.Hash != source.Lock {
		return unfinished(errors.Join(errors.New("Neovim private lock differs from the reviewed source"), err))
	}
	for step, name := range []string{"restoring locked plugins with lazy.nvim", "installing Tree-sitter parsers", "installing Mason language tools"} {
		platform.Detail(ctx, "Neovim %d/3: %s", step+1, name)
		command, err := nvimWithCompilerEnvironment(ctx, p.CompilerEnvironment, p.command(t, build, step, false))
		if err != nil {
			return unfinished(err)
		}
		if _, err := p.Runner.Run(ctx, command); err != nil {
			return unfinished(err)
		}
	}
	platform.Detail(ctx, "Neovim: verifying the synchronized runtime in a clean headless session")
	if err := p.check(ctx, t, build); err != nil {
		return unfinished(err)
	}
	if after, err := p.source(); err != nil || after != source {
		return unfinished(errors.Join(errors.New("Neovim source changed during synchronization"), err))
	}
	tree, err := builds.SnapshotBuild(build)
	if err != nil {
		return unfinished(err)
	}
	if err := builds.WriteManifest(build, builds.Manifest{Schema: 1, Resource: NvimResource, Version: version, Digest: digest, Tree: tree, Uses: t.uses}); err != nil {
		return "", err
	}
	return build, nil
}

// Apply implements engine.Provider. It reuses a runtime that has a manifest,
// the digest of the configuration and of the tool builds, an unchanged tree
// and a passing verification. Otherwise it builds a new runtime at its final
// path through the three phases and verifies it there. Then it publishes the
// current link of the build and the public runtime link of Neovim. A link
// that setup did not publish stays unless the approved finding permits its
// replacement.
func (p *NvimSync) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.validate(r); err != nil {
		return err
	}
	source, err := p.source()
	if err != nil {
		return err
	}
	o, err := p.observe(rec, source, false)
	if err != nil {
		return err
	}
	if o.LinkErr != "" {
		return errors.New(o.LinkErr)
	}
	if o.Link != "" && !o.Recorded {
		return errors.New("refusing to acquire a pre-existing Neovim runtime")
	}
	if o.Link != "" && o.Link != o.Record.Applied && o.Link != o.Record.Next && !approved.Replace {
		return fmt.Errorf("the link %s is not the one that setup published; it stays", p.current())
	}
	t, err := p.tools()
	if err != nil {
		return err
	}
	if t.waiting != "" {
		return errors.New(t.waiting + "; apply that resource first")
	}
	for _, folder := range []string{p.StateDir, builds.ToolsDir(p.StateDir), p.directory()} {
		if err := platform.PrepareDirectory(folder); err != nil {
			return err
		}
	}
	version, digest := source.Source[:12], nvimDigest(source, t.uses)
	candidate, err := p.reusable(ctx, t, version, digest)
	if err != nil {
		return err
	}
	if candidate == "" {
		if candidate, err = p.build(ctx, t, source, version, digest); err != nil {
			return err
		}
	}
	if err := builds.Publish(p.StateDir, NvimResource, candidate, rec); err != nil {
		return err
	}
	return p.writer().Converge(ctx, p.publicLink(candidate), rec, desktopChoice(approved))
}

// Remove implements engine.Provider. It releases the public runtime link of
// Neovim through the write order, then the current link of the build and its
// record. A link that setup did not publish stays. The runtime folders stay
// for the sweep of the builds package.
func (p *NvimSync) Remove(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.validate(r); err != nil {
		return err
	}
	if err := p.writer().Release(ctx, p.public(), rec, desktopChoice(approved)); err != nil {
		return err
	}
	return builds.Unpublish(p.StateDir, NvimResource, rec)
}
