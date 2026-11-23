package v2import_test

import (
	"archive/tar"
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
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/builds"
	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/hooks"
	"github.com/lgmr-dev/dotfiles/installer/state"
)

type storeRecords struct{ *state.Store }

func (s storeRecords) State() state.State { return s.View() }

func nvimArchive(t *testing.T) (*httptest.Server, catalog.ArchivePin) {
	t.Helper()
	var packed bytes.Buffer
	gz := gzip.NewWriter(&packed)
	tw := tar.NewWriter(gz)
	text := "v3 nvim\n"
	if err := tw.WriteHeader(&tar.Header{Name: "bin/nvim", Mode: 0755, Size: int64(len(text)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, text); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		t.Fatal(err)
	}
	data := packed.Bytes()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write(data); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	sum := sha256.Sum256(data)
	return server, catalog.ArchivePin{Version: "0.12.0", URL: server.URL + "/nvim.tar.gz", SHA256: hex.EncodeToString(sum[:]), Format: "tar.gz",
		Commands: map[string]string{"nvim": "bin/nvim"}, BinDirs: []string{"bin"}}
}

func selectNeovimInV3(t *testing.T, f *fx) {
	t.Helper()
	server, pin := nvimArchive(t)
	provider := &builds.Provider{StateDir: f.state, Pins: map[string]catalog.ArchivePin{"tool.nvim": pin}, Client: server.Client()}
	store, err := state.Open(f.state, f.engine().Target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	rec := storeRecords{store}
	r, _ := f.catalog.Resource("tool.nvim")
	finding, err := provider.Inspect(context.Background(), r, engine.Want{Present: true}, rec)
	if err != nil || finding.Status != engine.Missing {
		t.Fatalf("a v3 selection of Neovim must find no build: %s %s %v", finding.Status, finding.Reason, err)
	}
	if err := provider.Apply(context.Background(), r, finding, rec); err != nil {
		t.Fatalf("a v3 selection of Neovim must build it: %v", err)
	}
	if after, err := provider.Inspect(context.Background(), r, engine.Want{Present: true}, rec); err != nil || after.Status != engine.Satisfied {
		t.Fatalf("Neovim must be satisfied after its v3 build: %s %s %v", after.Status, after.Reason, err)
	}
}

func TestAChangedLinkOfAStagedBuildStaysAndTheReviewNamesIt(t *testing.T) {
	f := linuxFixture(t)
	f.save()
	f.mustImport()
	mustRestore(t, f)
	current := builds.CurrentDir(f.state, "tool.nvim")
	mine := filepath.Join(f.root, "my-nvim")
	write(t, filepath.Join(mine, "bin", "nvim"), "my nvim\n", 0755)
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	link(t, mine, current)
	deselectNeovim(t, f)
	proposal, err := f.preview()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(proposal.Details, "The link "+current+" does not point at the build that an earlier import made") {
		t.Fatalf("the review must name the changed link:\n%s", proposal.Details)
	}
	if result, err := proposal.Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, result.Message)
	}
	if target, err := os.Readlink(current); err != nil || target != mine {
		t.Fatalf("the changed link must stay: %q %v", target, err)
	}
	if _, err := os.Lstat(builds.CurrentDir(f.state, hooks.NvimResource)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the unchanged link of the runtime must go: %v", err)
	}
}
