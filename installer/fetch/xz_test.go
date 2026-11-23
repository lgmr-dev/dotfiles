package fetch_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/fetch"
)

func TestXZArchiveRetainsTarSafetyAndDecoderIntegrity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Zen's XZ archive is Linux-only")
	}
	if _, err := os.Stat("/usr/bin/xz"); err != nil {
		t.Fatal("Linux extraction fixture requires xz", err)
	}
	for _, name := range []string{"tool", "../outside"} {
		plain := debTarFixture(t, []archiveFixtureEntry{{Name: name, Text: "fixture", Mode: 0755}})
		cmd := exec.Command("/usr/bin/xz", "--compress", "--stdout")
		cmd.Stdin = bytes.NewReader(plain)
		encoded, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		pin, client, _ := archivePinFor(t, encoded, "tar.xz")
		err = fetch.Download(context.Background(), pin, filepath.Join(t.TempDir(), "payload"), fetch.Options{Client: client})
		if (err == nil) != (name == "tool") {
			t.Fatal("XZ bypassed checked tar extraction", name, err)
		}
		if name == "tool" {
			encoded[len(encoded)-3] ^= 0xff
			pin, client, _ = archivePinFor(t, encoded, "tar.xz")
			if err := fetch.Download(context.Background(), pin, filepath.Join(t.TempDir(), "payload"), fetch.Options{Client: client}); err == nil {
				t.Fatal("decoder checksum failure was ignored")
			}
		}
	}
}
