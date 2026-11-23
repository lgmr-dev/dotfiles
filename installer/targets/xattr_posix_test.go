//go:build darwin || linux

package targets_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/targets"
	"golang.org/x/sys/unix"
)

const testAttribute = "user.dotfiles-test"

func setAttribute(t *testing.T, path, value string) {
	t.Helper()
	err := unix.Setxattr(path, testAttribute, []byte(value), 0)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
		t.Skip("the temporary file system refuses user extended attributes", err)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func attributeOf(t *testing.T, path string) string {
	t.Helper()
	value := make([]byte, 256)
	count, err := unix.Getxattr(path, testAttribute, value)
	if err != nil {
		t.Fatalf("%s lost its extended attribute: %v", path, err)
	}
	return string(value[:count])
}

func TestApprovedReplacementKeepsExtendedAttributesInTheBackupAndTheRestore(t *testing.T) {
	for _, name := range []string{"file", "tree"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			f := fixtureNamed(name)
			d := f.first(e)
			members := []string{"."}
			if name == "file" {
				write(t, d.Path, strings.Repeat("personal settings\n", 20000), 0640)
			} else {
				replaceWith(e, d.Path, map[string]string{"mine.txt": "personal tree\n"})
				members = append(members, "mine.txt")
			}
			for _, member := range members {
				setAttribute(t, filepath.Join(d.Path, member), "personal "+member)
			}
			e.mustConverge(d, targets.Replace)
			backups := e.baselines()
			if len(backups) != 1 {
				t.Fatal("the approved replacement must save one backup", backups)
			}
			for _, member := range members {
				if got := attributeOf(t, filepath.Join(backups[0], member)); got != "personal "+member {
					t.Fatalf("the backup of %s has the attribute %q", member, got)
				}
			}
			e.mustRelease(d, targets.Preserve)
			for _, member := range members {
				if got := attributeOf(t, filepath.Join(d.Path, member)); got != "personal "+member {
					t.Fatalf("the restored %s has the attribute %q", member, got)
				}
			}
		})
	}
}
