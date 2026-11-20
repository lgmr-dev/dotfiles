package builds_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/builds"
	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

func TestBuildLifecycleRecordsTheCurrentLink(t *testing.T) {
	f := newFixture(t)
	pin := f.tool("1.0", "version one")
	f.provider.Pins[shared] = pin
	if finding := f.inspect(shared, true); finding.Status != engine.Missing {
		t.Fatal("a resource without a build is", finding.Status)
	}
	if finding := f.inspect(shared, false); finding.Status != engine.Absent {
		t.Fatal("an unwanted resource without a build is", finding.Status)
	}
	build := f.mustApply(shared)
	state := f.provider.StateDir
	current := builds.CurrentDir(state, shared)
	if filepath.Dir(build) != filepath.Join(state, "tools", shared) || !strings.HasPrefix(filepath.Base(build), "1.0-"+builds.Digest(pin, nil)[:12]+"-") || serial(build) != "1" {
		t.Fatal("unexpected build path", build)
	}
	record := f.rec.State().Targets[current]
	if record.Resource != shared || record.Kind != "link" || record.Applied != build || record.Next != "" || record.Before != "absent" {
		t.Fatal("unexpected link record", record)
	}
	command := builds.CommandPath(state, shared, "tool", pin)
	if command != filepath.Join(current, "tool") || read(t, command) != "version one" {
		t.Fatal("the stable command path does not reach the build", command)
	}
	if directories := builds.BinaryDirectories(state, shared, pin); !slices.Equal(directories, []string{current}) {
		t.Fatal("unexpected command folders", directories)
	}
	if builds.CommandPath(state, shared, "other", pin) != "" || builds.RequiredFilePath(state, shared, "other", pin) != "" {
		t.Fatal("an undeclared entry has a path")
	}
	var manifest builds.Manifest
	if err := platform.Decode([]byte(read(t, build+".manifest.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	tree, err := platform.SnapshotTree(build)
	if err != nil || manifest.Digest != builds.Digest(pin, nil) || manifest.Tree != tree || manifest.Resource != shared || manifest.Version != "1.0" {
		t.Fatal("unexpected manifest", manifest, err)
	}
	calls, requests := f.rec.calls, f.requests.Load()
	if finding := f.inspect(shared, true); finding.Status != engine.Satisfied || finding.Version != "1.0" {
		t.Fatal(finding)
	}
	if err := f.apply(shared); err != nil {
		t.Fatal(err)
	}
	if f.rec.calls != calls || f.requests.Load() != requests || f.build(shared) != build {
		t.Fatal("a repeated apply changed the state, downloaded or rebuilt")
	}
	if entries, err := os.ReadDir(filepath.Join(state, "downloads")); err != nil || len(entries) != 0 {
		t.Fatal("the download cache keeps an extracted archive", entries, err)
	}
}

func TestDigestChangesWithThePinAndWithABaseBuild(t *testing.T) {
	f := newFixture(t)
	pin := f.tool("1.0", "version one")
	digest := builds.Digest(pin, nil)
	other := pin
	other.SHA256 = strings.Repeat("a", 64)
	if builds.Digest(other, nil) == digest {
		t.Fatal("a pin change kept the digest")
	}
	if builds.Digest(pin, []string{"/state/tools/tool.python/3.13-aaaaaaaaaaaa-1"}) == digest ||
		builds.Digest(pin, []string{"/state/tools/tool.python/3.13-aaaaaaaaaaaa-1"}) == builds.Digest(pin, []string{"/state/tools/tool.python/3.13-aaaaaaaaaaaa-2"}) {
		t.Fatal("a base build change kept the digest")
	}
	launcher := pin
	launcher.WindowsRustLauncher = &catalog.WindowsRustLauncherPin{Revision: 1}
	rebuilt := launcher
	rebuilt.WindowsRustLauncher = &catalog.WindowsRustLauncherPin{Revision: 1, SHA256: strings.Repeat("b", 64)}
	if builds.Digest(launcher, nil) != builds.Digest(rebuilt, nil) || builds.Digest(launcher, nil) == digest {
		t.Fatal("the launcher recipe is in the digest and the installer hash is not")
	}
}

func TestUpdateKeepsTheCommandPathAndSweepRemovesTheOldBuild(t *testing.T) {
	f := newFixture(t)
	pin := f.tool("1.0", "version one")
	f.provider.Pins[shared] = pin
	old := f.mustApply(shared)
	command := builds.CommandPath(f.provider.StateDir, shared, "tool", pin)
	f.provider.Pins[shared] = f.tool("2.0", "version two")
	for _, update := range []bool{false, true} {
		finding, err := f.provider.Inspect(t.Context(), catalog.Resource{ID: shared}, engine.Want{Present: true, Update: update}, f.rec)
		if err != nil || finding.Status != engine.Outdated || finding.Version != "1.0" {
			t.Fatal("a new pin is not reported in each mode", finding, err)
		}
	}
	if read(t, command) != "version one" {
		t.Fatal("an inspection changed the build")
	}
	updated := f.mustApply(shared)
	if updated == old || read(t, command) != "version two" || read(t, filepath.Join(old, "tool")) != "version one" {
		t.Fatal("the update did not publish a new build beside the old one")
	}
	removed, left := f.sweep()
	if !slices.Equal(removed, []string{old}) || len(left) != 0 {
		t.Fatal("unexpected sweep", removed, left)
	}
	absent(t, old)
	absent(t, old+".manifest.json")
	if read(t, command) != "version two" || f.inspect(shared, true).Status != engine.Satisfied {
		t.Fatal("the sweep damaged the current build")
	}
	f.provider.Pins[shared] = pin
	if back := f.mustApply(shared); serial(back) != "1" || f.requests.Load() != 3 {
		t.Fatal("a build after the sweep did not start again", back, f.requests.Load())
	}
}

func TestRepairOfADeletedCurrentLinkDownloadsNothing(t *testing.T) {
	f := newFixture(t)
	f.provider.Pins[shared] = f.tool("1.0", "version one")
	build := f.mustApply(shared)
	if err := os.Remove(builds.CurrentDir(f.provider.StateDir, shared)); err != nil {
		t.Fatal(err)
	}
	if finding := f.inspect(shared, true); finding.Status != engine.Missing {
		t.Fatal(finding)
	}
	if removed, left := f.sweep(); len(removed) != 0 || len(left) != 0 {
		t.Fatal("the sweep removed a build that a record names", removed, left)
	}
	if repaired := f.mustApply(shared); repaired != build || f.requests.Load() != 1 {
		t.Fatal("the repair rebuilt or downloaded", repaired, f.requests.Load())
	}
}

func TestInterruptedBuildGetsANewSerial(t *testing.T) {
	f := newFixture(t)
	pin := f.add(tarball(t, []entry{{Name: "tool", Text: "program", Mode: 0755}, {Name: "data", Text: "data"}}), "tar.gz")
	pin.Commands, pin.BinDirs = map[string]string{"tool": "tool"}, []string{"."}
	pin.PythonStdlib, pin.Commands["python3"] = "lib", "tool"
	f.provider.Pins[shared] = pin
	dir := builds.ResourceDir(f.provider.StateDir, shared)
	first := filepath.Join(dir, "1.0-"+builds.Digest(pin, nil)[:12]+"-1")
	err := f.apply(shared)
	if err == nil || !strings.Contains(err.Error(), first) {
		t.Fatal("a failed preparation published, or its error does not name the folder", err)
	}
	if read(t, filepath.Join(first, "data")) != "data" {
		t.Fatal("the unfinished build did not stay")
	}
	absent(t, first+".manifest.json")
	absent(t, builds.CurrentDir(f.provider.StateDir, shared))
	if finding := f.inspect(shared, true); finding.Status != engine.Missing || len(f.rec.State().Targets) != 0 {
		t.Fatal("an unfinished build counts as present", finding)
	}
	pin.PythonStdlib = ""
	delete(pin.Commands, "python3")
	f.provider.Pins[shared] = pin
	if builds.Digest(pin, nil)[:12] == filepath.Base(first)[4:16] {
		t.Fatal("the fixture pin kept its digest")
	}
	unfinished := filepath.Join(dir, "1.0-"+builds.Digest(pin, nil)[:12]+"-1")
	write(t, filepath.Join(unfinished, "tool"), "interrupted bytes")
	build := f.mustApply(shared)
	if serial(build) != "2" || read(t, filepath.Join(build, "tool")) != "program" || read(t, filepath.Join(unfinished, "tool")) != "interrupted bytes" {
		t.Fatal("the next run reused the folder of an unfinished build", build)
	}
	removed, left := f.sweep()
	want := []string{first, unfinished}
	slices.Sort(want)
	if len(removed) != 0 || !slices.Equal(left, want) {
		t.Fatal("the sweep did not leave and report the unfinished builds", removed, left)
	}
	if leftovers, err := builds.Leftovers(t.Context(), f.provider.StateDir, f.rec.State()); err != nil || !slices.Equal(leftovers, left) {
		t.Fatal(leftovers, err)
	}
}

func TestInterruptedCurrentMoveCompletesOnlyAfterVerification(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		t.Run(map[bool]string{false: "verified", true: "changed"}[damaged], func(t *testing.T) {
			f := newFixture(t)
			f.provider.Pins[shared] = f.tool("1.0", "version one")
			old := f.mustApply(shared)
			f.provider.Pins[shared] = f.tool("2.0", "version two")
			calls := f.rec.calls
			f.rec.fail = func(call int) error {
				if call == calls+2 {
					return errors.New("interrupted before the promotion")
				}
				return nil
			}
			if err := f.apply(shared); err == nil {
				t.Fatal("the interrupted publication succeeded")
			}
			f.rec.fail = nil
			current := builds.CurrentDir(f.provider.StateDir, shared)
			record := f.rec.State().Targets[current]
			candidate := f.build(shared)
			if record.Applied != old || record.Next != candidate || candidate == old {
				t.Fatal("unexpected record of the unfinished move", record)
			}
			if damaged {
				write(t, filepath.Join(candidate, "tool"), "changed after the interruption")
			}
			if finding := f.inspect(shared, true); finding.Status != engine.Unhealthy {
				t.Fatal("an unfinished move is", finding.Status)
			}
			requests := f.requests.Load()
			build := f.mustApply(shared)
			record = f.rec.State().Targets[current]
			if record.Applied != build || record.Next != "" {
				t.Fatal("the record was not promoted", record)
			}
			if damaged {
				if build == candidate || serial(build) != "2" || f.requests.Load() != requests+1 || read(t, filepath.Join(candidate, "tool")) != "changed after the interruption" {
					t.Fatal("a candidate that failed its verification completed the move", build)
				}
				return
			}
			if build != candidate || f.requests.Load() != requests {
				t.Fatal("a verified candidate was rebuilt", build)
			}
		})
	}
}

func TestInterruptedMoveWithoutACurrentLinkCompletes(t *testing.T) {
	f := newFixture(t)
	f.provider.Pins[shared] = f.tool("1.0", "version one")
	old := f.mustApply(shared)
	dir := builds.ResourceDir(f.provider.StateDir, shared)
	current := builds.CurrentDir(f.provider.StateDir, shared)
	if err := os.Rename(current, filepath.Join(dir, ".previous-"+filepath.Base(old))); err != nil {
		t.Fatal(err)
	}
	if err := f.rec.Update(func(s *state.State) error {
		return s.BeginTarget(current, state.Record{Resource: shared, Kind: "link", Next: old})
	}); err != nil {
		t.Fatal(err)
	}
	if build := f.mustApply(shared); build != old || f.requests.Load() != 1 {
		t.Fatal("the move did not complete with the verified build", build)
	}
	absent(t, filepath.Join(dir, ".previous-"+filepath.Base(old)))
}

func TestForeignCurrentLinkStays(t *testing.T) {
	f := newFixture(t)
	f.provider.Pins[shared] = f.tool("1.0", "version one")
	current := builds.CurrentDir(f.provider.StateDir, shared)
	personal := filepath.Join(filepath.Dir(f.provider.StateDir), "personal")
	if err := os.MkdirAll(filepath.Dir(current), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(personal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := platform.CreateDirectoryLink(personal, current); err != nil {
		t.Fatal(err)
	}
	if finding := f.inspect(shared, true); finding.Status != engine.Blocked {
		t.Fatal("an unrecorded link is", finding.Status)
	}
	if err := f.apply(shared); err == nil {
		t.Fatal("setup replaced an unrecorded link")
	}
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	build := f.mustApply(shared)
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := platform.CreateDirectoryLink(personal, current); err != nil {
		t.Fatal(err)
	}
	for _, present := range []bool{true, false} {
		if finding := f.inspect(shared, present); finding.Status != engine.Changed || !slices.Equal(finding.Preserved, []string{current}) {
			t.Fatal("a link that setup did not publish is", finding.Status)
		}
	}
	if err := f.apply(shared); err == nil {
		t.Fatal("setup replaced a changed link without approval")
	}
	if err := f.remove(shared); err == nil {
		t.Fatal("setup removed a changed link")
	}
	if target, err := os.Readlink(current); err != nil || target != personal {
		t.Fatal("the changed link did not stay", target, err)
	}
	if removed, _ := f.sweep(); len(removed) != 0 {
		t.Fatal("the sweep removed a build that a record names", removed)
	}
	finding := f.inspect(shared, true)
	finding.Replace = true
	if err := f.provider.Apply(t.Context(), catalog.Resource{ID: shared}, finding, f.rec); err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(filepath.Dir(current), ".previous-"+filepath.Base(build))
	if target, err := os.Readlink(saved); err != nil || target != personal || f.build(shared) != build {
		t.Fatal("the approved replacement did not save the changed link", target, err)
	}
	if _, left := f.sweep(); !slices.Equal(left, []string{saved}) {
		t.Fatal("the saved link is not reported", left)
	}
}

func TestChangedCurrentBuildIsRepairedBesideIt(t *testing.T) {
	f := newFixture(t)
	f.provider.Pins[shared] = f.tool("1.0", "version one")
	old := f.mustApply(shared)
	write(t, filepath.Join(old, "notes"), "personal notes")
	if finding := f.inspect(shared, true); finding.Status != engine.Unhealthy {
		t.Fatal("a changed current build is", finding.Status)
	}
	if finding := f.inspect(shared, false); finding.Status != engine.Changed || !slices.Equal(finding.Preserved, []string{old}) {
		t.Fatal("a changed current build of a deselected resource is", finding.Status)
	}
	build := f.mustApply(shared)
	if build == old || serial(build) != "2" || read(t, filepath.Join(old, "notes")) != "personal notes" {
		t.Fatal("the repair did not use a new build", build)
	}
	removed, left := f.sweep()
	if len(removed) != 0 || !slices.Equal(left, []string{old}) || read(t, filepath.Join(old, "tool")) != "version one" {
		t.Fatal("the sweep did not keep the changed build complete", removed, left)
	}
}

func TestRemoveReleasesTheLinkAndTheSweepRemovesTheBuild(t *testing.T) {
	f := newFixture(t)
	f.provider.Pins[shared] = f.tool("1.0", "version one")
	build := f.mustApply(shared)
	if finding := f.inspect(shared, false); finding.Status != engine.Satisfied {
		t.Fatal(finding)
	}
	if err := f.remove(shared); err != nil {
		t.Fatal(err)
	}
	absent(t, builds.CurrentDir(f.provider.StateDir, shared))
	if len(f.rec.State().Targets) != 0 || f.inspect(shared, false).Status != engine.Absent {
		t.Fatal("the removal left a record")
	}
	if read(t, filepath.Join(build, "tool")) != "version one" {
		t.Fatal("the removal deleted the build before the sweep")
	}
	removed, left := f.sweep()
	if !slices.Equal(removed, []string{build}) || len(left) != 0 {
		t.Fatal(removed, left)
	}
	absent(t, builds.ResourceDir(f.provider.StateDir, shared))
	if again := f.mustApply(shared); again != build || f.requests.Load() != 2 {
		t.Fatal("a new install did not build again", again, f.requests.Load())
	}
}

func TestSweepKeepsAdditionsAndUnknownEntries(t *testing.T) {
	f := newFixture(t)
	f.provider.Pins[shared] = f.tool("1.0", "version one")
	first := f.mustApply(shared)
	f.provider.Pins[shared] = f.tool("2.0", "version two")
	second := f.mustApply(shared)
	f.provider.Pins[shared] = f.tool("3.0", "version three")
	third := f.mustApply(shared)
	write(t, filepath.Join(first, "added"), "personal addition")
	dir := builds.ResourceDir(f.provider.StateDir, shared)
	unknown := filepath.Join(dir, "notes.txt")
	write(t, unknown, "personal notes")
	stray := filepath.Join(builds.ToolsDir(f.provider.StateDir), "Personal Folder")
	write(t, filepath.Join(stray, "file"), "personal")
	leftovers, err := builds.Leftovers(t.Context(), f.provider.StateDir, f.rec.State())
	if err != nil || !slices.Equal(leftovers, []string{stray, first, unknown}) {
		t.Fatal(leftovers, err)
	}
	if read(t, filepath.Join(second, "tool")) != "version two" {
		t.Fatal("the read-only listing removed a build")
	}
	removed, left := f.sweep()
	if !slices.Equal(removed, []string{second}) || !slices.Equal(left, leftovers) {
		t.Fatal(removed, left)
	}
	if read(t, filepath.Join(first, "added")) != "personal addition" || read(t, filepath.Join(first, "tool")) != "version one" || read(t, first+".manifest.json") == "" {
		t.Fatal("the sweep changed a build with an addition")
	}
	if read(t, unknown) != "personal notes" || read(t, filepath.Join(stray, "file")) != "personal" || read(t, filepath.Join(third, "tool")) != "version three" {
		t.Fatal("the sweep changed an entry that it does not own")
	}
}

func TestSweepStopsWhenItCannotReadWhatTheActiveBuildUses(t *testing.T) {
	f := newFixture(t)
	f.provider.Pins[shared] = f.tool("1.0", "version one")
	first := f.mustApply(shared)
	f.provider.Pins[shared] = f.tool("2.0", "version two")
	second := f.mustApply(shared)
	if err := os.Remove(second + ".manifest.json"); err != nil {
		t.Fatal(err)
	}
	removed, _, err := builds.Sweep(t.Context(), f.provider.StateDir, f.rec.State())
	if err == nil || len(removed) != 0 || read(t, filepath.Join(first, "tool")) != "version one" {
		t.Fatal("the sweep removed a build without the manifest of the active build", removed, err)
	}
	if finding := f.inspect(shared, true); finding.Status != engine.Unhealthy {
		t.Fatal("a current build without a manifest is", finding.Status)
	}
}

func TestPortableDesktopBuildsDiscloseTheirRouteBeforeAnAction(t *testing.T) {
	f := newFixture(t)
	f.provider.Pins["tool.obsidian-portable"] = f.tool("1.0", "version one")
	finding := f.inspect("tool.obsidian-portable", true)
	if len(finding.Notes) != 1 || !strings.Contains(finding.Notes[0], "without Chromium's sandbox") {
		t.Fatal("the route note is missing before the install", finding.Notes)
	}
	f.mustApply("tool.obsidian-portable")
	if finding := f.inspect("tool.obsidian-portable", true); len(finding.Notes) != 0 {
		t.Fatal("a satisfied build repeats its route note", finding.Notes)
	}
}
