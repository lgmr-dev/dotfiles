package fetch_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/fetch"
)

type archiveFixtureEntry struct {
	Name, Text, Link string
	Mode             int64
}

func archiveTar(t *testing.T, entries []archiveFixtureEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tarball := tar.NewWriter(gz)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.Name, Size: int64(len(entry.Text)), Mode: entry.Mode, Typeflag: tar.TypeReg}
		if entry.Link != "" {
			header.Typeflag, header.Linkname, header.Size = tar.TypeSymlink, entry.Link, 0
		}
		if err := tarball.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.Link == "" {
			if _, err := io.WriteString(tarball, entry.Text); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := errors.Join(tarball.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func archivePinFor(t *testing.T, data []byte, format string) (catalog.ArchivePin, *http.Client, *atomic.Int32) {
	t.Helper()
	count := &atomic.Int32{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if _, err := w.Write(data); err != nil {
			t.Errorf("fixture response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	hash := sha256.Sum256(data)
	return catalog.ArchivePin{Version: "1.0", URL: server.URL + "/tool", SHA256: hex.EncodeToString(hash[:]), Format: format, Commands: map[string]string{"tool": "tool"}, BinDirs: []string{"."}}, server.Client(), count
}

func TestArchiveRejectsCorruptDownloadBeforeExtraction(t *testing.T) {
	data := archiveTar(t, []archiveFixtureEntry{{Name: "tool", Text: "program", Mode: 0755}})
	pin, client, _ := archivePinFor(t, data, "tar.gz")
	pin.SHA256 = strings.Repeat("0", 64)
	destination := filepath.Join(t.TempDir(), "payload")
	if err := fetch.Download(context.Background(), pin, destination, fetch.Options{Client: client}); err == nil {
		t.Fatal("accepted incorrect checksum")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("extracted bytes before checksum verification", err)
	}
}

func TestArchiveRejectsNonExecutableDeclaredCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not use POSIX execute bits")
	}
	pin, client, _ := archivePinFor(t, archiveTar(t, []archiveFixtureEntry{{Name: "tool", Text: "not executable", Mode: 0644}}), "tar.gz")
	if err := fetch.Download(context.Background(), pin, filepath.Join(t.TempDir(), "payload"), fetch.Options{Client: client}); err == nil {
		t.Fatal("accepted a declared command that cannot execute")
	}
}

func TestArchiveRejectsUnsafeAndAmbiguousPaths(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", "a/../../escape", `a\escape`, "a:stream", "NUL.txt", "a./tool", "COM1", "a//tool"} {
		t.Run(name, func(t *testing.T) {
			pin, client, _ := archivePinFor(t, archiveTar(t, []archiveFixtureEntry{{Name: name, Text: "bad", Mode: 0755}}), "tar.gz")
			if err := fetch.Download(context.Background(), pin, filepath.Join(t.TempDir(), "payload"), fetch.Options{Client: client}); err == nil {
				t.Fatal("accepted unsafe path")
			}
		})
	}
	for _, entries := range [][]archiveFixtureEntry{
		{{Name: "tool", Text: "one"}, {Name: "tool", Text: "two"}},
		{{Name: "tool", Link: "../outside"}},
		{{Name: "dir", Link: "inside"}, {Name: "dir/tool", Text: "redirected"}},
	} {
		pin, client, _ := archivePinFor(t, archiveTar(t, entries), "tar.gz")
		if err := fetch.Download(context.Background(), pin, filepath.Join(t.TempDir(), "payload"), fetch.Options{Client: client}); err == nil {
			t.Fatal("accepted duplicate or redirecting member", entries)
		}
	}
}

func TestArchiveExtractsZIPAndTarWithVerifiedCommands(t *testing.T) {
	for _, format := range []string{"zip", "tar.gz"} {
		t.Run(format, func(t *testing.T) {
			data := archiveTar(t, []archiveFixtureEntry{{Name: "tool", Text: "program", Mode: 0755}})
			if format == "zip" {
				var buffer bytes.Buffer
				writer := zip.NewWriter(&buffer)
				header := &zip.FileHeader{Name: "tool"}
				header.SetMode(0755)
				member, err := writer.CreateHeader(header)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(member, "program"); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				data = buffer.Bytes()
			}
			pin, client, _ := archivePinFor(t, data, format)
			destination := filepath.Join(t.TempDir(), "payload")
			if err := fetch.Download(context.Background(), pin, destination, fetch.Options{Client: client}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(destination, "tool"))
			if err != nil || string(got) != "program" {
				t.Fatal("extracted command differs", string(got), err)
			}
		})
	}
}

func TestArchiveNonCommandPayloadRequiresItsDeclaredEntryFiles(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-module", true: "valid-module"}[present], func(t *testing.T) {
			name := "other.txt"
			if present {
				name = "module.psd1"
			}
			pin, client, _ := archivePinFor(t, archiveTar(t, []archiveFixtureEntry{{Name: name, Text: "@{}", Mode: 0644}}), "tar.gz")
			pin.Commands = nil
			pin.BinDirs = nil
			pin.RequiredFiles = []string{"module.psd1"}
			err := fetch.Download(context.Background(), pin, filepath.Join(t.TempDir(), "payload"), fetch.Options{Client: client})
			if (err == nil) != present {
				t.Fatal("required module validation", err)
			}
		})
	}
}

func TestArchiveHandlesGitSourceGlobalPAXMetadata(t *testing.T) {
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	for _, header := range []*tar.Header{
		{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "pinned-git-commit"}},
		{Name: "source/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "source/module.zsh", Typeflag: tar.TypeReg, Mode: 0644, Size: 1},
	} {
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := tw.Write([]byte("#")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	pin, client, _ := archivePinFor(t, buffer.Bytes(), "tar.gz")
	pin.Commands = nil
	pin.BinDirs = nil
	pin.RequiredFiles = []string{"module.zsh"}
	pin.StripComponents = 1
	root := filepath.Join(t.TempDir(), "payload")
	if err := fetch.Download(context.Background(), pin, root, fetch.Options{Client: client}); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 1 || files[0].Name() != "module.zsh" {
		t.Fatal("metadata became an extracted file", files, err)
	}
}

func TestArchiveRejectsGlobalFileAttributeOverrides(t *testing.T) {
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "global", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"path": "outside"}}); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	pin, client, _ := archivePinFor(t, buffer.Bytes(), "tar.gz")
	if err := fetch.Download(context.Background(), pin, filepath.Join(t.TempDir(), "payload"), fetch.Options{Client: client}); err == nil || !strings.Contains(err.Error(), "unsupported global archive attribute") {
		t.Fatal("global path override accepted", err)
	}
}

func TestArchiveCaseDistinctFilesFollowDestinationFilesystem(t *testing.T) {
	parent := t.TempDir()
	if err := os.WriteFile(filepath.Join(parent, "case-probe"), []byte("probe"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(parent, "CASE-PROBE"))
	sensitive := errors.Is(err, os.ErrNotExist)
	if err != nil && !sensitive {
		t.Fatal(err)
	}
	entries := []archiveFixtureEntry{{Name: "tool", Text: "program", Mode: 0755}, {Name: "terminfo/2621A", Text: "uppercase", Mode: 0644}, {Name: "terminfo/2621a", Text: "lowercase", Mode: 0644}}
	pin, client, _ := archivePinFor(t, archiveTar(t, entries), "tar.gz")
	destination := filepath.Join(parent, "payload")
	err = fetch.Download(context.Background(), pin, destination, fetch.Options{Client: client})
	if !sensitive {
		if err == nil {
			t.Fatal("accepted colliding names on a case-insensitive filesystem")
		}
		return
	}
	if err != nil {
		t.Fatal("case-distinct native files rejected", err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(entry.Name)))
		if err != nil || string(data) != entry.Text {
			t.Fatal("archive member changed", entry.Name, string(data), err)
		}
	}
	files, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatal("extraction left probing artifacts", files)
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
