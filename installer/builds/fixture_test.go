package builds_test

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
	"sync/atomic"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/builds"
	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/state"
)

const shared = "tool.shared"

type entry struct {
	Name, Text, Link string
	Mode             int64
}

func tarball(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if _, err := buffer.Write(plainTar(t, entries)); err != nil {
		t.Fatal(err)
	}
	var packed bytes.Buffer
	gz := gzip.NewWriter(&packed)
	if _, err := io.Copy(gz, &buffer); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return packed.Bytes()
}

func plainTar(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, item := range entries {
		header := &tar.Header{Name: item.Name, Size: int64(len(item.Text)), Mode: item.Mode, Typeflag: tar.TypeReg}
		if item.Link != "" {
			header.Typeflag, header.Linkname, header.Size = tar.TypeSymlink, item.Link, 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if item.Link == "" {
			if _, err := io.WriteString(writer, item.Text); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

type records struct {
	state state.State
	calls int
	fail  func(call int) error
}

func (r *records) State() state.State { return r.state.Clone() }

func (r *records) Update(edit func(*state.State) error) error {
	r.calls++
	if r.fail != nil {
		if err := r.fail(r.calls); err != nil {
			return err
		}
	}
	next := r.state.Clone()
	if err := edit(&next); err != nil {
		return err
	}
	next.Schema, next.Target = 2, "test/amd64:/home/test"
	if err := next.Validate(); err != nil {
		return err
	}
	r.state = next
	return nil
}

type runner struct {
	commands []platform.Command
	hook     func(platform.Command) error
}

func (r *runner) Run(_ context.Context, c platform.Command) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	r.commands = append(r.commands, c)
	if r.hook != nil {
		return nil, r.hook(c)
	}
	return nil, nil
}

type fixture struct {
	t        *testing.T
	provider *builds.Provider
	rec      *records
	runner   *runner
	server   *httptest.Server
	assets   map[string][]byte
	requests atomic.Int32
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, rec: &records{}, runner: &runner{}, assets: map[string][]byte{}}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := f.assets[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.requests.Add(1)
		if _, err := w.Write(data); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(f.server.Close)
	directory, err := platform.ResolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.provider = &builds.Provider{StateDir: filepath.Join(directory, "state"), Pins: map[string]catalog.ArchivePin{}, Client: f.server.Client(), Runner: f.runner}
	return f
}

func (f *fixture) add(data []byte, format string) catalog.ArchivePin {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	f.assets["/"+digest] = data
	return catalog.ArchivePin{Version: "1.0", URL: f.server.URL + "/" + digest, SHA256: digest, Format: format}
}

func (f *fixture) tool(version, text string) catalog.ArchivePin {
	pin := f.add(tarball(f.t, []entry{{Name: "tool", Text: text, Mode: 0755}}), "tar.gz")
	pin.Version, pin.Commands, pin.BinDirs = version, map[string]string{"tool": "tool"}, []string{"."}
	return pin
}

func (f *fixture) inspect(id string, present bool) engine.Finding {
	f.t.Helper()
	finding, err := f.provider.Inspect(f.t.Context(), catalog.Resource{ID: id}, engine.Want{Present: present}, f.rec)
	if err != nil {
		f.t.Fatal(err)
	}
	return finding
}

func (f *fixture) apply(id string) error {
	f.t.Helper()
	return f.provider.Apply(f.t.Context(), catalog.Resource{ID: id}, f.inspect(id, true), f.rec)
}

func (f *fixture) mustApply(id string) string {
	f.t.Helper()
	if err := f.apply(id); err != nil {
		f.t.Fatal(err)
	}
	if finding := f.inspect(id, true); finding.Status != engine.Satisfied {
		f.t.Fatalf("%s is %s after its apply step: %s", id, finding.Status, finding.Reason)
	}
	return f.build(id)
}

func (f *fixture) build(id string) string {
	f.t.Helper()
	path, err := builds.BuildPath(f.provider.StateDir, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *fixture) remove(id string) error {
	f.t.Helper()
	return f.provider.Remove(f.t.Context(), catalog.Resource{ID: id}, f.inspect(id, false), f.rec)
}

func (f *fixture) sweep() (removed, left []string) {
	f.t.Helper()
	removed, left, err := builds.Sweep(f.t.Context(), f.provider.StateDir, f.rec.State())
	if err != nil {
		f.t.Fatal(err)
	}
	return removed, left
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0755); err != nil {
		t.Fatal(err)
	}
}

func absent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists: %v", path, err)
	}
}

func serial(build string) string {
	return build[strings.LastIndex(build, "-")+1:]
}
