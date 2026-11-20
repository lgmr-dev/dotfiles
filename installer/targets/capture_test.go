package targets_test

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

type noCopyKind struct{ targets.Kind }

func (noCopyKind) CannotCopy() bool { return true }

func (noCopyKind) Save(context.Context, targets.Desired, string) (string, error) {
	return "", errors.New("this kind makes no copy")
}

func TestCaptureSavesTheLiveUnitAndChangesNothing(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(e.writer.StateDir, "v2-restore", "saved")
	file := filepath.Join(e.root, ".config", "tool", "settings.toml")
	write(t, file, "personal = true\n", 0640)
	profile := filepath.Join(e.root, ".zshrc")
	write(t, profile, "export A=1\n\n# >>> dotfiles:integration.shells >>>\n. old\n# <<< dotfiles:integration.shells <<<\n", 0644)
	before := describe(t, e.root)

	saved, made, err := e.writer.Capture(context.Background(), file, state.Record{Resource: "config.tool", Kind: "file"}, dir)
	if err != nil || len(made) != 0 {
		t.Fatalf("capture of a file: %q %v %v", saved, made, err)
	}
	backup, ok := strings.CutPrefix(saved, "backup:")
	if !ok || filepath.Dir(backup) != dir || read(t, backup) != "personal = true\n" {
		t.Fatalf("a file must give a copy in the given folder: %q", saved)
	}
	if info, err := os.Stat(backup); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0640 {
		t.Fatalf("the copy must keep the mode: %v %v", info, err)
	}

	saved, made, err = e.writer.Capture(context.Background(), targets.Key(profile, "integration.shells"), state.Record{Resource: "integration.shells", Kind: "block"}, dir)
	value, decodeErr := base64.StdEncoding.DecodeString(strings.TrimPrefix(saved, "value:"))
	if err != nil || decodeErr != nil || len(made) != 0 || string(value) != "# >>> dotfiles:integration.shells >>>\n. old\n# <<< dotfiles:integration.shells <<<\n" {
		t.Fatalf("a block must give its inline value: %q %v %v", saved, made, err)
	}

	missing := filepath.Join(e.root, "new", "deep", "file")
	saved, made, err = e.writer.Capture(context.Background(), missing, state.Record{Resource: "config.tool", Kind: "file"}, dir)
	if err != nil || saved != targets.Absent || !slices.Equal(made, []string{filepath.Join(e.root, "new", "deep"), filepath.Join(e.root, "new")}) {
		t.Fatalf("an absent unit must give absence and the folders that a write creates: %q %v %v", saved, made, err)
	}
	part := filepath.Join(e.root, "new", "profile")
	saved, made, err = e.writer.Capture(context.Background(), targets.Key(part, "integration.shells"), state.Record{Resource: "integration.shells", Kind: "block"}, dir)
	if err != nil || saved != targets.Absent || !slices.Equal(made, []string{part, filepath.Join(e.root, "new")}) {
		t.Fatalf("an absent part of an absent file must name the file first: %q %v %v", saved, made, err)
	}

	e.writer.Kinds = map[string]targets.Kind{"font": noCopyKind{targets.FileKind("font")}}
	saved, _, err = e.writer.Capture(context.Background(), file, state.Record{Resource: "tool.font", Kind: "font"}, dir)
	if err != nil || !strings.HasPrefix(saved, "fp:file:") {
		t.Fatalf("a kind without a copy must give the live fingerprint: %q %v", saved, err)
	}
	if after := describe(t, e.root); after != before {
		t.Fatalf("capture changed the home folder:\n%s\nwas:\n%s", after, before)
	}
	if len(e.store.View().Targets) != 0 {
		t.Fatal("capture changed the state")
	}
	if _, _, err := e.writer.Capture(context.Background(), file, state.Record{Resource: "x", Kind: "unknown"}, dir); err == nil {
		t.Fatal("an unknown kind must fail")
	}
}
