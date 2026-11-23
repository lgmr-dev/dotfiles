package targets_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/targets"
)

type configFixture struct {
	*env
	provider    *targets.ConfigProvider
	resource    catalog.Resource
	source      string
	destination string
}

func configCatalog(t *testing.T, items ...catalog.Target) *catalog.Catalog {
	t.Helper()
	tool := catalog.Tool{Schema: 1, Resources: []catalog.Resource{{ID: "config.test", Name: "Config", Action: "config", Scope: "user"}}, Targets: items}
	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := catalog.Load(fstest.MapFS{"fixture/tool.json": &fstest.MapFile{Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func newConfig(t *testing.T, mode string, directory bool) *configFixture {
	t.Helper()
	if mode == "link" && runtime.GOOS == "windows" {
		t.Skip("Windows uses copies and junctions")
	}
	e := newEnv(t)
	item := catalog.Target{Resource: "config.test", Source: "app/config.json", Folder: "config", Path: "app/nested/config.json", Directory: directory}
	if mode == "copy" {
		item.Mode = "copy"
	}
	checkout := filepath.Join(e.home, "checkout")
	f := &configFixture{env: e, source: filepath.Join(checkout, "app", "config.json"), destination: filepath.Join(e.root, ".config", "app", "nested", "config.json")}
	if directory {
		write(t, filepath.Join(f.source, "init.lua"), "canonical configuration", 0644)
	} else {
		write(t, f.source, "canonical configuration", 0644)
	}
	f.provider = &targets.ConfigProvider{
		Catalog:  configCatalog(t, item),
		Context:  platform.Context{OS: runtime.GOOS, Arch: runtime.GOARCH},
		Folders:  platform.Folders{Home: e.root, Config: filepath.Join(e.root, ".config")},
		Checkout: checkout,
		Writer:   e.writer,
	}
	f.resource, _ = f.provider.Catalog.Resource("config.test")
	return f
}

func (f *configFixture) inspect(present bool) engine.Finding {
	f.t.Helper()
	finding, err := f.provider.Inspect(f.t.Context(), f.resource, engine.Want{Present: present}, f)
	if err != nil {
		f.t.Fatal(err)
	}
	return finding
}

func (f *configFixture) expect(present bool, status engine.Status) engine.Finding {
	f.t.Helper()
	finding := f.inspect(present)
	if finding.Status != status {
		f.t.Fatalf("status %s, want %s: %+v", finding.Status, status, finding)
	}
	return finding
}

func (f *configFixture) apply(replace bool) error {
	finding := f.inspect(true)
	finding.Replace = replace
	return f.provider.Apply(f.t.Context(), f.resource, finding, f)
}

func (f *configFixture) remove(replace bool) error {
	finding := f.inspect(false)
	finding.Replace = replace
	return f.provider.Remove(f.t.Context(), f.resource, finding, f)
}

func (f *configFixture) content() string {
	f.t.Helper()
	path := f.destination
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, "init.lua")
	}
	return read(f.t, path)
}

func noError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationInstallUpdateRemoveReinstall(t *testing.T) {
	for _, mode := range []string{"copy", "link"} {
		for _, directory := range []bool{false, true} {
			t.Run(mode, func(t *testing.T) {
				f := newConfig(t, mode, directory)
				f.expect(false, engine.Absent)
				first := f.expect(true, engine.Missing)
				if _, err := os.Lstat(f.root); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("inspection created the target", err)
				}
				if again := f.inspect(true); again.Token != first.Token || first.Token == "" {
					t.Fatal("an unchanged target changed its token")
				}
				for cycle := 0; cycle < 2; cycle++ {
					noError(t, f.apply(false))
					satisfied := f.expect(true, engine.Satisfied)
					if satisfied.Token == first.Token || f.content() != "canonical configuration" {
						t.Fatal("the token did not follow the observation", f.content())
					}
					edited := f.source
					if directory {
						edited = filepath.Join(f.source, "init.lua")
					}
					write(t, edited, "updated configuration", 0644)
					if mode == "link" {
						f.expect(true, engine.Satisfied)
					} else {
						f.expect(true, engine.Unhealthy)
						noError(t, f.apply(false))
						f.expect(true, engine.Satisfied)
					}
					if f.content() != "updated configuration" {
						t.Fatal(f.content())
					}
					f.expect(false, engine.Satisfied)
					noError(t, f.remove(false))
					f.expect(false, engine.Absent)
					if _, err := os.Lstat(f.root); !errors.Is(err, os.ErrNotExist) || len(f.State().Targets) != 0 || len(f.baselines()) != 0 {
						t.Fatal("removal left the configuration, its folders, a record or storage", err)
					}
					write(t, edited, "canonical configuration", 0644)
				}
			})
		}
	}
}

func TestConfigurationRemovalKeepsAFolderThatThePersonUsed(t *testing.T) {
	f := newConfig(t, "copy", false)
	noError(t, f.apply(false))
	personal := filepath.Join(filepath.Dir(f.destination), "personal.txt")
	write(t, personal, "keep", 0600)
	noError(t, f.remove(false))
	if _, err := os.Lstat(f.destination); !errors.Is(err, os.ErrNotExist) || read(t, personal) != "keep" {
		t.Fatal("removal changed a folder that a person used", err)
	}
}

func TestConfigurationKeepsExistingContentAndItsFirstBaselineAcrossUpdates(t *testing.T) {
	for _, mode := range []string{"copy", "link"} {
		t.Run(mode, func(t *testing.T) {
			f := newConfig(t, mode, false)
			write(t, f.destination, "personal configuration", 0640)
			origin := describe(t, f.root)
			changed := f.expect(true, engine.Changed)
			if !slices.Equal(changed.Preserved, []string{f.destination}) || !strings.Contains(changed.Reason, f.destination) {
				t.Fatal("the changed target is not named", changed)
			}
			if err := f.apply(false); err == nil || !strings.Contains(err.Error(), "configuration changed after approval") || describe(t, f.root) != origin {
				t.Fatal("existing content was replaced without approval", err)
			}
			noError(t, f.apply(true))
			f.expect(true, engine.Satisfied)
			baseline := f.State().Targets[f.destination].Before
			if !strings.HasPrefix(baseline, "backup:"+filepath.Join(f.writer.StateDir, "baselines")) {
				t.Fatal("the first baseline must be under the state folder", baseline)
			}
			write(t, f.source, "updated configuration", 0644)
			noError(t, f.apply(false))
			if f.content() != "updated configuration" || f.State().Targets[f.destination].Before != baseline {
				t.Fatal("an update changed the first baseline")
			}
			noError(t, f.remove(false))
			if describe(t, f.root) != origin || len(f.baselines()) != 0 {
				t.Fatal("removal did not restore the first baseline")
			}
		})
	}
}

func TestConfigurationChangedTargetIsPreservedOnUpdateAndRemoval(t *testing.T) {
	f := newConfig(t, "copy", false)
	noError(t, f.apply(false))
	write(t, f.destination, "edited by the owner", 0600)
	for _, present := range []bool{true, false} {
		finding := f.expect(present, engine.Changed)
		if !slices.Equal(finding.Preserved, []string{f.destination}) {
			t.Fatal(finding)
		}
	}
	if err := f.remove(false); !errors.Is(err, targets.ErrChanged) || read(t, f.destination) != "edited by the owner" || len(f.State().Targets) != 1 {
		t.Fatal("removal changed an edited target", err)
	}
	noError(t, f.apply(true))
	saved := f.baselines()
	if len(saved) != 1 || read(t, saved[0]) != "edited by the owner" || f.content() != "canonical configuration" {
		t.Fatal("the edited copy was not saved before its replacement", saved)
	}
}

func TestConfigurationRemovalDoesNotNeedTheCheckout(t *testing.T) {
	f := newConfig(t, "link", true)
	noError(t, f.apply(false))
	noError(t, os.RemoveAll(f.provider.Checkout))
	blocked := f.expect(true, engine.Blocked)
	if !strings.Contains(blocked.Reason, "canonical configuration source is unavailable or has the wrong type; restore it before applying") {
		t.Fatal(blocked)
	}
	if err := f.apply(false); err == nil {
		t.Fatal("applied without a source")
	}
	f.expect(false, engine.Satisfied)
	noError(t, f.remove(false))
	if _, err := os.Lstat(f.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("removal left the link", err)
	}
}

func TestConfigurationDeletedTargetStillRestoresItsBaseline(t *testing.T) {
	f := newConfig(t, "copy", true)
	write(t, filepath.Join(f.destination, "mine.lua"), "personal", 0600)
	origin := describe(t, f.root)
	noError(t, f.apply(true))
	noError(t, os.RemoveAll(f.destination))
	f.expect(true, engine.Unhealthy)
	f.expect(false, engine.Satisfied)
	noError(t, f.remove(false))
	if describe(t, f.root) != origin {
		t.Fatal("the baseline of a deleted target was not restored")
	}
}

func TestConfigurationTokenFollowsTheDestination(t *testing.T) {
	f := newConfig(t, "copy", false)
	noError(t, f.apply(false))
	before := f.inspect(true)
	write(t, f.destination, "edited by the owner", 0600)
	if after := f.inspect(true); after.Token == before.Token {
		t.Fatal("a changed destination kept its token")
	}
	removal := f.inspect(false)
	write(t, f.destination, "edited again", 0600)
	if after := f.inspect(false); after.Token == removal.Token || after.Token == before.Token {
		t.Fatal("a changed destination kept its removal token")
	}
}

func TestConfigurationReleasesATargetThatTheCatalogNoLongerNames(t *testing.T) {
	f := newConfig(t, "copy", false)
	noError(t, f.apply(false))
	moved := filepath.Join(f.root, ".config", "app", "moved.json")
	f.provider.Catalog = configCatalog(t, catalog.Target{Resource: "config.test", Source: "app/config.json", Folder: "config", Path: "app/moved.json", Mode: "copy"})
	f.expect(true, engine.Unhealthy)
	noError(t, f.apply(false))
	f.expect(true, engine.Satisfied)
	if _, err := os.Lstat(filepath.Dir(f.destination)); !errors.Is(err, os.ErrNotExist) || read(t, moved) != "canonical configuration" || len(f.State().Targets) != 1 {
		t.Fatal("the earlier target stayed", err)
	}
}

func TestConfigurationFollowsARedirectedKnownFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses a symbolic link")
	}
	f := newConfig(t, "link", false)
	physical := filepath.Join(f.home, "physical")
	noError(t, os.MkdirAll(physical, 0755))
	noError(t, os.MkdirAll(f.root, 0755))
	noError(t, os.Symlink(physical, filepath.Join(f.root, ".config")))
	noError(t, f.apply(false))
	key := filepath.Join(physical, "app", "nested", "config.json")
	if record, has := f.State().Targets[key]; !has || record.Kind != "link" || f.content() != "canonical configuration" {
		t.Fatal("the record must name the physical destination", f.State().Targets)
	}
	noError(t, f.remove(false))
	if entries, err := os.ReadDir(physical); err != nil || len(entries) != 0 {
		t.Fatal("removal left content in the redirected folder", entries, err)
	}
}

func TestConfigurationRefusesADestinationInsideTheCheckout(t *testing.T) {
	f := newConfig(t, "copy", false)
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses a symbolic link")
	}
	noError(t, os.MkdirAll(f.root, 0755))
	noError(t, os.Symlink(filepath.Join(f.provider.Checkout, "inside"), filepath.Join(f.root, ".config")))
	noError(t, os.MkdirAll(filepath.Join(f.provider.Checkout, "inside"), 0755))
	if _, err := f.provider.Inspect(t.Context(), f.resource, engine.Want{Present: true}, f); err == nil || !strings.Contains(err.Error(), "overlaps its source checkout") {
		t.Fatal("a destination inside the checkout was accepted", err)
	}
}
