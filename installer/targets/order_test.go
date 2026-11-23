package targets_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/targets"
)

type fixture struct {
	name          string
	first, second func(e *env) targets.Desired
	outside       func(e *env)
	personal      func(e *env)
	intruder      func(e *env)
}

func source(e *env, name, content string) string {
	directory := filepath.Join(e.home, "source", name)
	write(e.t, filepath.Join(directory, "a.txt"), content, 0644)
	write(e.t, filepath.Join(directory, "sub", "b.txt"), "nested "+content, 0600)
	return directory
}

func replaceWith(e *env, path string, files map[string]string) {
	e.t.Helper()
	if err := os.RemoveAll(path); err != nil {
		e.t.Fatal(err)
	}
	for name, content := range files {
		write(e.t, filepath.Join(path, name), content, 0640)
	}
}

func fixtures() []fixture {
	filePath := func(e *env) string { return filepath.Join(e.root, "app", "settings.conf") }
	treePath := func(e *env) string { return filepath.Join(e.root, "app", "tree") }
	blockPath := func(e *env) string { return filepath.Join(e.root, "shell", ".profile") }
	fieldPath := func(e *env) string { return filepath.Join(e.root, "app", "settings.json") }
	file := func(content string) func(e *env) targets.Desired {
		return func(e *env) targets.Desired {
			return targets.Desired{Resource: "config.test", Kind: "file", Path: filePath(e), Content: []byte(content), Mode: 0644}
		}
	}
	tree := func(name string) func(e *env) targets.Desired {
		return func(e *env) targets.Desired {
			return targets.Desired{Resource: "config.test", Kind: "tree", Path: treePath(e), Tree: source(e, name, name+" content\n")}
		}
	}
	link := func(name string) func(e *env) targets.Desired {
		return func(e *env) targets.Desired {
			return targets.Desired{Resource: "config.test", Kind: "link", Path: treePath(e), Link: source(e, name, name+" content\n")}
		}
	}
	block := func(script string) func(e *env) targets.Desired {
		return func(e *env) targets.Desired {
			return targets.Desired{Resource: "config.test", Kind: "block", Path: blockPath(e), Slot: "shell.test", Content: []byte(script)}
		}
	}
	field := func(value string) func(e *env) targets.Desired {
		return func(e *env) targets.Desired {
			return targets.Desired{Resource: "config.test", Kind: "field", Path: fieldPath(e), Slot: "theme", Content: []byte(value)}
		}
	}
	return []fixture{
		{
			name: "file", first: file("version one\n"), second: file("version two\n"),
			personal: func(e *env) { write(e.t, filePath(e), "personal settings\n", 0640) },
			intruder: func(e *env) { replaceWith(e, filePath(e), map[string]string{"": "intruder\n"}) },
		},
		{
			name: "tree", first: tree("one"), second: tree("two"),
			personal: func(e *env) { replaceWith(e, treePath(e), map[string]string{"mine.txt": "personal tree\n"}) },
			intruder: func(e *env) { replaceWith(e, treePath(e), map[string]string{"other.txt": "intruder\n"}) },
		},
		{
			name: "link", first: link("one"), second: link("two"),
			personal: func(e *env) { replaceWith(e, treePath(e), map[string]string{"mine.txt": "personal tree\n"}) },
			intruder: func(e *env) { replaceWith(e, treePath(e), map[string]string{"": "intruder\n"}) },
		},
		{
			name: "block", first: block("export A=1\n"), second: block("export A=2"),
			outside: func(e *env) { write(e.t, blockPath(e), "# personal line\nalias x=y", 0640) },
			personal: func(e *env) {
				write(e.t, blockPath(e), "# mine\r\n\r\n# >>> dotfiles:shell.test >>>\r\nold body\r\n# <<< dotfiles:shell.test <<<\r\nafter\r\n", 0640)
			},
			intruder: func(e *env) {
				write(e.t, blockPath(e), "# intruder outside\n\n# >>> dotfiles:shell.test >>>\nintruder\n# <<< dotfiles:shell.test <<<\n", 0600)
			},
		},
		{
			name: "field", first: field(`"rose-pine"`), second: field(`"rose-pine-moon"`),
			outside:  func(e *env) { write(e.t, fieldPath(e), `{"model":"personal"}`, 0640) },
			personal: func(e *env) { write(e.t, fieldPath(e), "{\n  \"model\": \"personal\", \"theme\" : \"old\"\n}\n", 0640) },
			intruder: func(e *env) { write(e.t, fieldPath(e), `{"theme":"intruder","other":1}`, 0600) },
		},
	}
}

func prepare(e *env, f fixture, start string) (targets.Desired, targets.Choice, string) {
	e.t.Helper()
	switch start {
	case "personal":
		f.personal(e)
		origin := describe(e.t, e.root)
		if err := e.converge(f.first(e), targets.Preserve); !errors.Is(err, targets.ErrChanged) || describe(e.t, e.root) != origin || len(e.State().Targets) != 0 {
			e.t.Fatal("unrecorded content was not preserved", err)
		}
		return f.first(e), targets.Replace, origin
	case "owned":
		if f.outside != nil {
			f.outside(e)
		}
		origin := describe(e.t, e.root)
		e.mustConverge(f.first(e), targets.Preserve)
		return f.second(e), targets.Preserve, origin
	}
	return f.first(e), targets.Preserve, describe(e.t, e.root)
}

func holdsIntruder(e *env) bool {
	for _, saved := range e.baselines() {
		if strings.Contains(describe(e.t, saved), "intruder") {
			return true
		}
	}
	return false
}

func restored(e *env, f fixture, d targets.Desired, origin string) bool {
	if f.outside == nil {
		return describe(e.t, e.root) == origin
	}
	content := read(e.t, d.Path)
	return strings.Contains(content, "old") && strings.Contains(content, "intruder outside") != strings.Contains(content, "other") && !strings.Contains(content, "rose-pine") && !strings.Contains(content, "export A")
}

func TestInterruptedWriteCompletesOnTheNextRunAndRestoresTheBaseline(t *testing.T) {
	for _, f := range fixtures() {
		for _, start := range []string{"absent", "personal", "owned"} {
			for _, point := range []string{"baseline", "next", "recheck", "move", "publish", "write", "promote"} {
				t.Run(f.name+"/"+start+"/"+point, func(t *testing.T) {
					e := newEnv(t)
					d, choice, origin := prepare(e, f, start)
					seen := interruptAt(point)
					err := e.converge(d, choice)
					fired := slices.Contains(*seen, point)
					if fired != errors.Is(err, errFault) || !fired && err != nil {
						t.Fatal("interruption was not reported", fired, err)
					}
					seen = record()
					if err := e.converge(d, choice); err != nil {
						t.Fatal("the next run did not complete the write", err)
					}
					e.satisfied(d)
					switch {
					case !fired:
					case point == "promote" && len(*seen) != 0:
						t.Fatal("a completed write ran again", *seen)
					case (point == "publish" || point == "write") && !slices.Equal(*seen, []string{"promote"}):
						t.Fatal("a match with the intended value did not complete the write", *seen)
					case point != "publish" && point != "write" && point != "promote" && !slices.Contains(*seen, "write"):
						t.Fatal("the approved write did not run again", *seen)
					}
					targets.SetFault(nil)
					e.mustRelease(d, targets.Preserve)
					if after := describe(t, e.root); after != origin {
						t.Fatalf("the release did not restore the earlier content:\n%s\nwant:\n%s", after, origin)
					}
					if left := e.baselines(); len(left) != 0 {
						t.Fatal("the release left baseline storage", left)
					}
				})
			}
		}
	}
}

func TestInterruptedWriteNeverReplacesContentThatChangedMeanwhile(t *testing.T) {
	for _, f := range fixtures() {
		for _, start := range []string{"absent", "personal", "owned"} {
			for _, point := range []string{"baseline", "next", "recheck"} {
				t.Run(f.name+"/"+start+"/"+point, func(t *testing.T) {
					e := newEnv(t)
					d, choice, origin := prepare(e, f, start)
					seen := interruptAt(point)
					if err := e.converge(d, choice); !slices.Contains(*seen, point) {
						if err != nil {
							t.Fatal(err)
						}
						return
					}
					targets.SetFault(nil)
					f.intruder(e)
					changed := describe(t, e.root)
					if err := e.converge(d, targets.Preserve); !errors.Is(err, targets.ErrChanged) {
						t.Fatal("changed content needs a decision", err)
					}
					if live, err := e.writer.Inspect(d, e); err != nil || live.State != targets.StateChanged {
						t.Fatal("changed content was not reported", live, err)
					}
					if describe(t, e.root) != changed {
						t.Fatal("changed content was replaced without approval")
					}
					e.mustConverge(d, targets.Replace)
					if !holdsIntruder(e) {
						t.Fatal("the replaced content was not saved", e.baselines())
					}
					e.mustRelease(d, targets.Preserve)
					if start == "personal" && !restored(e, f, d, origin) {
						t.Fatal("the first baseline was not restored")
					}
				})
			}
		}
	}
}

func TestInterruptedReleaseCompletesOnTheNextRun(t *testing.T) {
	for _, f := range fixtures() {
		for _, start := range []string{"absent", "personal", "owned"} {
			for _, point := range []string{"release", "move", "publish", "restore", "made"} {
				t.Run(f.name+"/"+start+"/"+point, func(t *testing.T) {
					e := newEnv(t)
					d, choice, origin := prepare(e, f, start)
					e.mustConverge(d, choice)
					seen := interruptAt(point)
					err := e.release(d, targets.Preserve)
					fired := slices.Contains(*seen, point)
					if fired != errors.Is(err, errFault) || !fired && err != nil {
						t.Fatal("interruption was not reported", fired, err)
					}
					if record, has := e.State().Targets[targets.Key(d.Path, d.Slot)]; fired && (!has || !record.Release || record.Applied == "") {
						t.Fatal("the unfinished removal is not in the record", record)
					}
					seen = record()
					e.mustRelease(d, targets.Preserve)
					if fired && (point == "publish" || point == "restore" || point == "made") && slices.Contains(*seen, "restore") {
						t.Fatal("a match with the baseline did not complete the removal", *seen)
					}
					if fired && (point == "release" || point == "move") && start == "personal" && !slices.Contains(*seen, "restore") {
						t.Fatal("the approved restore did not run again", *seen)
					}
					if after := describe(t, e.root); after != origin {
						t.Fatalf("the release did not restore the earlier content:\n%s\nwant:\n%s", after, origin)
					}
					if left := e.baselines(); len(left) != 0 {
						t.Fatal("the release left baseline storage", left)
					}
				})
			}
		}
	}
}

func TestInterruptedReleaseNeverReplacesContentThatChangedMeanwhile(t *testing.T) {
	for _, f := range fixtures() {
		for _, start := range []string{"absent", "personal"} {
			t.Run(f.name+"/"+start, func(t *testing.T) {
				e := newEnv(t)
				d, choice, origin := prepare(e, f, start)
				e.mustConverge(d, choice)
				interruptAt("release")
				if err := e.release(d, targets.Preserve); !errors.Is(err, errFault) {
					t.Fatal(err)
				}
				targets.SetFault(nil)
				f.intruder(e)
				changed := describe(t, e.root)
				if err := e.release(d, targets.Preserve); !errors.Is(err, targets.ErrChanged) || describe(t, e.root) != changed {
					t.Fatal("changed content was not preserved", err)
				}
				if live, err := e.writer.InspectRecord(targets.Key(d.Path, d.Slot), e); err != nil || live.State != targets.StateChanged {
					t.Fatal("changed content was not reported", live, err)
				}
				e.mustRelease(d, targets.Replace)
				if !holdsIntruder(e) {
					t.Fatal("the replaced content was not saved", e.baselines())
				}
				if start == "personal" && !restored(e, f, d, origin) {
					t.Fatal("the first baseline was not restored")
				}
			})
		}
	}
}

func TestContentThatChangesDuringTheWriteStays(t *testing.T) {
	for _, f := range fixtures() {
		for _, start := range []string{"absent", "personal", "owned"} {
			for _, point := range []string{"next", "recheck"} {
				t.Run(f.name+"/"+start+"/"+point, func(t *testing.T) {
					e := newEnv(t)
					d, choice, _ := prepare(e, f, start)
					changed := ""
					targets.SetFault(func(current string) error {
						if current == point && changed == "" {
							f.intruder(e)
							changed = describe(t, e.root)
						}
						return nil
					})
					if err := e.converge(d, choice); !errors.Is(err, targets.ErrChanged) || describe(t, e.root) != changed {
						t.Fatal("content that changed after the approval was replaced", err)
					}
					targets.SetFault(nil)
					if err := e.converge(d, targets.Preserve); !errors.Is(err, targets.ErrChanged) || describe(t, e.root) != changed {
						t.Fatal("the next run replaced changed content", err)
					}
				})
			}
		}
	}
}
