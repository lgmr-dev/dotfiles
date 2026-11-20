package catalog_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/tools"
)

func previousResources(t *testing.T) []catalog.Resource {
	t.Helper()
	var document struct {
		Schema    int                `json:"schema"`
		Resources []catalog.Resource `json:"resources"`
	}
	decodePrevious(t, "resources.json", &document)
	kept := []catalog.Resource{}
	for _, r := range document.Resources {
		if r.Action != "sentinel-retired" {
			kept = append(kept, r)
		}
	}
	if len(document.Resources)-len(kept) != 5 {
		t.Fatal("the v2 catalog does not hold the five retired Sentinel resources")
	}
	return kept
}

func previousClosure(t *testing.T, resources []catalog.Resource, roots []string, ctx platform.Context) []string {
	t.Helper()
	byID := map[string]catalog.Resource{}
	for _, r := range resources {
		byID[r.ID] = r
	}
	result, done := []string{}, map[string]bool{}
	var visit func(string)
	visit = func(id string) {
		r, ok := byID[id]
		if !ok || !r.Available(ctx) {
			t.Fatalf("v2 closure reaches %s, which is unknown or unavailable on %s", id, ctx.OS)
		}
		if done[id] {
			return
		}
		done[id] = true
		dependencies := append(append([]string{}, r.Requires...), r.PlatformRequires[ctx.OS]...)
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			visit(dependency)
		}
		result = append(result, id)
	}
	ordered := append([]string{}, roots...)
	sort.Strings(ordered)
	for _, root := range ordered {
		visit(root)
	}
	return result
}

func TestEachResourceEqualsItsV2Row(t *testing.T) {
	c := released(t)
	want := previousResources(t)
	if len(c.Resources) != len(want) {
		t.Fatalf("the tool folders hold %d resources, v2 holds %d", len(c.Resources), len(want))
	}
	for _, expected := range want {
		actual, found := c.Resource(expected.ID)
		actual.Order = 0
		if !found || !reflect.DeepEqual(actual, expected) {
			t.Errorf("%s differs from v2:\n got %+v\nwant %+v", expected.ID, actual, expected)
		}
	}
}

func TestClosureEqualsV2OnEachPlatformAndSelection(t *testing.T) {
	c := released(t)
	want := previousResources(t)
	for _, ctx := range contexts {
		all := []string{}
		for _, r := range want {
			if !r.Capability || !r.Available(ctx) {
				continue
			}
			all = append(all, r.ID)
			actual, err := c.Closure([]string{r.ID}, ctx)
			if expected := previousClosure(t, want, []string{r.ID}, ctx); err != nil || !slices.Equal(actual, expected) {
				t.Errorf("%s on %s/%s: %v\n got %v\nwant %v", r.ID, ctx.OS, ctx.Arch, err, actual, expected)
			}
		}
		actual, err := c.Closure(all, ctx)
		if expected := previousClosure(t, want, all, ctx); err != nil || !slices.Equal(actual, expected) {
			t.Errorf("all capabilities on %s/%s: %v\n got %v\nwant %v", ctx.OS, ctx.Arch, err, actual, expected)
		}
		if len(all) < 30 {
			t.Fatalf("only %d capabilities on %s", len(all), ctx.OS)
		}
	}
}

func TestMenuKeepsTheV2CapabilityOrder(t *testing.T) {
	c := released(t)
	for _, ctx := range contexts {
		want := []string{}
		for _, r := range previousResources(t) {
			if r.Capability && r.Available(ctx) {
				want = append(want, r.ID)
			}
		}
		got := []string{}
		for _, r := range c.Menu(ctx) {
			got = append(got, r.ID)
		}
		if !slices.Equal(got, want) {
			t.Errorf("menu on %s/%s:\n got %v\nwant %v", ctx.OS, ctx.Arch, got, want)
		}
	}
}

func TestTargetsEqualTheV2Manifest(t *testing.T) {
	c := released(t)
	var document struct {
		Schema  int              `json:"schema"`
		Targets []catalog.Target `json:"targets"`
	}
	decodePrevious(t, "config-targets.json", &document)
	group := func(targets []catalog.Target) map[string][]catalog.Target {
		result := map[string][]catalog.Target{}
		for _, target := range targets {
			result[target.Resource] = append(result[target.Resource], target)
		}
		return result
	}
	if len(c.Targets) != len(document.Targets) || !reflect.DeepEqual(group(c.Targets), group(document.Targets)) {
		t.Fatalf("targets differ from v2:\n got %+v\nwant %+v", c.Targets, document.Targets)
	}
}

func TestArchivePinsEqualTheV2File(t *testing.T) {
	c := released(t)
	var document struct {
		Schema    int                                   `json:"schema"`
		Resources map[string]map[string]json.RawMessage `json:"resources"`
	}
	decodePrevious(t, "archive-pins.json", &document)
	if len(c.Pins.Archives) != len(document.Resources) {
		t.Fatalf("the tool folders hold %d archive families, v2 holds %d", len(c.Pins.Archives), len(document.Resources))
	}
	for family, platforms := range document.Resources {
		if len(c.Pins.Archives[family]) != len(platforms) {
			t.Errorf("%s has %d platform pins, v2 has %d", family, len(c.Pins.Archives[family]), len(platforms))
		}
		for key, raw := range platforms {
			var expected catalog.ArchivePin
			if err := json.Unmarshal(raw, &expected); err != nil {
				t.Fatal(family, key, err)
			}
			want, err := json.Marshal(expected)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(c.Pins.Archives[family][key])
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("%s on %s differs from v2: %v", family, key, err)
			}
		}
	}
}

func TestOtherPinFilesAndShellFragmentsKeepTheirV2Bytes(t *testing.T) {
	c := released(t)
	for file, resource := range map[string]string{
		"linux-code-pins.json":    "tool.vscode",
		"linux-package-pins.json": "tool.tailscale",
		"buildtools-pin.json":     "tool.compiler",
		"vcredist-pin.json":       "tool.vcredist",
		"windows-app-pins.json":   "app.t3-code-windows",
	} {
		matches, err := fs.Glob(tools.FS, "*/"+file)
		if err != nil || len(matches) != 1 {
			t.Fatal(file, matches, err)
		}
		data, err := fs.ReadFile(tools.FS, matches[0])
		if err != nil || !bytes.Equal(data, previous(t, file)) {
			t.Errorf("%s differs from the v2 file: %v", matches[0], err)
		}
		tool, err := fs.ReadFile(tools.FS, path.Join(path.Dir(matches[0]), "tool.json"))
		if err != nil || !bytes.Contains(tool, []byte(`"id": "`+resource+`"`)) {
			t.Errorf("%s is not in the folder of %s: %v", file, resource, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join("..", "shells"))
	if err != nil || len(entries) != len(c.Shell) {
		t.Fatalf("v2 has %d shell sources, the tool folders list %d: %v", len(entries), len(c.Shell), err)
	}
	for _, fragment := range c.Shell {
		data, err := fs.ReadFile(tools.FS, fragment.Path())
		if err != nil || !bytes.Equal(data, previous(t, path.Join("shells", path.Base(fragment.Source)))) {
			t.Errorf("%s differs from the v2 source: %v", fragment.Path(), err)
		}
	}
}

func TestTypedPinsMatchTheV2Files(t *testing.T) {
	c := released(t)
	var code struct {
		Schema int                               `json:"schema"`
		Pins   map[string]catalog.LinuxCodePin   `json:"pins"`
		Modes  map[string]catalog.LinuxCodeModes `json:"modes"`
	}
	decodePrevious(t, "linux-code-pins.json", &code)
	for arch, want := range code.Pins {
		if got, err := c.Pins.LinuxCodePin(arch); err != nil || got != want || c.Pins.LinuxCodeDeclaredModes(want.Version) != code.Modes[want.Version] {
			t.Error("Linux Code pin differs", arch, got, err)
		}
	}
	if _, err := c.Pins.LinuxCodePin("riscv64"); err == nil || !reflect.DeepEqual(c.Pins.LinuxCodeModes, code.Modes) {
		t.Error("Linux Code pins accept an unknown architecture or lose a declared mode")
	}
	if c.Pins.LinuxCodeDeclaredModes("1.0.0-1") != (catalog.LinuxCodeModes{Sandbox: "755"}) {
		t.Error("an undeclared Code version must use the plain sandbox mode")
	}
	var packages struct {
		Schema    int                                           `json:"schema"`
		Resources map[string]map[string]catalog.LinuxPackagePin `json:"resources"`
	}
	decodePrevious(t, "linux-package-pins.json", &packages)
	for _, arch := range []string{"amd64", "arm64"} {
		if got, ok := c.Pins.LinuxPackage("tool.tailscale", arch); !ok || got != packages.Resources["tool.tailscale"][arch] {
			t.Error("Tailscale pin differs", arch, got)
		}
	}
	var buildTools catalog.WindowsBuildToolsPin
	decodePrevious(t, "buildtools-pin.json", &buildTools)
	if c.Pins.BuildTools == nil || !reflect.DeepEqual(*c.Pins.BuildTools, buildTools) {
		t.Error("Build Tools pin differs")
	}
	var runtime catalog.WindowsVendorPin
	decodePrevious(t, "vcredist-pin.json", &runtime)
	if c.Pins.VCRedist == nil || *c.Pins.VCRedist != runtime {
		t.Error("VC++ runtime pin differs")
	}
	apps := map[string]catalog.WindowsAppPin{}
	decodePrevious(t, "windows-app-pins.json", &apps)
	if got, ok := c.Pins.WindowsApp("app.t3-code-windows"); !ok || got != apps["app.t3-code-windows"] || got.Publisher != "T3 Tools Inc" || len(c.Pins.WindowsApps) != 1 {
		t.Error("Windows application pin differs", got)
	}
}
