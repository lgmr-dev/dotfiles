package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/state"
)

func importFixture(t *testing.T) (setup, []byte) {
	t.Helper()
	s := wiredSetup(t, "linux", "amd64")
	if err := os.MkdirAll(s.stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	home, err := json.Marshal(s.folders.Home)
	if err != nil {
		t.Fatal(err)
	}
	original := []byte("{\n  \"schema\": 1,\n  \"context\": {\n    \"os\": \"linux\",\n    \"arch\": \"amd64\"\n  },\n  \"target\": " + string(home) +
		",\n  \"source\": \"v2\",\n  \"generation\": 3,\n  \"selected\": [],\n  \"keep\": [],\n  \"receipts\": {}\n}\n")
	if err := os.WriteFile(filepath.Join(s.stateDir, state.File), original, 0600); err != nil {
		t.Fatal(err)
	}
	return s, original
}

func runV2(t *testing.T, s setup, run func(context.Context, setup) error, args ...string) (string, error) {
	t.Helper()
	c, err := parseArguments(args)
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	s.command, s.output = c, output
	runErr := run(context.Background(), s)
	data, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data), runErr
}

func TestImportAndRestoreCommandsOfASchema1State(t *testing.T) {
	s, original := importFixture(t)
	file := filepath.Join(s.stateDir, state.File)
	unchanged := func(step string) {
		t.Helper()
		if data, err := os.ReadFile(file); err != nil || string(data) != string(original) {
			t.Fatalf("%s changed the state of release v2: %v", step, err)
		}
	}
	for _, args := range [][]string{{"plan"}, {"apply"}, {"update"}} {
		text, err := runV2(t, s, importV2, args...)
		if err != nil || !strings.Contains(text, "Import from release v2 (fb15ce5, v2.3.0) into release v3.") {
			t.Fatalf("%v: %q %v", args, text, err)
		}
		if (args[0] != "plan") != strings.Contains(text, "Nothing was changed. To import this installation, run the same command with --yes.") {
			t.Fatalf("%v: %q", args, text)
		}
		unchanged(args[0])
	}
	if text, err := runV2(t, s, importV2, "check"); err == nil || !strings.Contains(err.Error(), "runs release v2") || !strings.Contains(text, "Import from release v2") {
		t.Fatalf("check of a schema-1 state: %q %v", text, err)
	}
	if _, err := runV2(t, s, importV2, "apply", "--yes", "--select=zsh"); err == nil || !strings.Contains(err.Error(), "keeps the selection of release v2") {
		t.Fatal(err)
	}
	if _, err := runV2(t, s, restoreV2, "restore-v2", "--yes"); err == nil || !strings.Contains(err.Error(), "nothing to restore") {
		t.Fatal(err)
	}
	unchanged("a refused command")
	text, err := runV2(t, s, importV2, "apply", "--yes")
	if err != nil || !strings.Contains(text, "The import from release v2 is complete.") {
		t.Fatalf("%q %v", text, err)
	}
	st, err := state.Peek(s.stateDir, s.target)
	if err != nil || st.Schema != 2 || st.V2Restore != "open" {
		t.Fatalf("%+v %v", st, err)
	}
	text, err = runV2(t, s, restoreV2, "restore-v2")
	if err != nil || !strings.Contains(text, "Nothing was changed. To restore release v2, run restore-v2 --yes.") {
		t.Fatalf("%q %v", text, err)
	}
	if st, err := state.Peek(s.stateDir, s.target); err != nil || st.V2Restore != "open" {
		t.Fatal("a restore preview changed the state")
	}
	text, err = runV2(t, s, restoreV2, "restore-v2", "--yes")
	if err != nil || !strings.Contains(text, "Check out fb15ce5 (v2.3.0) and run its setup.") {
		t.Fatalf("%q %v", text, err)
	}
	unchanged("restore-v2 must give back the exact state, and it")
	if _, err := state.Peek(s.stateDir, s.target); !errors.Is(err, state.ErrSchema1) {
		t.Fatal(err)
	}
}

func TestWiringKeepsTheLegacyCommandFoldersOfDeferredMacApplications(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses a symbolic link")
	}
	s := wiredSetup(t, "darwin", "arm64")
	if got := newWiring(s).profiles.LegacyCommandDirectories; len(got) != 0 {
		t.Fatalf("a fresh installation has no legacy folder: %v", got)
	}
	payload := filepath.Join(s.stateDir, "packages", "tool.ghostty", "current", "Ghostty.app")
	application := filepath.Join(s.folders.Home, "Applications", "Ghostty.app")
	for _, folder := range []string{payload, filepath.Dir(application)} {
		if err := os.MkdirAll(folder, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(payload, application); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(s.stateDir, s.target)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Update(func(st *state.State) error {
		st.Targets[application] = state.Record{Resource: "integration.ghostty", Kind: "link", Applied: "link:abc", Before: "absent"}
		return nil
	})
	if err = errors.Join(err, store.Close()); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(s.stateDir, "packages", "tool.ghostty", "current", "Ghostty.app", "Contents", "MacOS")}
	if got := newWiring(s).profiles.LegacyCommandDirectories; !slices.Equal(got, want) {
		t.Fatalf("legacy folders: %v, want %v", got, want)
	}
	if err := os.Remove(application); err != nil {
		t.Fatal(err)
	}
	if got := newWiring(s).profiles.LegacyCommandDirectories; len(got) != 0 {
		t.Fatalf("after the release of the legacy link: %v", got)
	}
}
