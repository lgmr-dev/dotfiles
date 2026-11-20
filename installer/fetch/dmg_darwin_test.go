package fetch_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/fetch"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

func TestNativeDMGPreservesBundleFilesWithoutSyntheticAppleDouble(t *testing.T) {
	if os.Getenv("DOTFILES_TEST_NATIVE") != "1" {
		t.Skip("requires an opted-in disposable Mac for a private disk-image mount")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	image := filepath.Join(root, "fixture.dmg")
	run := func(args ...string) {
		t.Helper()
		if output, err := exec.Command("/usr/bin/hdiutil", args...).CombinedOutput(); err != nil {
			t.Fatalf("private disk image %v: %v\n%s", args, err, output)
		}
	}
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	run("create", "-size", "64m", "-fs", "APFS", "-type", "UDIF", image)
	run("attach", "-nobrowse", "-mountpoint", source, image)
	attached := true
	t.Cleanup(func() {
		if attached {
			if output, err := exec.Command("/usr/bin/hdiutil", "detach", source).CombinedOutput(); err != nil {
				t.Errorf("detach private source image: %v\n%s", err, output)
			}
		}
	})
	bundle := filepath.Join(source, "Fixture.app")
	for _, name := range []string{"Contents/Resources/note.txt", "Contents/Resources/._actual-file"} {
		path := filepath.Join(bundle, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("original "+name+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Setxattr(filepath.Join(bundle, "Contents/Resources/note.txt"), "com.apple.metadata:pr87-test", []byte("fixture-metadata"), 0); err != nil {
		t.Fatal(err)
	}
	before, err := platform.SnapshotTreeBounded(bundle, fetch.MaxPackageBytes, fetch.MaxPackageEntries)
	if err != nil {
		t.Fatal(err)
	}
	run("detach", source)
	attached = false
	file, err := os.Open(image)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "extracted")
	if err := fetch.Extract(t.Context(), file, info.Size(), catalog.ArchivePin{Format: "dmg", AppBundle: "Fixture.app", DMGRevision: 1}, destination, ""); err != nil {
		t.Fatal(err)
	}
	after, err := platform.SnapshotTreeBounded(filepath.Join(destination, "Fixture.app"), fetch.MaxPackageBytes, fetch.MaxPackageEntries)
	if err != nil || before != after {
		t.Fatalf("native disk-image transport changed actual bundle files: before=%+v after=%+v err=%v", before, after, err)
	}
}
