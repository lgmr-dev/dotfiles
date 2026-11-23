package platform_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

func writeFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func resolvedTemp(t *testing.T) string {
	t.Helper()
	root, err := platform.ResolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSnapshotAndCopyPreserveNestedBytesAndDetectLaterEdits(t *testing.T) {
	root := t.TempDir()
	source, destination := filepath.Join(root, "source"), filepath.Join(root, "destination")
	if err := os.MkdirAll(filepath.Join(source, "nested", "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"init.lua": "return {}\n", "nested/config.json": "{\"theme\":\"rose-pine\"}\n"} {
		writeFixture(t, filepath.Join(source, name), data)
	}
	before, err := platform.SnapshotTree(source)
	if err != nil || before.Kind != "directory" {
		t.Fatal(before, err)
	}
	if err := platform.CopyTree(source, destination); err != nil {
		t.Fatal(err)
	}
	after, err := platform.SnapshotTree(destination)
	if err != nil || before != after {
		t.Fatal("copied configuration differs", before, after, err)
	}
	if err := platform.CopyTree(source, destination); err == nil {
		t.Fatal("copy overwrote an existing destination")
	}
	writeFixture(t, filepath.Join(destination, "nested", "config.json"), "user customization")
	modified, err := platform.SnapshotTree(destination)
	if err != nil || modified == before {
		t.Fatal("snapshot missed nested user changes", modified, err)
	}
	absent, err := platform.SnapshotTree(filepath.Join(root, "missing"))
	if err != nil || absent.Kind != "absent" || absent.Hash != "" {
		t.Fatal("missing path is not absent", absent, err)
	}
}

func TestSnapshotDoesNotFollowLinksAndCopyRejectsLinkedSources(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Windows copies need no symlink privilege; POSIX exercises link identity")
	}
	root := t.TempDir()
	link := filepath.Join(root, "config")
	if err := os.Symlink(filepath.Join(root, "absent external target"), link); err != nil {
		t.Fatal(err)
	}
	before, err := platform.SnapshotTree(link)
	if err != nil || before.Kind != "link" {
		t.Fatal("snapshot followed a dangling user link", before, err)
	}
	if err := platform.CopyTree(link, filepath.Join(root, "copy")); err == nil {
		t.Fatal("copy followed a user link")
	}
	if _, err := platform.ReadPlainDirectory(link); err == nil {
		t.Fatal("listed a link as a plain folder")
	}
}

func TestSnapshotRefusesOversizedUserFilesBeforeCopy(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "large")
	file, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(platform.MaxTreeBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.SnapshotTree(source); err == nil {
		t.Fatal("unbounded user file was accepted")
	}
	destination := filepath.Join(root, "copy")
	if err := platform.CopyTree(source, destination); err == nil {
		t.Fatal("unbounded user file was copied")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("oversized source created a target", err)
	}
	if _, err := platform.SnapshotTreeBounded(root, platform.MaxTreeBytes, 1); err == nil {
		t.Fatal("entry bound was not applied")
	}
}

func TestInspectionFailureReportsOnlyBoundedMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-file")
	writeFixture(t, path, "personal content must not appear")
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, path, "changed")
	after, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	failure := platform.InspectionFailure(strings.Repeat("n", 4096), "file", before, after, true, nil)
	text := failure.Error()
	for _, wanted := range []string{"configuration changed during inspection; review a fresh plan", "[truncated]", "kind=file", "same_file=true", "size=32", "size=7", "mode=", "mtime_ns="} {
		if !strings.Contains(text, wanted) {
			t.Fatal("missing failure metadata", wanted, text)
		}
	}
	if len(text) > 1000 || strings.Contains(text, path) || strings.Contains(text, "personal content") {
		t.Fatal("diagnostic exceeded its content/path boundary", text)
	}
	failure = platform.InspectionFailure("quoted\"\nUnicode-λ-😀", "file", before, after, false, nil)
	if strings.Contains(failure.Error(), "\n") || !strings.Contains(failure.Error(), `entry="quoted\"\nUnicode-λ-😀"`) || !strings.Contains(failure.Error(), "same_file=false") {
		t.Fatal("relative name or identity verdict was not preserved safely", failure)
	}
	failure = platform.InspectionFailure("private-file", "file", before, nil, false, os.ErrNotExist)
	if !errors.Is(failure, os.ErrNotExist) || !strings.Contains(failure.Error(), "after={unavailable}") {
		t.Fatal("diagnostic hid original Lstat failure", failure)
	}
}

type directorySizeObservation struct {
	os.FileInfo
	size int64
}

func (i directorySizeObservation) Size() int64 { return i.size }

func TestInspectionWindowsDirectorySizeContract(t *testing.T) {
	info, err := os.Lstat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{4096, 16384, 65536} {
		before, after := directorySizeObservation{info, 0}, directorySizeObservation{info, size}
		if changed := platform.MetadataChanged(before, after); changed != (runtime.GOOS != "windows") {
			t.Fatalf("directory size %d was treated contrary to the native metadata contract: changed=%v", size, changed)
		}
	}
}

func TestInspectionStillRejectsDefinedMetadataChanges(t *testing.T) {
	for _, kind := range []string{"file-size", "mode", "directory-time"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "entry")
			if kind == "directory-time" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFixture(t, path, "original content")
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file-size":
				writeFixture(t, path, "larger changed content")
				if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(path, 0400); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(path, before.Mode().Perm()); err != nil {
						t.Error(err)
					}
				})
			case "directory-time":
				writeFixture(t, filepath.Join(path, "new-child"), "new content")
				changed := before.ModTime().Add(2 * time.Second)
				if err := os.Chtimes(path, changed, changed); err != nil {
					t.Fatal(err)
				}
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || !platform.MetadataChanged(before, after) {
				t.Fatal("defined metadata change escaped inspection", before, after, err)
			}
		})
	}
}

func TestBindDestinationResolvesTheDeepestExistingParent(t *testing.T) {
	root := resolvedTemp(t)
	bound, err := platform.BindDestination(filepath.Join(root, "missing", "deeper", "file"))
	if err != nil || bound != filepath.Join(root, "missing", "deeper", "file") {
		t.Fatal(bound, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "missing")); !os.IsNotExist(err) {
		t.Fatal("binding created a folder", err)
	}
	writeFixture(t, filepath.Join(root, "file"), "x")
	if _, err := platform.BindDestination(filepath.Join(root, "file", "child")); err == nil {
		t.Fatal("accepted a file as a parent")
	}
	if runtime.GOOS == "windows" {
		return
	}
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, filepath.Join(root, "redirected")); err != nil {
		t.Fatal(err)
	}
	bound, err = platform.BindDestination(filepath.Join(root, "redirected", "nvim", "init.lua"))
	if err != nil || bound != filepath.Join(actual, "nvim", "init.lua") {
		t.Fatal("redirected parent was not bound to its physical path", bound, err)
	}
}

func TestMoveNoReplaceKeepsAnExistingDestination(t *testing.T) {
	root := resolvedTemp(t)
	from, to := filepath.Join(root, "from"), filepath.Join(root, "sub", "to")
	writeFixture(t, from, "payload")
	writeFixture(t, to, "personal")
	if err := platform.MoveNoReplace(from, to); err == nil {
		t.Fatal("move replaced an existing destination")
	}
	data, err := os.ReadFile(to)
	if err != nil || string(data) != "personal" {
		t.Fatal("destination changed", string(data), err)
	}
	if err := os.Remove(to); err != nil {
		t.Fatal(err)
	}
	if err := platform.MoveNoReplace(from, to); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(to)
	if err != nil || string(data) != "payload" {
		t.Fatal("payload was not moved", string(data), err)
	}
	if _, err := os.Lstat(from); !os.IsNotExist(err) {
		t.Fatal("source stayed after the move", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Symlink(filepath.Join(root, "sub"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, from, "payload")
	if err := platform.MoveNoReplace(from, filepath.Join(root, "linked", "other")); err == nil {
		t.Fatal("move went through a redirected parent")
	}
}

func TestCreatedDirectoriesAreRemovedOnlyWhileEmpty(t *testing.T) {
	root := resolvedTemp(t)
	target := filepath.Join(root, "a", "b", "c", "file")
	missing, err := platform.MissingDirectories(target)
	want := []string{filepath.Join(root, "a", "b", "c"), filepath.Join(root, "a", "b"), filepath.Join(root, "a")}
	if err != nil || len(missing) != 3 || missing[0] != want[0] || missing[1] != want[1] || missing[2] != want[2] {
		t.Fatal(missing, err)
	}
	if !platform.ValidCreatedDirectories(target, missing) || platform.ValidCreatedDirectories(target, []string{filepath.Join(root, "a")}) {
		t.Fatal("ancestor chain validation is wrong")
	}
	writeFixture(t, target, "x")
	writeFixture(t, filepath.Join(root, "a", "personal"), "mine")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if kept := platform.RemoveCreatedDirectories(missing); kept != "" {
		t.Fatal("unexpected failure", kept)
	}
	if _, err := os.Lstat(filepath.Join(root, "a", "b")); !os.IsNotExist(err) {
		t.Fatal("empty created folder stayed", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "a", "personal"))
	if err != nil || string(data) != "mine" {
		t.Fatal("removed a folder with a personal addition", err)
	}
	deep := root
	for i := 0; i < platform.MaxCreatedDirectories+2; i++ {
		deep = filepath.Join(deep, "d")
	}
	if _, err := platform.MissingDirectories(deep); err == nil {
		t.Fatal("accepted an unbounded chain of missing folders")
	}
	removed, err := platform.RemoveEmptyDirectory(filepath.Join(root, "a"))
	if err != nil || removed {
		t.Fatal("removed a folder with content", removed, err)
	}
}

func TestDirectoryLinkResolvesToItsTarget(t *testing.T) {
	root := resolvedTemp(t)
	target := filepath.Join(root, "payload")
	writeFixture(t, filepath.Join(target, "file"), "x")
	link := filepath.Join(root, "current")
	if err := platform.CreateDirectoryLink(target, link); err != nil {
		t.Fatal(err)
	}
	actual, err := platform.ResolvePath(filepath.Join(link, "file"))
	if err != nil || actual != filepath.Join(target, "file") {
		t.Fatal(actual, err)
	}
	snapshot, err := platform.SnapshotTree(link)
	if err != nil || snapshot.Kind != "link" && snapshot.Kind != "junction" {
		t.Fatal("snapshot traversed the link", snapshot, err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "file")); err != nil {
		t.Fatal("link removal touched its target", err)
	}
}

func TestWindowsExtendedPath(t *testing.T) {
	for path, want := range map[string]string{`C:\Users\a`: `\\?\C:\Users\a`, `\\server\share\a`: `\\?\UNC\server\share\a`, `\\?\C:\a`: `\\?\C:\a`} {
		if got := platform.WindowsExtendedPath(path); got != want {
			t.Error(path, got, want)
		}
	}
}
