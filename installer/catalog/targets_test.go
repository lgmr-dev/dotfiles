package catalog_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/platform"
)

func TestTargetsAllowExplicitDirectoryCopies(t *testing.T) {
	tool := catalog.Tool{
		Resources: []catalog.Resource{{ID: "config.skills", Name: "Skills", Action: "config", Scope: "user"}},
		Targets:   []catalog.Target{{Resource: "config.skills", Source: "skills/test", Folder: "home", Path: ".agents/skills/test", Directory: true, Mode: "copy"}},
	}
	if _, err := catalog.Load(folders(t, map[string]catalog.Tool{"skills": tool})); err != nil {
		t.Fatal("portable skills require real directory copies", err)
	}
}

func TestTargetsCoverEachConfigResourceWithCanonicalSources(t *testing.T) {
	c := released(t)
	for _, target := range c.Targets {
		if strings.Contains(target.Source, "chezmoi") || strings.HasPrefix(target.Source, "home/") {
			t.Fatal("native configuration uses a mirrored compatibility source", target)
		}
		info, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(target.Source)))
		if err != nil || info.IsDir() != target.Directory {
			t.Fatal("missing or mistyped canonical configuration source", target, err)
		}
	}
}

func TestWindowsTargetsUseIndependentlyRedirectedKnownFolders(t *testing.T) {
	c := released(t)
	root := t.TempDir()
	known := platform.Folders{Home: filepath.Join(root, "profile"), Config: filepath.Join(root, "configuration"), LocalAppData: filepath.Join(root, "redirected local"), AppData: filepath.Join(root, "redirected roaming"), Documents: filepath.Join(root, "redirected documents")}
	for resource, destination := range map[string]string{
		"config.nvim":       filepath.Join(known.LocalAppData, "nvim"),
		"config.herdr":      filepath.Join(known.AppData, "herdr", "config.toml"),
		"config.lazygit":    filepath.Join(known.LocalAppData, "lazygit", "config.yml"),
		"config.lazydocker": filepath.Join(known.AppData, "lazydocker", "config.yml"),
	} {
		targets, err := c.TargetsFor(resource, windows, known, filepath.Join(root, "source"))
		if err != nil || len(targets) != 1 {
			t.Fatal(resource, targets, err)
		}
		wantMode := "copy"
		if resource == "config.nvim" {
			wantMode = "junction"
		}
		if targets[0].Destination != destination || targets[0].Mode != wantMode || !strings.HasPrefix(targets[0].Source, filepath.Join(root, "source")) {
			t.Fatal("known folder was guessed or Windows requires symlink privileges", targets[0])
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("target resolution created directories", entries, err)
	}
}

func TestTargetResolutionRejectsMissingFoldersOverlapAndWrongResources(t *testing.T) {
	c := released(t)
	root := t.TempDir()
	checkout := filepath.Join(root, "source")
	for _, known := range []platform.Folders{{}, {Home: root, Config: checkout}, {Home: root, Config: "relative"}} {
		if _, err := c.TargetsFor("config.nvim", linux, known, checkout); err == nil {
			t.Fatal("invalid configuration destination was accepted", known)
		}
	}
	known := platform.Folders{Home: filepath.Join(root, "home"), Config: filepath.Join(root, "home", ".config")}
	targets, err := c.TargetsFor("config.ghostty", linux, known, checkout)
	if err != nil || len(targets) == 0 || targets[0].Mode != "link" {
		t.Fatal("Linux configuration must resolve to live links", targets, err)
	}
	for _, test := range []struct{ resource, checkout string }{
		{"tool.node", checkout}, {"missing", checkout}, {"config.aerospace", checkout}, {"config.ghostty", "relative"}, {"config.ghostty", checkout + string(filepath.Separator)},
	} {
		if _, err := c.TargetsFor(test.resource, linux, known, test.checkout); err == nil {
			t.Fatal("invalid target resolution accepted", test)
		}
	}
	for _, ctx := range contexts {
		for _, r := range c.Resources {
			if r.Action != "config" || !r.Available(ctx) {
				continue
			}
			all := platform.Folders{Home: filepath.Join(root, "h"), Config: filepath.Join(root, "c"), LocalAppData: filepath.Join(root, "l"), AppData: filepath.Join(root, "a"), Documents: filepath.Join(root, "d")}
			if targets, err := c.TargetsFor(r.ID, ctx, all, checkout); err != nil || len(targets) == 0 {
				t.Fatal("configuration resource has no target", r.ID, ctx, err)
			}
		}
	}
}

func TestLoadRejectsEscapeDuplicateAndUnownedTargets(t *testing.T) {
	first := func(document map[string]any) map[string]any { return at(document, "targets", 0) }
	for name, change := range map[string]func(map[string]any){
		"source escape":   func(d map[string]any) { first(d)["source"] = "../outside" },
		"path escape":     func(d map[string]any) { first(d)["path"] = "../outside" },
		"drive path":      func(d map[string]any) { first(d)["path"] = "C:/outside" },
		"unclean path":    func(d map[string]any) { first(d)["path"] = "tmux/../../outside" },
		"duplicate":       func(d map[string]any) { d["targets"] = append(d["targets"].([]any), first(d)) },
		"not a config":    func(d map[string]any) { first(d)["resource"] = "tool.tmux" },
		"another folder":  func(d map[string]any) { first(d)["resource"] = "config.nvim" },
		"unknown":         func(d map[string]any) { first(d)["resource"] = "config.missing" },
		"guessed folder":  func(d map[string]any) { first(d)["folder"] = "guessed_documents" },
		"windows folder":  func(d map[string]any) { first(d)["folder"] = "app_data" },
		"absent platform": func(d map[string]any) { first(d)["platforms"] = []any{"windows"} },
		"junction on POSIX": func(d map[string]any) {
			first(d)["mode"], first(d)["directory"] = "junction", true
		},
		"unknown mode": func(d map[string]any) { first(d)["mode"] = "hardlink" },
		"no target":    func(d map[string]any) { delete(d, "targets") },
		"unknown key":  func(d map[string]any) { first(d)["owner"] = "x" },
	} {
		files := releasedFiles(t)
		edit(t, files, "tmux/tool.json", change)
		if _, err := catalog.Load(files); err == nil {
			t.Error("unsafe target accepted:", name)
		}
	}
	c := released(t)
	if !slices.ContainsFunc(c.Targets, func(target catalog.Target) bool { return target.Resource == "config.tmux" }) {
		t.Fatal("the fixture folder has no target")
	}
}
