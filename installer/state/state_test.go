package state_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/state"
)

const target = "linux/amd64:/home/fixture"

func open(t *testing.T, dir string) *state.Store {
	t.Helper()
	store, err := state.Open(dir, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func absolute(t *testing.T, elements ...string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join(append([]string{string(filepath.Separator), "fixture"}, elements...)...))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMissingStateIsEmptyAndPeekWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	peeked, err := state.Peek(dir, target)
	if err != nil || peeked.Schema != 0 || peeked.Target != target || peeked.Selected == nil || peeked.System == nil || peeked.Targets == nil {
		t.Fatal(peeked, err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("a read created the state folder", err)
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := open(t, dir)
	config, made := absolute(t, "config", "nvim"), absolute(t, "config")
	err := store.Update(func(s *state.State) error {
		s.Source = "source-hash"
		s.Selected = []string{"zsh", "neovim", "zsh"}
		s.Keep = []string{"tool.node"}
		s.AddSystem("apt", "zsh")
		s.AddSystem("apt", "zsh")
		s.AddSystem("windows", "vcredist")
		s.Mark("tool.zsh:install")
		s.V2Restore = "open"
		if err := s.BeginTarget(config, state.Record{Resource: "config.nvim", Kind: "link", Next: "fp:new", Before: "absent", Made: []string{made}}); err != nil {
			return err
		}
		if err := s.BeginTarget(config+"#block", state.Record{Resource: "profile.zsh", Kind: "block", Next: "fp:block", Before: "value:aGVsbG8="}); err != nil {
			return err
		}
		return s.Promote(config)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := state.State{Schema: 2, Target: target, Source: "source-hash", Selected: []string{"neovim", "zsh"}, Keep: []string{"tool.node"},
		System: map[string][]string{"apt": {"zsh"}, "windows": {"vcredist"}}, Pending: []string{"tool.zsh:install"}, V2Restore: "open",
		Targets: map[string]state.Record{
			config:            {Resource: "config.nvim", Kind: "link", Applied: "fp:new", Before: "absent", Made: []string{made}},
			config + "#block": {Resource: "profile.zsh", Kind: "block", Next: "fp:block", Before: "value:aGVsbG8="},
		}}
	if got := store.View(); !reflect.DeepEqual(got, want) {
		t.Fatalf("view differs\n got %+v\nwant %+v", got, want)
	}
	peeked, err := state.Peek(dir, target)
	if err != nil || !reflect.DeepEqual(peeked, want) {
		t.Fatalf("saved state differs\n got %+v\nwant %+v\n%v", peeked, want, err)
	}
	view := store.View()
	view.Selected[0], view.System["apt"][0] = "changed", "changed"
	view.Targets[config].Made[0] = "changed"
	view.Targets[config] = state.Record{}
	if got := store.View(); !reflect.DeepEqual(got, want) {
		t.Fatal("a view shares memory with the store")
	}
	if !peeked.HasSystem("apt", "zsh") || peeked.HasSystem("brew", "zsh") {
		t.Fatal("system record query is wrong")
	}
}

func TestWriteOrderHelpers(t *testing.T) {
	store := open(t, t.TempDir())
	key, made := absolute(t, "file"), absolute(t)
	update := func(edit func(*state.State) error) {
		t.Helper()
		if err := store.Update(edit); err != nil {
			t.Fatal(err)
		}
	}
	update(func(s *state.State) error {
		return s.BeginTarget(key, state.Record{Resource: "config.git", Kind: "file", Next: "fp:1", Before: "backup:recovery/1", Made: []string{made}})
	})
	update(func(s *state.State) error { return s.Promote(key) })
	update(func(s *state.State) error {
		return s.BeginTarget(key, state.Record{Resource: "config.git", Kind: "file", Next: "fp:2", Before: "absent"})
	})
	record := store.View().Targets[key]
	if record.Applied != "fp:1" || record.Next != "fp:2" || record.Before != "backup:recovery/1" || len(record.Made) != 1 {
		t.Fatal("an update lost the first baseline or the applied value", record)
	}
	update(func(s *state.State) error { return s.Promote(key) })
	update(func(s *state.State) error { return s.BeginRelease(key) })
	record = store.View().Targets[key]
	if !record.Release || record.Next != "backup:recovery/1" || record.Applied != "fp:2" {
		t.Fatal("removal does not restore the first baseline", record)
	}
	if err := store.Update(func(s *state.State) error { return s.Promote(key) }); err == nil {
		t.Fatal("a removal was promoted to an applied value")
	}
	update(func(s *state.State) error {
		return s.BeginTarget(key, state.Record{Resource: "config.git", Kind: "file", Next: "fp:3"})
	})
	if record = store.View().Targets[key]; record.Release || record.Next != "fp:3" {
		t.Fatal("a new write did not end the removal", record)
	}
	update(func(s *state.State) error { s.Drop(key); return nil })
	if len(store.View().Targets) != 0 {
		t.Fatal("record stayed after the drop")
	}
	other := absolute(t, "other")
	update(func(s *state.State) error {
		if err := s.BeginTarget(other, state.Record{Resource: "config.git", Kind: "file", Next: "fp:1"}); err != nil {
			return err
		}
		if err := s.Promote(other); err != nil {
			return err
		}
		return s.BeginRelease(other)
	})
	if record = store.View().Targets[other]; record.Next != "absent" {
		t.Fatal("a target without a baseline is not restored to absence", record)
	}
	for name, edit := range map[string]func(*state.State) error{
		"begin without value": func(s *state.State) error { return s.BeginTarget(key, state.Record{Resource: "a", Kind: "file"}) },
		"promote unknown":     func(s *state.State) error { return s.Promote(key) },
		"release unknown":     func(s *state.State) error { return s.BeginRelease(key) },
		"release without applied": func(s *state.State) error {
			_ = s.BeginTarget(key, state.Record{Resource: "a", Kind: "file", Next: "x"})
			return s.BeginRelease(key)
		},
	} {
		if err := store.Update(edit); err == nil {
			t.Error("accepted", name)
		}
	}
	update(func(s *state.State) error {
		s.Mark("tool.zsh:install")
		s.Mark("tool.zsh:install")
		s.AddSystem("brew", "tmux")
		return nil
	})
	update(func(s *state.State) error {
		s.Clear("tool.zsh:install")
		s.RemoveSystem("brew", "tmux")
		return nil
	})
	if view := store.View(); len(view.Pending) != 0 || len(view.System["brew"]) != 0 {
		t.Fatal("mark or system record stayed", view)
	}
}

func TestStrictDecodeRejectsWithoutAChange(t *testing.T) {
	key := strings.ReplaceAll(absolute(t, "file"), `\`, `\\`)
	valid := `"schema":2,"target":"` + target + `","selected":[],"keep":[],"system":{},"targets":{}`
	for name, document := range map[string]string{
		"unknown field":         `{` + valid + `,"generation":1}`,
		"unknown record field":  `{"schema":2,"target":"` + target + `","selected":[],"keep":[],"system":{},"targets":{"` + key + `":{"resource":"a","kind":"file","applied":"x","owner":"me"}}}`,
		"future schema":         `{"schema":3,"target":"` + target + `"}`,
		"no schema":             `{"target":"` + target + `"}`,
		"trailing content":      `{` + valid + `} {}`,
		"not json":              `schema: 2`,
		"invalid kind":          `{"schema":2,"target":"` + target + `","selected":[],"keep":[],"system":{},"targets":{"` + key + `":{"resource":"a","kind":"socket","applied":"x"}}}`,
		"invalid baseline":      `{"schema":2,"target":"` + target + `","selected":[],"keep":[],"system":{},"targets":{"` + key + `":{"resource":"a","kind":"file","applied":"x","before":"value:%%%"}}}`,
		"unknown baseline":      `{"schema":2,"target":"` + target + `","selected":[],"keep":[],"system":{},"targets":{"` + key + `":{"resource":"a","kind":"file","applied":"x","before":"copy:x"}}}`,
		"release not applied":   `{"schema":2,"target":"` + target + `","selected":[],"keep":[],"system":{},"targets":{"` + key + `":{"resource":"a","kind":"file","next":"absent","release":true}}}`,
		"release without next":  `{"schema":2,"target":"` + target + `","selected":[],"keep":[],"system":{},"targets":{"` + key + `":{"resource":"a","kind":"file","applied":"x","release":true}}}`,
		"empty record":          `{"schema":2,"target":"` + target + `","selected":[],"keep":[],"system":{},"targets":{"` + key + `":{"resource":"a","kind":"file"}}}`,
		"relative key":          `{"schema":2,"target":"` + target + `","selected":[],"keep":[],"system":{},"targets":{"relative/file":{"resource":"a","kind":"file","applied":"x"}}}`,
		"relative made":         `{"schema":2,"target":"` + target + `","selected":[],"keep":[],"system":{},"targets":{"` + key + `":{"resource":"a","kind":"file","applied":"x","made":["relative"]}}}`,
		"invalid resource":      `{"schema":2,"target":"` + target + `","selected":[],"keep":[],"system":{},"targets":{"` + key + `":{"resource":"Not Valid","kind":"file","applied":"x"}}}`,
		"invalid selection":     `{"schema":2,"target":"` + target + `","selected":["Bad ID"],"keep":[],"system":{},"targets":{}}`,
		"invalid provider":      `{"schema":2,"target":"` + target + `","selected":[],"keep":[],"system":{"snap":["x"]},"targets":{}}`,
		"invalid system name":   `{"schema":2,"target":"` + target + `","selected":[],"keep":[],"system":{"apt":["two words"]},"targets":{}}`,
		"invalid pending mark":  `{` + valid + `,"pending":["no-step"]}`,
		"invalid restore value": `{` + valid + `,"v2_restore":"closed"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, state.File)
			if err := os.WriteFile(path, []byte(document), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := state.Peek(dir, target); err == nil || errors.Is(err, state.ErrSchema1) {
				t.Fatal("accepted", err)
			}
			if store, err := state.Open(dir, target); err == nil {
				store.Close()
				t.Fatal("opened")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != document {
				t.Fatal("a rejected state changed", string(data), err)
			}
			store, err := state.Open(filepath.Join(dir, "other"), target)
			if err != nil {
				t.Fatal("a rejected open kept the lock or blocked another folder", err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSchema1IsDetectedAndPreserved(t *testing.T) {
	dir := t.TempDir()
	document := `{"schema":1,"context":{"os":"linux","arch":"amd64"},"target":"/home/fixture","source":"x","generation":4,"selected":["neovim"],"keep":[],"receipts":{}}`
	path := filepath.Join(dir, state.File)
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Peek(dir, target); !errors.Is(err, state.ErrSchema1) {
		t.Fatal("schema 1 was not detected", err)
	}
	if _, err := state.Open(dir, target); !errors.Is(err, state.ErrSchema1) {
		t.Fatal("schema 1 was not detected by a mutating open", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != document {
		t.Fatal("schema 1 state changed", err)
	}
	store, err := state.Open(dir, target)
	if !errors.Is(err, state.ErrSchema1) || store != nil {
		t.Fatal("the lock of the failed open was not released", err)
	}
}

func TestTargetMismatchStops(t *testing.T) {
	dir := t.TempDir()
	store := open(t, dir)
	if err := store.Update(func(s *state.State) error { s.Selected = []string{"neovim"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(s *state.State) error { s.Target = "linux/arm64:/elsewhere"; return nil }); err != nil {
		t.Fatal(err)
	}
	if store.View().Target != target {
		t.Fatal("an edit changed the target")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"linux/arm64:/home/fixture", "linux/amd64:/home/other", "darwin/arm64:/home/fixture"} {
		if _, err := state.Peek(dir, other); err == nil || !strings.Contains(err.Error(), "state target differs") {
			t.Fatal("accepted the state of another target", other, err)
		}
		if _, err := state.Open(dir, other); err == nil {
			t.Fatal("opened the state of another target", other)
		}
	}
}

func TestFailedEditLeavesTheFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	store := open(t, dir)
	if err := store.Update(func(s *state.State) error { s.Selected = []string{"neovim"}; return nil }); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, state.File)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("edit failed")
	if err := store.Update(func(s *state.State) error { s.Selected = []string{"zsh"}; return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*state.State) error{
		"invalid id":       func(s *state.State) error { s.Keep = []string{"Not Valid"}; return nil },
		"invalid provider": func(s *state.State) error { s.AddSystem("snap", "x"); return nil },
		"invalid mark":     func(s *state.State) error { s.Mark("nostep"); return nil },
		"invalid record": func(s *state.State) error {
			s.Targets["relative"] = state.Record{Resource: "a", Kind: "file", Applied: "x"}
			return nil
		},
	} {
		if err := store.Update(edit); err == nil {
			t.Error("published", name)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("a failed edit changed the file", string(after), err)
	}
	if view := store.View(); len(view.Selected) != 1 || view.Selected[0] != "neovim" || len(view.Keep) != 0 || len(view.Targets) != 0 {
		t.Fatal("a failed edit changed the view", view)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatal("a failed edit left a file", entries, err)
	}
}

func TestOpenExcludesASecondOpen(t *testing.T) {
	dir := t.TempDir()
	first, err := state.Open(dir, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.Open(dir, target); err == nil || !strings.Contains(err.Error(), "another installer holds the mutation lock") {
		t.Fatal("a second mutating run was admitted", err)
	}
	if _, err := state.Peek(dir, target); err != nil {
		t.Fatal("a read needs the lock", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, state.LockFile)); err != nil {
		t.Fatal("the lock does not use the v2 file name", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal("a second close failed", err)
	}
	second, err := state.Open(dir, target)
	if err != nil {
		t.Fatal("the lock was not released", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
