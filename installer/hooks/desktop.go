package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/builds"
	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/system"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

const desktopObsidianPortable = "tool.obsidian-portable"

var desktopResources = []string{"desktop.vscode", "desktop.aerospace", "integration.ghostty", "desktop.rose-pine", "desktop.windows-terminal",
	"desktop.openwhispr", "desktop.obsidian", "desktop.t3-code", "desktop.zen", "desktop.xremap"}

// DesktopResource reports that Desktop is the provider of the resource.
func DesktopResource(id string) bool { return slices.Contains(desktopResources, id) }

// DesktopApplications gives the macOS applications that Homebrew casks
// install. Application returns the verified bundle of a cask resource. Cask
// returns its reviewed recipe. system.Brew implements it.
type DesktopApplications interface {
	Application(ctx context.Context, r catalog.Resource, rec engine.Records) (string, error)
	Cask(ctx context.Context, r catalog.Resource) (system.CaskSpec, error)
}

// Desktop is the engine.Provider of the fixed application integrations: the
// Linux desktop entries and the xremap autostart entry, the Windows Start
// menu shortcuts, the macOS application links of the private builds, the
// Rose Pine extension link of VS Code, and on macOS the command links code
// and ghostty into the application bundles of their casks. Each one is a
// file or link target, so it keeps its first baseline and a removal restores
// it. OpenWhispr on macOS needs no file: it is satisfied while its cask
// application is present.
//
// StateDir is the installer state folder. Pins holds the effective archive
// pin of each resource of the host. On macOS, Catalog gives the cask
// resources, Casks is the Homebrew provider and CommandDirectory is the
// managed command folder that the shell profile puts on PATH. On Windows,
// PowerShell is Windows PowerShell and Runner runs the shortcut preparation.
type Desktop struct {
	Context          platform.Context
	Folders          platform.Folders
	StateDir         string
	Pins             map[string]catalog.ArchivePin
	Catalog          *catalog.Catalog
	Casks            DesktopApplications
	CommandDirectory string
	PowerShell       string
	Runner           platform.Runner
}

type desktopRecipe struct {
	Package, Name string
	Kind          string
	Path          string
	Link          string
	Contents      string
	Command, Icon string
	Cask          string
	Requires      []string
}

func (p *Desktop) writer() *targets.Writer { return &targets.Writer{StateDir: p.StateDir} }

func (p *Desktop) required(id, name string) (string, error) {
	pin, ok := p.Pins[id]
	if !ok {
		return "", fmt.Errorf("%s has no archive pin for this system", id)
	}
	path := builds.RequiredFilePath(p.StateDir, id, name, pin)
	if path == "" {
		return "", fmt.Errorf("the archive pin of %s does not declare the file %s", id, name)
	}
	return path, nil
}

func (p *Desktop) command(id, name string) (string, error) {
	pin, ok := p.Pins[id]
	if !ok {
		return "", fmt.Errorf("%s has no archive pin for this system", id)
	}
	path := builds.CommandPath(p.StateDir, id, name, pin)
	if path == "" {
		return "", fmt.Errorf("the archive pin of %s does not declare the command %s", id, name)
	}
	return path, nil
}

func (p *Desktop) recipe(id string) (desktopRecipe, error) {
	var r desktopRecipe
	switch id {
	case "desktop.vscode":
		r = desktopRecipe{Package: "tool.vscode", Name: "Visual Studio Code"}
	case "integration.ghostty":
		r = desktopRecipe{Package: "tool.ghostty", Name: "Ghostty"}
	case "desktop.aerospace":
		r = desktopRecipe{Package: "tool.aerospace", Name: "AeroSpace"}
	case "desktop.windows-terminal":
		r = desktopRecipe{Package: "tool.windows-terminal", Name: "Windows Terminal"}
	case "desktop.rose-pine":
		r = desktopRecipe{Package: "vscode.rose-pine", Name: "Rose Pine"}
	case "desktop.openwhispr":
		r = desktopRecipe{Package: "tool.openwhispr", Name: "OpenWhispr"}
	case "desktop.obsidian":
		r = desktopRecipe{Package: "tool.obsidian", Name: "Obsidian"}
		if p.Context.OS == "linux" {
			r.Package = desktopObsidianPortable
		}
	case "desktop.t3-code":
		r = desktopRecipe{Package: "tool.t3-code-app", Name: "T3 Code"}
	case "desktop.zen":
		r = desktopRecipe{Package: "tool.zen", Name: "Zen"}
	case "desktop.xremap":
		r = desktopRecipe{Package: "tool.xremap", Name: "Mac keyboard shortcuts"}
	default:
		return r, errors.New("unknown fixed desktop integration")
	}
	if !filepath.IsAbs(p.StateDir) || filepath.Clean(p.StateDir) != p.StateDir || !filepath.IsAbs(p.Folders.Home) {
		return r, errors.New("desktop integration needs an absolute state folder and the home folder")
	}
	if r.Package == "vscode.rose-pine" {
		entry, err := p.required(r.Package, "extension/package.json")
		if err != nil {
			return r, err
		}
		r.Kind, r.Link, r.Path = "link", filepath.Dir(entry), filepath.Join(p.Folders.Home, ".vscode", "extensions", "dotfiles.rose-pine")
		r.Requires = []string{entry}
		return r, nil
	}
	var err error
	switch p.Context.OS {
	case "darwin":
		return p.darwinRecipe(r)
	case "linux":
		return p.linuxRecipe(r)
	case "windows":
		switch r.Package {
		case "tool.vscode":
			r.Command, err = p.required(r.Package, "Code.exe")
		case "tool.windows-terminal":
			r.Command, err = p.required(r.Package, "WindowsTerminal.exe")
		default:
			return r, errors.New("desktop application is unavailable on Windows")
		}
		if err != nil {
			return r, err
		}
		if !filepath.IsAbs(p.Folders.Programs) {
			return r, errors.New("desktop integration needs the actual Programs known folder")
		}
		r.Kind, r.Icon, r.Path = "shortcut", r.Command, filepath.Join(p.Folders.Programs, "Dotfiles "+r.Name+".lnk")
		r.Requires = []string{r.Command}
		return r, nil
	}
	return r, errors.New("unsupported desktop platform")
}

func (p *Desktop) darwinRecipe(r desktopRecipe) (desktopRecipe, error) {
	switch r.Package {
	case "tool.vscode", "tool.ghostty":
		if !filepath.IsAbs(p.CommandDirectory) || filepath.Clean(p.CommandDirectory) != p.CommandDirectory {
			return r, errors.New("desktop integration needs the managed command folder")
		}
		r.Kind, r.Cask = "link", r.Package
		r.Path = filepath.Join(p.CommandDirectory, map[string]string{"tool.vscode": "code", "tool.ghostty": "ghostty"}[r.Package])
		return r, nil
	case "tool.openwhispr":
		r.Kind, r.Cask = "none", r.Package
		return r, nil
	case "tool.aerospace", "tool.obsidian":
		entry, err := p.required(r.Package, r.Name+".app/Contents/Info.plist")
		if err != nil {
			return r, err
		}
		r.Kind, r.Link, r.Path = "link", filepath.Dir(filepath.Dir(entry)), filepath.Join(p.Folders.Home, "Applications", r.Name+".app")
		r.Requires = []string{entry}
		return r, nil
	}
	return r, errors.New("desktop application is unavailable on macOS")
}

func (p *Desktop) linuxRecipe(r desktopRecipe) (desktopRecipe, error) {
	var err error
	if r.Package == "tool.xremap" {
		if r.Command, err = p.command(r.Package, "xremap"); err != nil {
			return r, err
		}
		folder := p.Folders.Config
		if folder == "" {
			folder = filepath.Join(p.Folders.Home, ".config")
		}
		r.Kind, r.Path, r.Requires = "file", filepath.Join(folder, "autostart", "dotfiles-mac-keyboard.desktop"), []string{r.Command}
		r.Contents, err = desktopXremapEntry(r.Name, r.Command, filepath.Join(folder, "xremap", "config.yml"))
		return r, err
	}
	switch r.Package {
	case "tool.ghostty":
		r.Command, err = p.command(r.Package, "ghostty")
		if err == nil {
			r.Icon, err = p.required(r.Package, "usr/share/icons/hicolor/256x256/apps/com.mitchellh.ghostty.png")
		}
	case "tool.openwhispr":
		r.Command, err = p.command(r.Package, "open-whispr")
		if err == nil {
			r.Icon, err = p.required(r.Package, "resources/src/assets/icon.png")
		}
	case desktopObsidianPortable:
		r.Command, err = p.required(r.Package, "obsidian")
		if err == nil {
			r.Icon, err = p.required(r.Package, "resources/icon.png")
		}
	case "tool.t3-code-app":
		r.Command, err = p.required(r.Package, "t3-code")
		if err == nil {
			r.Icon, err = p.required(r.Package, "usr/share/icons/hicolor/256x256/apps/t3code.png")
		}
	case "tool.zen":
		r.Command, err = p.command(r.Package, "zen")
		if err == nil {
			r.Icon, err = p.required(r.Package, "browser/chrome/icons/default/default128.png")
		}
	default:
		return r, errors.New("desktop application is unavailable on Linux")
	}
	if err != nil {
		return r, err
	}
	folder := p.Folders.Data
	if folder == "" {
		folder = filepath.Join(p.Folders.Home, ".local", "share")
	}
	r.Kind, r.Requires = "file", []string{r.Command, r.Icon}
	r.Path = filepath.Join(folder, "applications", "dotfiles-"+strings.TrimSuffix(strings.TrimPrefix(r.Package, "tool."), "-portable")+".desktop")
	class := map[string]string{"tool.ghostty": "com.mitchellh.ghostty", "tool.zen": "zen", desktopObsidianPortable: "md.obsidian.Obsidian", "tool.openwhispr": "open-whispr", "tool.t3-code-app": "com.t3tools.T3Code"}[r.Package]
	r.Contents, err = desktopEntry(r.Name, r.Command, r.Icon, class, r.Package == "tool.zen")
	if err != nil {
		return r, err
	}
	exec := "\nExec=" + desktopExecArgument(r.Command)
	switch r.Package {
	case desktopObsidianPortable:
		r.Contents = strings.Replace(r.Contents, exec+"\n", exec+" --no-sandbox %U\n", 1)
		r.Contents = strings.Replace(r.Contents, "Categories=Development;", "Categories=Office;\nMimeType=x-scheme-handler/obsidian;", 1)
	case "tool.t3-code-app":
		r.Contents = strings.Replace(r.Contents, exec+"\n", exec+" %U\n", 1)
		r.Contents = strings.Replace(r.Contents, "Categories=Development;", "Categories=Development;\nMimeType=x-scheme-handler/t3code;", 1)
	}
	return r, nil
}

func (p *Desktop) application(ctx context.Context, recipe desktopRecipe, rec engine.Records) (application, link string, err error) {
	if p.Catalog == nil || p.Casks == nil {
		return "", "", errors.New("desktop integration needs the Homebrew application provider")
	}
	resource, ok := p.Catalog.Resource(recipe.Cask)
	if !ok {
		return "", "", fmt.Errorf("the catalog has no resource %s", recipe.Cask)
	}
	application, err = p.Casks.Application(ctx, resource, rec)
	if err != nil {
		return "", "", err
	}
	if recipe.Kind == "none" {
		return application, "", nil
	}
	spec, err := p.Casks.Cask(ctx, resource)
	if err != nil {
		return "", "", err
	}
	link = spec.CommandTarget(application)
	if link == "" || filepath.Base(recipe.Path) != spec.Command {
		return "", "", fmt.Errorf("the cask of %s has no managed command for %s", recipe.Name, recipe.Path)
	}
	return application, link, nil
}

func (p *Desktop) unit(ctx context.Context, id string, recipe desktopRecipe, rec engine.Records) (unit *targets.Desired, waiting string, err error) {
	path := recipe.Path
	if path != "" {
		if path, err = platform.BindDestination(recipe.Path); err != nil {
			return nil, "", err
		}
	}
	switch {
	case recipe.Cask != "":
		_, link, err := p.application(ctx, recipe, rec)
		if err != nil {
			return nil, recipe.Name + " is not installed: " + err.Error(), nil
		}
		if recipe.Kind == "none" {
			return nil, "", nil
		}
		return &targets.Desired{Resource: id, Kind: "link", Path: path, Link: link}, "", nil
	case recipe.Kind == "link":
		return &targets.Desired{Resource: id, Kind: "link", Path: path, Link: recipe.Link}, "", nil
	case recipe.Kind == "shortcut":
		data, err := platform.ReadDocument(p.shortcutInput(recipe))
		if errors.Is(err, os.ErrNotExist) {
			return nil, "setup prepares the Start menu shortcut of " + recipe.Name + " during apply", nil
		}
		if err != nil {
			return nil, "", err
		}
		return &targets.Desired{Resource: id, Kind: "file", Path: path, Content: data}, "", nil
	}
	return &targets.Desired{Resource: id, Kind: "file", Path: path, Content: []byte(recipe.Contents)}, "", nil
}

func desktopRecorded(st state.State, id string) []string {
	keys := []string{}
	for _, key := range slices.Sorted(maps.Keys(st.Targets)) {
		if st.Targets[key].Resource == id {
			keys = append(keys, key)
		}
	}
	return keys
}

func desktopReleaseOrder(st state.State, id string) []string {
	keys := desktopRecorded(st, id)
	slices.SortStableFunc(keys, func(a, b string) int { return len(st.Targets[a].Made) - len(st.Targets[b].Made) })
	return keys
}

func desktopToken(rows [][]string) string {
	encoded, _ := json.Marshal(rows)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

type desktopInspection struct {
	rows       [][]string
	unreadable []string
	changed    []string
	work       int
	owned      int
}

func (i *desktopInspection) record(w *targets.Writer, rec engine.Records, key string) error {
	live, err := w.InspectRecord(key, rec)
	if err != nil {
		return err
	}
	record := rec.State().Targets[key]
	i.rows = append(i.rows, []string{key, record.Applied, record.Next, fmt.Sprint(record.Release), live.State, live.Fingerprint})
	switch {
	case live.State == targets.StateUnreadable:
		i.unreadable = append(i.unreadable, live.Detail)
	case live.State == targets.StateChanged && record.Applied != "" && record.Kind != "font":
		i.changed = append(i.changed, key)
	}
	return nil
}

func (i *desktopInspection) unit(w *targets.Writer, rec engine.Records, d targets.Desired) error {
	live, err := w.Inspect(d, rec)
	if err != nil {
		return err
	}
	record, has := rec.State().Targets[targets.Key(d.Path, d.Slot)]
	switch {
	case live.State == targets.StateUnreadable:
		if !has {
			i.unreadable = append(i.unreadable, live.Detail)
		}
	case live.State == targets.StateChanged && live.Fingerprint != live.Want:
		if !has || record.Applied == "" {
			i.changed = append(i.changed, d.Path)
		}
	case live.Satisfied && record.Kind == d.Kind:
	default:
		i.work++
	}
	return nil
}

func (i *desktopInspection) released() engine.Finding {
	finding := engine.Finding{Status: engine.Satisfied, Token: desktopToken(i.rows)}
	switch {
	case len(i.rows) == 1:
		finding.Status = engine.Absent
	case len(i.unreadable) != 0:
		finding.Status, finding.Reason = engine.Unknown, strings.Join(i.unreadable, "; ")
	case len(i.changed) != 0:
		finding.Status, finding.Preserved = engine.Changed, i.changed
		finding.Reason = "content changed after setup wrote it: " + strings.Join(i.changed, ", ")
	}
	return finding
}

func (i *desktopInspection) wanted(waiting, unhealthy string) engine.Finding {
	finding := engine.Finding{Status: engine.Satisfied, Token: desktopToken(i.rows)}
	switch {
	case len(i.unreadable) != 0:
		finding.Status, finding.Reason = engine.Unknown, strings.Join(i.unreadable, "; ")
	case len(i.changed) != 0:
		finding.Status, finding.Preserved = engine.Changed, i.changed
		finding.Reason = "existing content differs from the selected content: " + strings.Join(i.changed, ", ")
	case waiting != "" && i.owned == 0:
		finding.Status, finding.Reason = engine.Missing, waiting
	case waiting != "":
		finding.Status, finding.Reason = engine.Unhealthy, waiting
	case i.work != 0 && i.owned == 0:
		finding.Status = engine.Missing
	case i.work != 0:
		finding.Status, finding.Reason = engine.Unhealthy, unhealthy
	}
	return finding
}

func desktopInspect(w *targets.Writer, rec engine.Records, id string, want engine.Want, units []targets.Desired, paths []string, known ...string) (*desktopInspection, error) {
	st := rec.State()
	i := &desktopInspection{rows: [][]string{{fmt.Sprint(want.Present)}}}
	recorded := desktopRecorded(st, id)
	for _, key := range recorded {
		if err := i.record(w, rec, key); err != nil {
			return nil, err
		}
		if st.Targets[key].Applied != "" {
			i.owned++
		}
	}
	if !want.Present {
		return i, nil
	}
	for _, path := range paths {
		if slices.Contains(recorded, path) {
			continue
		}
		live, err := platform.SnapshotTree(path)
		if err != nil {
			i.unreadable = append(i.unreadable, path+": "+err.Error())
			continue
		}
		i.rows = append(i.rows, []string{path, live.Kind, live.Hash})
	}
	for _, d := range units {
		if err := i.unit(w, rec, d); err != nil {
			return nil, err
		}
	}
	for _, key := range recorded {
		if !slices.ContainsFunc(units, func(d targets.Desired) bool { return targets.Key(d.Path, d.Slot) == key }) && len(units) != 0 && !slices.Contains(known, key) {
			i.work++
		}
	}
	return i, nil
}

func desktopChoice(approved engine.Finding) targets.Choice {
	if approved.Replace {
		return targets.Replace
	}
	return targets.Preserve
}

func desktopConverge(ctx context.Context, w *targets.Writer, rec engine.Records, id string, approved engine.Finding, units []targets.Desired) error {
	for _, d := range units {
		if err := w.Converge(ctx, d, rec, desktopChoice(approved)); err != nil {
			return err
		}
	}
	for _, key := range desktopReleaseOrder(rec.State(), id) {
		if !slices.ContainsFunc(units, func(d targets.Desired) bool { return targets.Key(d.Path, d.Slot) == key }) {
			if err := w.Release(ctx, key, rec, desktopChoice(approved)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Inspect implements engine.Provider. It only reads and prepares no file.
// For a wanted resource it reports Missing while the application, the
// private build or the prepared shortcut is not there, Changed when the
// destination holds content that setup did not write, and Unhealthy when a
// target differs from its record. For a released resource it reports Absent
// without a record and Changed for a recorded target with other content.
func (p *Desktop) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	if err := ctx.Err(); err != nil {
		return engine.Finding{}, err
	}
	w := p.writer()
	if !want.Present {
		i, err := desktopInspect(w, rec, r.ID, want, nil, nil)
		if err != nil {
			return engine.Finding{}, err
		}
		return i.released(), nil
	}
	recipe, err := p.recipe(r.ID)
	if err != nil {
		return engine.Finding{}, err
	}
	unit, waiting, err := p.unit(ctx, r.ID, recipe, rec)
	if err != nil {
		return engine.Finding{}, err
	}
	units, paths := []targets.Desired{}, []string{}
	if unit != nil {
		units = append(units, *unit)
	}
	if recipe.Path != "" {
		path, err := platform.BindDestination(recipe.Path)
		if err != nil {
			return engine.Finding{}, err
		}
		paths = append(paths, path)
	}
	i, err := desktopInspect(w, rec, r.ID, want, units, paths)
	if err != nil {
		return engine.Finding{}, err
	}
	if recipe.Kind == "none" && waiting == "" && len(desktopRecorded(rec.State(), r.ID)) != 0 {
		i.work++
	}
	return i.wanted(waiting, "a desktop integration target differs from its record"), nil
}

// Apply implements engine.Provider. It checks the files that the entry
// names, prepares the Windows shortcut when it has none, converges the
// target, and then releases each recorded target that the recipe no longer
// names. With approved.Replace it saves changed content first.
func (p *Desktop) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	recipe, err := p.recipe(r.ID)
	if err != nil {
		return err
	}
	for _, path := range recipe.Requires {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("desktop integration requires its verified private application: %w", err)
		}
	}
	if recipe.Kind == "shortcut" {
		if err := p.prepareShortcut(ctx, recipe); err != nil {
			return err
		}
	}
	unit, waiting, err := p.unit(ctx, r.ID, recipe, rec)
	if err != nil {
		return err
	}
	if waiting != "" {
		return errors.New(waiting)
	}
	units := []targets.Desired{}
	if unit != nil {
		units = append(units, *unit)
	}
	return desktopConverge(ctx, p.writer(), rec, r.ID, approved, units)
}

// Remove implements engine.Provider. It releases each recorded target of the
// resource through the write order and deletes the prepared shortcut.
func (p *Desktop) Remove(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := desktopConverge(ctx, p.writer(), rec, r.ID, approved, nil); err != nil {
		return err
	}
	if recipe, err := p.recipe(r.ID); err == nil && recipe.Kind == "shortcut" {
		if err := os.Remove(p.shortcutInput(recipe)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
