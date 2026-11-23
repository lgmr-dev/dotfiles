package platform_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

func TestMetadataCopyNeverReplacesAnExistingFile(t *testing.T) {
	root := t.TempDir()
	from, to := filepath.Join(root, "source"), filepath.Join(root, "existing")
	writeFixture(t, from, "source")
	writeFixture(t, to, "personal staging file")
	if err := platform.CopyWithMetadata(context.Background(), from, to); err == nil {
		t.Fatal("accepted preexisting copy destination")
	}
	data, err := os.ReadFile(to)
	if err != nil || string(data) != "personal staging file" {
		t.Fatal("overwrote existing staging file", string(data), err)
	}
}

func TestCancelledMetadataCopyLeavesNoDestination(t *testing.T) {
	root := t.TempDir()
	from, to := filepath.Join(root, "source"), filepath.Join(root, "next")
	writeFixture(t, from, "personal profile")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := platform.CopyWithMetadata(ctx, from, to); err == nil {
		t.Fatal("cancelled copy succeeded")
	}
	if _, err := os.Lstat(to); !os.IsNotExist(err) {
		t.Fatal("cancelled copy left bytes", err)
	}
}

func TestMetadataCopyKeepsBytesAndMode(t *testing.T) {
	root := t.TempDir()
	from, to := filepath.Join(root, "source"), filepath.Join(root, "next")
	writeFixture(t, from, "personal profile")
	if err := os.Chmod(from, 0640); err != nil {
		t.Fatal(err)
	}
	if err := platform.CopyWithMetadata(context.Background(), from, to); err != nil {
		t.Fatal(err)
	}
	before, err := platform.SnapshotTree(from)
	if err != nil {
		t.Fatal(err)
	}
	after, err := platform.SnapshotTree(to)
	if err != nil || before != after {
		t.Fatal("copy differs from its source", before, after, err)
	}
}
