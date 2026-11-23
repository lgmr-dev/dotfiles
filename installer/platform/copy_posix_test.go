//go:build darwin || linux

package platform_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/platform"
	"golang.org/x/sys/unix"
)

func TestMetadataCopyRefusesAnUnpreservableOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can preserve the fixture owner; this assertion requires an ordinary account")
	}
	source := "/etc/protocols"
	var before unix.Stat_t
	if err := unix.Stat(source, &before); err != nil {
		if os.IsNotExist(err) {
			t.Skip("public root-owned fixture is unavailable on this image")
		}
		t.Fatal(err)
	}
	if before.Uid != 0 {
		t.Fatal("fixture must have a different, privileged owner")
	}
	destination := filepath.Join(t.TempDir(), "staged")
	if err := platform.CopyWithMetadata(context.Background(), source, destination); err == nil {
		t.Fatal("silently changed a root-owned file into a user-owned file")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("refused ownership copy left a profile copy behind", err)
	}
	var after unix.Stat_t
	if err := unix.Stat(source, &after); err != nil || before.Uid != after.Uid || before.Gid != after.Gid {
		t.Fatal("source ownership changed", err)
	}
	uid, gid, _, err := platform.FileOwner(source)
	if err != nil || uid != before.Uid || gid != before.Gid {
		t.Fatal("owner query differs", uid, gid, err)
	}
}
