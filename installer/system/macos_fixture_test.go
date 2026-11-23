package system

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/state"
)

type macTestRecords struct{ st state.State }

func newMacTestRecords() *macTestRecords {
	return &macTestRecords{st: state.State{Schema: 2, Target: "darwin/arm64:/Users/fixture", Selected: []string{}, Keep: []string{}, System: map[string][]string{}, Targets: map[string]state.Record{}}}
}

func (r *macTestRecords) State() state.State { return r.st.Clone() }

func (r *macTestRecords) Update(edit func(*state.State) error) error {
	next := r.st.Clone()
	if err := edit(&next); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	r.st = next
	return nil
}

func macTestExit(code int) error {
	return &platform.CommandError{Message: "fixture native failure", ExitCode: &code}
}

type macTestRunner struct {
	commands []platform.Command
	inputs   []string
	run      func(c platform.Command, input string) ([]byte, error)
}

func (r *macTestRunner) Run(_ context.Context, c platform.Command) ([]byte, error) {
	input := ""
	if c.Stdin != nil {
		data, err := io.ReadAll(c.Stdin)
		if err != nil {
			return nil, err
		}
		input = string(data)
	}
	r.commands, r.inputs = append(r.commands, c), append(r.inputs, input)
	if r.run == nil {
		return nil, nil
	}
	return r.run(c, input)
}

func macTestSkipWithoutPOSIXPaths(t *testing.T) {
	t.Helper()
	if os.PathSeparator != '/' {
		t.Skip("the macOS providers use POSIX paths")
	}
}

func macTestLine(c platform.Command) string {
	return strings.Join(slices.Concat(c.Env, []string{c.Path}, c.Args), " ")
}

func macTestRoot(t *testing.T) string {
	t.Helper()
	macTestSkipWithoutPOSIXPaths(t)
	root, err := platform.ResolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func macTestWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
