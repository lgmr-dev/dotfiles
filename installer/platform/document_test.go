package platform_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

func TestDecodeIsStrict(t *testing.T) {
	var value struct {
		Name string `json:"name"`
	}
	if err := platform.Decode([]byte(`{"name":"a"}`), &value); err != nil || value.Name != "a" {
		t.Fatal(value, err)
	}
	for _, data := range []string{`{"name":"a","other":1}`, `{"name":"a"} {"name":"b"}`, `{"name":"a"} x`, ``, `{"name":`} {
		if err := platform.Decode([]byte(data), &value); err == nil {
			t.Errorf("accepted %q", data)
		}
	}
	if err := platform.Decode(bytes.Repeat([]byte(" "), 8*1024*1024+1), &value); err == nil {
		t.Fatal("accepted an oversized document")
	}
}

func TestSaveDocumentPublishesPrivateCompleteBytes(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(directory, "state.json")
	if _, err := platform.ReadDocument(path); !os.IsNotExist(err) {
		t.Fatal("missing document must report absence", err)
	}
	if err := platform.SaveDocument(path, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if err := platform.SaveDocument(path, []byte("second\n")); err != nil {
		t.Fatal(err)
	}
	data, err := platform.ReadDocument(path)
	if err != nil || string(data) != "second\n" {
		t.Fatal(string(data), err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("document is not private", info.Mode())
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatal("publication left a temporary file", entries, err)
	}
}

func TestRejectedPublicationPreservesPreviousDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := platform.SaveDocument(path, []byte("kept\n")); err != nil {
		t.Fatal(err)
	}
	if err := platform.SaveDocument(path, bytes.Repeat([]byte("x"), 8*1024*1024+1)); err == nil || !strings.Contains(err.Error(), "existing state preserved") {
		t.Fatal("oversized document accepted", err)
	}
	data, err := platform.ReadDocument(path)
	if err != nil || string(data) != "kept\n" {
		t.Fatal(string(data), err)
	}
	directory := filepath.Join(filepath.Dir(path), "folder.json")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := platform.SaveDocument(directory, []byte("x")); err == nil {
		t.Fatal("replaced a folder with a document")
	}
	if _, err := platform.ReadDocument(directory); err == nil {
		t.Fatal("read a folder as a document")
	}
}

func TestDocumentReplacementWhileReaderIsOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := platform.SaveDocument(path, []byte("old\n")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := root.Open(filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.SaveDocument(path, []byte("new\n")); err != nil {
		t.Fatal("publication failed while a reader held the old document", err)
	}
	old := make([]byte, 16)
	n, _ := reader.Read(old)
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := platform.ReadDocument(path)
	if string(old[:n]) != "old\n" || err != nil || string(data) != "new\n" {
		t.Fatal(string(old[:n]), string(data), err)
	}
}

func TestDocumentFolderMustNotBeALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need a privilege on Windows")
	}
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	if err := platform.SaveDocument(filepath.Join(link, "state.json"), []byte("x")); err == nil {
		t.Fatal("published through a linked folder")
	}
	if err := os.WriteFile(filepath.Join(actual, "state.json"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.ReadDocument(filepath.Join(link, "state.json")); err == nil {
		t.Fatal("read through a linked folder")
	}
}

func TestLockExcludesASecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "mutation.lock")
	release, err := platform.Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := platform.Lock(path); err == nil || !strings.Contains(err.Error(), "another installer holds the mutation lock") {
		t.Fatal("second holder was admitted", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	release, err = platform.Lock(path)
	if err != nil {
		t.Fatal("lock was not released", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}
