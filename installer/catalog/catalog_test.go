package catalog_test

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/platform"
)

func TestLoadRejectsAmbiguousOrUnprovisionablePackageBindings(t *testing.T) {
	for _, kind := range []string{"valid", "duplicate", "unknown-provider", "retired-provider", "empty-package", "unavailable", "no-action"} {
		t.Run(kind, func(t *testing.T) {
			resource := catalog.Resource{ID: "package", Name: "Package", Action: "node", Bindings: map[string]catalog.Binding{"linux": {Provider: "archive", Package: "node"}}}
			resources := []catalog.Resource{resource}
			switch kind {
			case "duplicate":
				duplicate := resource
				duplicate.ID = "alias"
				resources = append(resources, duplicate)
			case "unknown-provider":
				resource.Bindings["linux"] = catalog.Binding{Provider: "native", Package: "node"}
			case "retired-provider":
				resource.Bindings["linux"] = catalog.Binding{Provider: "nix-home", Package: "node"}
			case "empty-package":
				resource.Bindings["linux"] = catalog.Binding{Provider: "archive"}
			case "unavailable":
				resources[0].Platforms = []string{"windows"}
			case "no-action":
				resources[0].Action = ""
			}
			if _, err := catalog.Load(folders(t, one(resources...))); (err == nil) != (kind == "valid") {
				t.Fatal("unexpected binding validation", err)
			}
		})
	}
}

func TestLoadAcceptsOnlyTheReviewedNativeApplications(t *testing.T) {
	homebrew := catalog.Resource{ID: "infra.homebrew", Name: "Homebrew", Platforms: []string{"darwin"}}
	for _, test := range []struct {
		id, os, provider, name string
		requires               []string
		valid                  bool
	}{
		{"app.zen", "darwin", "homebrew-cask", "zen", []string{"infra.homebrew"}, true},
		{"app.t3-code", "darwin", "homebrew-cask", "t3-code", []string{"infra.homebrew"}, true},
		{"tool.ghostty", "darwin", "homebrew-cask", "ghostty", []string{"infra.homebrew"}, true},
		{"tool.openwhispr", "darwin", "homebrew-cask", "openwhispr", []string{"infra.homebrew"}, true},
		{"tool.ghostty", "darwin", "homebrew-cask", "ghostty@tip", []string{"infra.homebrew"}, false},
		{"tool.ghostty", "darwin", "homebrew-cask", "ghostty", nil, false},
		{"app.other", "darwin", "homebrew-cask", "zen", []string{"infra.homebrew"}, false},
		{"app.zen", "darwin", "homebrew-cask", "t3-code", []string{"infra.homebrew"}, false},
		{"app.zen", "darwin", "homebrew-cask", "zen", nil, false},
		{"tool.tmux", "darwin", "homebrew-formula", "tmux", []string{"infra.homebrew"}, true},
		{"tool.tmux", "darwin", "homebrew-formula", "homebrew/core/tmux", []string{"infra.homebrew"}, true},
		{"tool.tmux", "darwin", "homebrew-formula", "core/tmux", []string{"infra.homebrew"}, false},
		{"tool.tmux", "linux", "homebrew-formula", "tmux", []string{"infra.homebrew"}, false},
		{"tool.zsh", "linux", "apt", "zsh", nil, true},
		{"tool.zsh", "linux", "apt", "zsh --purge", nil, false},
		{"tool.zsh", "darwin", "apt", "zsh", nil, false},
		{"app.other", "windows", "windows-app", "t3-code", nil, false},
		{"app.t3-code-windows", "windows", "windows-app", "zen", nil, false},
		{"app.t3-code-windows", "linux", "windows-app", "t3-code", nil, false},
	} {
		resource := catalog.Resource{ID: test.id, Name: "Application", Action: "package", Scope: "user", Platforms: []string{test.os}, PlatformRequires: map[string][]string{}, Bindings: map[string]catalog.Binding{test.os: {Provider: test.provider, Package: test.name}}}
		if test.requires != nil {
			resource.PlatformRequires["darwin"] = test.requires
		}
		if _, err := catalog.Load(folders(t, one(homebrew, resource))); (err == nil) != test.valid {
			t.Error("unexpected application binding validation", test, err)
		}
	}
	files := releasedFiles(t)
	edit(t, files, "t3-code-app/tool.json", func(document map[string]any) {
		for _, resource := range document["resources"].([]any) {
			if entry := resource.(map[string]any); entry["id"] == "app.t3-code-windows" {
				entry["scope"] = "machine"
			}
		}
	})
	if _, err := catalog.Load(files); err == nil {
		t.Fatal("a machine-scope Windows application was accepted")
	}
}

func TestLoadValidatesCategoriesAndMenuPositions(t *testing.T) {
	for _, test := range []struct {
		name, first, second, hidden string
		firstOrder, secondOrder     int
		hiddenOrder                 int
		valid                       bool
	}{
		{name: "flat catalog", firstOrder: 1, secondOrder: 2, valid: true},
		{name: "complete grouping", first: "Tools", second: "Other tools", firstOrder: 1, secondOrder: 2, valid: true},
		{name: "missing category", first: "Tools", firstOrder: 1, secondOrder: 2},
		{name: "hidden category", first: "Tools", second: "Tools", hidden: "Tools", firstOrder: 1, secondOrder: 2},
		{name: "padded category", first: " Tools", second: "Tools", firstOrder: 1, secondOrder: 2},
		{name: "blank category", first: " ", second: "Tools", firstOrder: 1, secondOrder: 2},
		{name: "control character", first: "Shell\x1b[2Jessentials", second: "Tools", firstOrder: 1, secondOrder: 2},
		{name: "missing order", first: "Tools", second: "Tools", firstOrder: 1},
		{name: "repeated order", first: "Tools", second: "Tools", firstOrder: 2, secondOrder: 2},
		{name: "negative order", first: "Tools", second: "Tools", firstOrder: 1, secondOrder: -1},
		{name: "hidden order", first: "Tools", second: "Tools", firstOrder: 1, secondOrder: 2, hiddenOrder: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := catalog.Load(folders(t, one(
				catalog.Resource{ID: "first", Name: "First", Capability: true, Category: test.first, Order: test.firstOrder},
				catalog.Resource{ID: "second", Name: "Second", Capability: true, Category: test.second, Order: test.secondOrder},
				catalog.Resource{ID: "hidden", Name: "Hidden", Action: "archive", Category: test.hidden, Order: test.hiddenOrder},
			)))
			if (err == nil) != test.valid {
				t.Fatal("unexpected category validation", err)
			}
		})
	}
}

func TestMenuFollowsOrderAcrossFoldersAndPlatforms(t *testing.T) {
	c, err := catalog.Load(folders(t, map[string]catalog.Tool{
		"alpha": {Resources: []catalog.Resource{{ID: "last", Name: "Last", Capability: true, Order: 3}}},
		"beta":  {Resources: []catalog.Resource{{ID: "first", Name: "First", Capability: true, Order: 1}, {ID: "hidden", Name: "Hidden"}}},
		"gamma": {Resources: []catalog.Resource{{ID: "mac", Name: "Mac", Capability: true, Order: 2, Platforms: []string{"darwin"}}}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	ids := func(ctx platform.Context) string {
		names := []string{}
		for _, r := range c.Menu(ctx) {
			names = append(names, r.ID)
		}
		return strings.Join(names, " ")
	}
	if ids(darwin) != "first mac last" || ids(linux) != "first last" {
		t.Fatal("menu ignores order or platform", ids(darwin), ids(linux))
	}
}

func TestLoadRejectsInvalidResourcesAndGraphs(t *testing.T) {
	valid := catalog.Resource{ID: "tool", Name: "Tool"}
	for name, resources := range map[string][]catalog.Resource{
		"invalid ID":                            {{ID: "Tool", Name: "Tool"}},
		"no name":                               {{ID: "tool"}},
		"duplicate":                             {valid, valid},
		"invalid scope":                         {{ID: "tool", Name: "Tool", Scope: "system"}},
		"invalid platform":                      {{ID: "tool", Name: "Tool", Platforms: []string{"plan9"}}},
		"invalid requirement OS":                {{ID: "tool", Name: "Tool", PlatformRequires: map[string][]string{"plan9": {"tool"}}}},
		"unknown requirement":                   {{ID: "tool", Name: "Tool", Requires: []string{"missing"}}},
		"unknown requirement on another system": {{ID: "tool", Name: "Tool", Platforms: []string{"linux"}, PlatformRequires: map[string][]string{"windows": {"missing"}}}},
		"unavailable requirement":               {{ID: "tool", Name: "Tool", Requires: []string{"mac"}}, {ID: "mac", Name: "Mac", Platforms: []string{"darwin"}}},
		"cycle":                                 {{ID: "a", Name: "A", Requires: []string{"b"}}, {ID: "b", Name: "B", Requires: []string{"a"}}},
		"empty architecture list":               {{ID: "tool", Name: "Tool", PlatformArchitectures: map[string][]string{"linux": {}}}},
		"unsupported architecture":              {{ID: "tool", Name: "Tool", PlatformArchitectures: map[string][]string{"windows": {"arm64"}}}},
		"architecture of an absent system":      {{ID: "tool", Name: "Tool", Platforms: []string{"darwin"}, PlatformArchitectures: map[string][]string{"linux": {"amd64"}}}},
		"replan without retention":              {{ID: "tool", Name: "Tool", Action: "homebrew", ReplanAfter: true}},
		"replan with a binding":                 {{ID: "tool", Name: "Tool", Action: "homebrew", Retain: true, ReplanAfter: true, Bindings: map[string]catalog.Binding{"linux": {Provider: "apt", Package: "tool"}}}},
	} {
		if _, err := catalog.Load(folders(t, one(resources...))); err == nil {
			t.Error("invalid catalog accepted:", name)
		}
	}
	if _, err := catalog.Load(folders(t, one(valid, catalog.Resource{ID: "infra", Name: "Infra", Action: "homebrew", Retain: true, ReplanAfter: true}))); err != nil {
		t.Fatal("valid catalog rejected", err)
	}
}

func TestLoadRejectsInvalidToolFolders(t *testing.T) {
	tool := func(id string) catalog.Tool {
		return catalog.Tool{Resources: []catalog.Resource{{ID: id, Name: "Tool"}}}
	}
	for name, files := range map[string]fstest.MapFS{
		"resource in two folders":   folders(t, map[string]catalog.Tool{"alpha": tool("tool"), "beta": tool("tool")}),
		"folder without a resource": folders(t, map[string]catalog.Tool{"alpha": {}}),
		"invalid folder name":       folders(t, map[string]catalog.Tool{"Alpha_Tool": tool("tool")}),
		"folder without tool.json":  {"alpha/pins.json": {Data: []byte(`{}`)}},
		"unknown schema":            {"alpha/tool.json": {Data: []byte(`{"schema":2,"resources":[{"id":"tool","name":"Tool"}]}`)}},
		"unknown field":             {"alpha/tool.json": {Data: []byte(`{"schema":1,"resources":[{"id":"tool","name":"Tool","owner":"x"}]}`)}},
		"second document":           {"alpha/tool.json": {Data: []byte(`{"schema":1,"resources":[{"id":"tool","name":"Tool"}]}{}`)}},
		"unknown file": {
			"alpha/tool.json":  {Data: []byte(`{"schema":1,"resources":[{"id":"tool","name":"Tool"}]}`)},
			"alpha/notes.json": {Data: []byte(`{}`)},
		},
		"shell fragment without a file": {
			"alpha/tool.json": {Data: []byte(`{"schema":1,"resources":[{"id":"tool","name":"Tool"}],"shell":[{"resource":"tool","shell":"zsh","source":"shell/init.zsh"}]}`)},
		},
		"shell fragment outside the shell folder": {
			"alpha/tool.json": {Data: []byte(`{"schema":1,"resources":[{"id":"tool","name":"Tool"}],"shell":[{"resource":"tool","shell":"zsh","source":"init.zsh"}]}`)},
			"alpha/init.zsh":  {Data: []byte(":")},
		},
		"shell fragment of an unknown shell": {
			"alpha/tool.json":       {Data: []byte(`{"schema":1,"resources":[{"id":"tool","name":"Tool"}],"shell":[{"resource":"tool","shell":"fish","source":"shell/init.fish"}]}`)},
			"alpha/shell/init.fish": {Data: []byte(":")},
		},
		"shell fragment of another folder": {
			"alpha/tool.json":     {Data: []byte(`{"schema":1,"resources":[{"id":"tool","name":"Tool"}]}`)},
			"beta/tool.json":      {Data: []byte(`{"schema":1,"resources":[{"id":"other","name":"Other"}],"shell":[{"resource":"tool","shell":"zsh","source":"shell/init.zsh"}]}`)},
			"beta/shell/init.zsh": {Data: []byte(":")},
		},
	} {
		if _, err := catalog.Load(files); err == nil {
			t.Error("invalid tool folders accepted:", name)
		}
	}
	c, err := catalog.Load(fstest.MapFS{
		"alpha/tool.json":      {Data: []byte(`{"schema":1,"resources":[{"id":"tool","name":"Tool"}],"shell":[{"resource":"tool","shell":"zsh","source":"shell/init.zsh"}]}`)},
		"alpha/shell/init.zsh": {Data: []byte(":")},
		"README.md":            {Data: []byte("ignored")},
	})
	if err != nil || len(c.Shell) != 1 || c.Shell[0].Path() != "alpha/shell/init.zsh" {
		t.Fatal("valid shell fragment rejected or without its folder", err)
	}
}

func TestClosureIsStableAndRejectsUnknownOrUnavailableRoots(t *testing.T) {
	c := released(t)
	first, err := c.Closure([]string{"neovim", "zsh", "git"}, linux)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Closure([]string{"git", "zsh", "neovim", "zsh"}, linux)
	if err != nil || !slices.Equal(first, second) {
		t.Fatal("closure depends on the selection order or duplicates", err)
	}
	position := map[string]int{}
	for index, id := range first {
		position[id] = index
	}
	for _, id := range first {
		r, _ := c.Resource(id)
		for _, dependency := range r.Dependencies(linux) {
			if position[dependency] >= position[id] {
				t.Fatalf("%s comes before its requirement %s", id, dependency)
			}
		}
	}
	for _, root := range []string{"missing", "powershell", "integration.sentinel", "skills.sentinel-claude"} {
		if _, err := c.Closure([]string{root}, linux); err == nil {
			t.Error("closure accepted", root)
		}
	}
}

func TestReleasedCatalogKeepsItsPlatformRoutes(t *testing.T) {
	c := released(t)
	for _, ctx := range contexts {
		choices := []string{}
		for _, r := range c.Menu(ctx) {
			choices = append(choices, r.ID)
			if !r.Capability || r.Category == "" {
				t.Fatal("menu holds a hidden resource", r.ID)
			}
		}
		if slices.Contains(choices, "infra.homebrew") || slices.Contains(choices, "tool.clangd") {
			t.Fatal("prerequisite exposed as a user choice", ctx)
		}
		if slices.Contains(choices, "tmux") != (ctx.OS != "windows") || slices.Contains(choices, "kiro-cli") != (ctx.OS != "windows") || slices.Contains(choices, "powershell") != (ctx.OS == "windows") {
			t.Fatal("unexpected choices", ctx, choices)
		}
		for root, tool := range map[string]string{"ripgrep": "tool.rg", "fd": "tool.fd"} {
			closure, err := c.Closure([]string{root}, ctx)
			if err != nil || !slices.Contains(closure, tool) || !slices.Contains(closure, "integration.shells") {
				t.Fatal("standalone search tool lacks its executable or shell path", root, closure, err)
			}
			for _, id := range closure {
				if r, _ := c.Resource(id); r.Capability && id != root {
					t.Fatal("standalone tool selects another capability", root, id)
				}
			}
		}
		neovim, err := c.Closure([]string{"neovim"}, ctx)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(neovim, "tool.clangd") != (ctx.OS == "linux") || slices.Contains(neovim, "infra.apple-clt") != (ctx.OS == "darwin") || slices.Contains(neovim, "tool.clipboard-wayland") != (ctx.OS == "linux") {
			t.Fatal("Neovim prerequisites changed", ctx, neovim)
		}
		herdr, err := c.Closure([]string{"herdr"}, ctx)
		if err != nil || !slices.Contains(herdr, "tool.herdr") || slices.Contains(herdr, "tool.tmux") {
			t.Fatal("Herdr route changed", ctx, herdr, err)
		}
	}
	for id, name := range map[string]string{"tool.git": "git", "tool.tmux": "tmux", "tool.zsh": "zsh", "tool.make": "make", "tool.compiler": "build-essential", "tool.clangd": "clangd", "tool.clipboard": "xclip", "tool.clipboard-wayland": "wl-clipboard"} {
		if r, ok := c.Resource(id); !ok || r.Capability || r.Bindings["linux"] != (catalog.Binding{Provider: "apt", Package: name}) {
			t.Errorf("%s lacks its hidden APT route: %+v", id, r)
		}
	}
	homebrew, _ := c.Resource("infra.homebrew")
	clt, _ := c.Resource("infra.apple-clt")
	if !homebrew.ReplanAfter || !clt.ReplanAfter {
		t.Fatal("the bootstrap resources lost their replan points")
	}
}
