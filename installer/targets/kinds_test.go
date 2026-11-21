package targets_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

func utf16Bytes(order binary.ByteOrder, text string) []byte {
	units := utf16.Encode([]rune(text))
	out := make([]byte, 2*len(units))
	for i, unit := range units {
		order.PutUint16(out[2*i:], unit)
	}
	return out
}

func encoders() map[string]func(string) []byte {
	return map[string]func(string) []byte{
		"":             func(s string) []byte { return []byte(s) },
		"\xef\xbb\xbf": func(s string) []byte { return []byte(s) },
		"\xff\xfe":     func(s string) []byte { return utf16Bytes(binary.LittleEndian, s) },
		"\xfe\xff":     func(s string) []byte { return utf16Bytes(binary.BigEndian, s) },
	}
}

func blockOf(slot, script string) targets.Desired {
	return targets.Desired{Kind: "block", Slot: slot, Content: []byte(script)}
}

func installBlock(t *testing.T, data []byte, d targets.Desired) []byte {
	t.Helper()
	value, err := targets.Block{}.Render(d)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := targets.Block{}.Replace(data, d, value, true)
	if err != nil {
		t.Fatal(err)
	}
	found, present, err := targets.Block{}.Extract(installed, d)
	if err != nil || !present || !bytes.Equal(found, value) {
		t.Fatalf("the block does not read back as written: %q %v %v", found, present, err)
	}
	return installed
}

func TestBlockRoundTripPreservesEncodingAndAllOutsideBytes(t *testing.T) {
	originals := [][]byte{nil, []byte("# personal"), []byte("# café\n"), []byte("# personal\r\n"), {0xef, 0xbb, 0xbf, '#', ' ', 'x'}}
	for _, bom := range []string{"\xff\xfe", "\xfe\xff"} {
		originals = append(originals, append([]byte(bom), encoders()[bom]("# café 🌍\r\n")...))
	}
	for _, original := range originals {
		d := blockOf("integration.starship", "# prompt\necho 'ready'")
		if _, present, err := (targets.Block{}).Extract(original, d); err != nil || present {
			t.Fatal("an absent block was reported", present, err)
		}
		installed := installBlock(t, original, d)
		if again := installBlock(t, installed, d); !bytes.Equal(again, installed) {
			t.Fatal("repeat changed the profile")
		}
		restored, err := targets.Block{}.Replace(installed, d, nil, false)
		if err != nil || !bytes.Equal(restored, original) {
			t.Fatalf("round trip changed bytes: %x / %x: %v", original, restored, err)
		}
	}
}

func TestBlockKeepsTheLineEndingsOfTheFileAndOfAnExistingBlock(t *testing.T) {
	d := blockOf("profile.test", "one\ntwo")
	windows := installBlock(t, []byte("# personal\r\n"), d)
	if want := "# personal\r\n\r\n# >>> dotfiles:profile.test >>>\r\none\r\ntwo\r\n# <<< dotfiles:profile.test <<<\r\n"; string(windows) != want {
		t.Fatalf("got %q", windows)
	}
	mixed := "# personal\r\n\n# >>> dotfiles:profile.test >>>\nold\n# <<< dotfiles:profile.test <<<\n# later\r\n"
	updated := installBlock(t, []byte(mixed), d)
	if want := strings.Replace(mixed, "old", "one\ntwo", 1); string(updated) != want {
		t.Fatalf("got %q, want %q", updated, want)
	}
	top := targets.Block{Top: true}
	value, err := top.Render(d)
	if err != nil {
		t.Fatal(err)
	}
	first, err := top.Replace([]byte("\xef\xbb\xbf[user]\n"), d, value, true)
	if err != nil || !bytes.HasPrefix(first, []byte("\xef\xbb\xbf\n# >>> dotfiles:profile.test >>>\n")) || !bytes.HasSuffix(first, []byte("<<<\n[user]\n")) {
		t.Fatalf("a top block must follow the byte order mark: %q %v", first, err)
	}
}

func TestBlockRejectsMalformedOrAmbiguousInput(t *testing.T) {
	d := blockOf("test", "x")
	for _, data := range [][]byte{{0xff}, {0xff, 0xfe, 0}, {0xff, 0xfe, 0, 0xd8}, []byte("x\x00y"), []byte("# >>> dotfiles:test >>>\n"), []byte("\n# >>> dotfiles:test >>>\nx\n# <<< dotfiles:test <<<\n\n# >>> dotfiles:test >>>\nx\n# <<< dotfiles:test <<<\n")} {
		if _, err := (targets.Block{}).Replace(data, d, nil, false); err == nil {
			t.Fatalf("accepted malformed input: %x", data)
		}
		if _, _, err := (targets.Block{}).Extract(data, d); err == nil {
			t.Fatalf("read malformed input: %x", data)
		}
	}
	for _, script := range []string{"a\r\nb", "a\x00", "# >>> dotfiles:other >>>", "# <<< dotfiles:test <<<"} {
		if _, err := (targets.Block{}).Render(blockOf("test", script)); err == nil {
			t.Fatalf("accepted the script %q", script)
		}
	}
	if _, err := (targets.Block{}).Render(blockOf("Not An ID", "x")); err == nil {
		t.Fatal("accepted an invalid block identity")
	}
}

func TestBlockOpeningTheFileMayLoseOnlyItsLeadingNewline(t *testing.T) {
	d := blockOf("profile.test", "# script")
	want, err := targets.Block{}.Render(d)
	if err != nil {
		t.Fatal(err)
	}
	for bom, encode := range encoders() {
		for _, nl := range []string{"\n", "\r\n"} {
			opening := encode("# >>> dotfiles:profile.test >>>" + nl + "# script" + nl + "# <<< dotfiles:profile.test <<<" + nl)
			personal := encode("# personal" + nl)
			formatted := append(append([]byte(bom), opening...), personal...)
			value, present, err := targets.Block{}.Extract(formatted, d)
			if err != nil || !present || !bytes.Equal(value, want) {
				t.Fatalf("%x %q: opening block was not recognized as written: %q %v", bom, nl, value, err)
			}
			content, err := targets.Block{}.Replace(formatted, d, nil, false)
			if err != nil || !bytes.Equal(content, append([]byte(bom), personal...)) {
				t.Fatalf("%x %q: removal changed outside bytes: %q %v", bom, nl, content, err)
			}
			edited := append(append(append([]byte(bom), encode("x")...), opening...), personal...)
			if _, err := (targets.Block{}).Replace(edited, d, nil, false); err == nil || !strings.Contains(err.Error(), "markers were edited") {
				t.Fatalf("%x %q: text before the opening marker was accepted: %v", bom, nl, err)
			}
		}
	}
}

func fieldOf(value string) targets.Desired {
	return targets.Desired{Kind: "field", Slot: "theme", Content: []byte(value)}
}

func TestFieldPublicationPreservesUnrelatedBytesAndRestoresPriorValue(t *testing.T) {
	for _, jsonc := range []bool{false, true} {
		field := targets.Field{JSONC: func(string) bool { return jsonc }}
		for _, original := range []string{
			"{}", "{\r\n  \r\n}", "{ \"model\" : {\"limit\":9007199254740993}, \"items\" : [1, 2] }\r\n",
			"{\"theme\":\"old\", \"model\":\"personal\"}", "{\"model\":\"personal\", \"theme\" : \"old\"}",
			"{\"theme\":\"\\u006fld\"}",
		} {
			d := fieldOf(`"rose-pine"`)
			before, present, err := field.Extract([]byte(original), d)
			if err != nil {
				t.Fatal(original, err)
			}
			value, err := field.Render(d)
			if err != nil {
				t.Fatal(err)
			}
			installed, err := field.Replace([]byte(original), d, value, true)
			if err != nil {
				t.Fatal(original, err)
			}
			if found, _, err := field.Extract(installed, d); err != nil || string(found) != `"rose-pine"` {
				t.Fatal("the field does not read back", string(found), err)
			}
			repeated, err := field.Replace(installed, d, value, true)
			if err != nil || !bytes.Equal(installed, repeated) {
				t.Fatal("repeated publication changed settings", err)
			}
			restored, err := field.Replace(installed, d, before, present)
			if err != nil || string(restored) != original {
				t.Fatalf("original bytes changed: %q -> %q: %v", original, restored, err)
			}
		}
	}
}

func TestFieldRemovalPreservesLaterPersonalFields(t *testing.T) {
	for _, current := range []string{`{"theme":"rose-pine","model":"later"}`, `{"model":"later","theme":"rose-pine"}`, `{"first":1,"theme":"rose-pine","last":2}`} {
		removed, err := targets.Field{}.Replace([]byte(current), fieldOf(""), nil, false)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]json.RawMessage
		if err := json.Unmarshal(removed, &values); err != nil || values["theme"] != nil || len(values) == 0 {
			t.Fatal("removal damaged unrelated fields", string(removed), err)
		}
	}
}

func TestFieldRejectsAmbiguousAndOutOfScopeInputs(t *testing.T) {
	value := []byte(`"rose-pine"`)
	for _, data := range []string{"[]", "null", "42", `{\"theme\":`, `{"theme":"a","theme":"b"}`, "{} {}", "{\"theme\":\"\xff\"}", "{\"theme\":\"x\",}", "{ /*comment*/ }"} {
		if _, err := (targets.Field{}).Replace([]byte(data), fieldOf(""), value, true); err == nil {
			t.Fatalf("accepted malformed settings %q", data)
		}
		if _, _, err := (targets.Field{}).Extract([]byte(data), fieldOf("")); err == nil {
			t.Fatalf("read malformed settings %q", data)
		}
	}
	for _, desired := range []string{"", "1 2", `{"theme":`, "// comment\n1"} {
		if _, err := (targets.Field{}).Render(fieldOf(desired)); err == nil {
			t.Fatalf("accepted the value %q", desired)
		}
	}
	if _, err := (targets.Field{}).Render(targets.Desired{Slot: "a\nb", Content: value}); err == nil {
		t.Fatal("accepted an invalid field name")
	}
	if _, _, err := (targets.Field{}).Extract(nil, fieldOf("")); err == nil || !strings.Contains(err.Error(), "existing JSON settings are empty") {
		t.Fatal("an empty settings file must stop", err)
	}
}

func jsonc() targets.Field { return targets.Field{JSONC: func(string) bool { return true }} }

func TestJSONCFieldRemovalPreservesAdjacentComments(t *testing.T) {
	for _, original := range []string{
		`{/*before*/"theme":"old"/*after*/,/*next*/"keep":1,/*tail*/}`,
		`{"keep":1,/*before*/"theme":"old"/*after*/,/*tail*/}`,
		`{/*before*/"theme":"old"/*after*/,/*tail*/}`,
		`{/*before*/"theme":"old"/*after*/}`,
	} {
		result, err := jsonc().Replace([]byte(original), fieldOf(""), nil, false)
		if err != nil {
			t.Fatal(original, err)
		}
		for _, comment := range []string{"/*before*/", "/*after*/", "/*next*/", "/*tail*/"} {
			if strings.Contains(original, comment) && !bytes.Contains(result, []byte(comment)) {
				t.Fatal("lost personal comment", string(result))
			}
		}
		if _, present, err := jsonc().Extract(result, fieldOf("")); err != nil || present {
			t.Fatal("the field stayed", string(result), err)
		}
	}
}

func TestJSONCRejectsAmbiguousAndMalformedSettings(t *testing.T) {
	for _, data := range []string{`{,}`, `{"array":[,]}`, `{"theme":1,"theme":2}`, `{"theme":1,,}`, `{"a":1}true`, `/*open`, `{"a":"bad\"}`, "// invalid \xff\n{}", "{}\x00", `[]`} {
		if _, err := jsonc().Replace([]byte(data), fieldOf(""), []byte(`"new"`), true); err == nil {
			t.Errorf("accepted %q", data)
		}
	}
}

func TestJSONCEmptyDocumentKeepsAPersonalComment(t *testing.T) {
	for data, want := range map[string]bool{"{}\n": true, "{ }": true, "{} // mine\n": false, `{"a":1}`: false} {
		if empty, err := jsonc().Empty([]byte(data), fieldOf("")); err != nil || empty != want {
			t.Fatal(data, empty, err)
		}
	}
	if empty, err := (targets.Field{}).Empty([]byte(`{"a":1}`), fieldOf("")); err != nil || empty {
		t.Fatal(empty, err)
	}
}

func TestJSONCLifecyclePreservesPersonalCommentsAndFields(t *testing.T) {
	for _, prior := range []string{"", `"workbench.colorTheme" : "personal", /* after theme */`} {
		t.Run(prior, func(t *testing.T) {
			e := newEnv(t)
			path := filepath.Join(e.root, "Code", "settings.json")
			e.writer.Kinds = map[string]targets.Kind{"field": targets.SlotKind("field", targets.Field{JSONC: func(file string) bool { return file == path }})}
			original := "\xef\xbb\xbf// personal header\r\n{\r\n" + prior + "\r\n" +
				`"url":"https://example.org/a/*b*/", // inline` + "\r\n" +
				`"nested":{"workbench.colorTheme":"keep",},"huge":9007199254740993,` + "\r\n}\r\n"
			write(t, path, original, 0600)
			d := targets.Desired{Resource: "integration.vscode", Kind: "field", Path: path, Slot: "workbench.colorTheme", Content: []byte(`"Rosé Pine"`)}
			choice := targets.Preserve
			if prior != "" {
				if err := e.converge(d, targets.Preserve); !errors.Is(err, targets.ErrChanged) || read(t, path) != original {
					t.Fatal("a personal theme must need a decision", err)
				}
				choice = targets.Replace
			}
			e.mustConverge(d, choice)
			if data := read(t, path); !strings.Contains(data, `"workbench.colorTheme":"Rosé Pine"`) && !strings.Contains(data, `"workbench.colorTheme" : "Rosé Pine"`) {
				t.Fatal("desired theme missing", data)
			}
			write(t, path, strings.Replace(read(t, path), "9007199254740993", "9007199254740995", 1), 0600)
			d.Content = []byte(`"Rosé Pine Moon"`)
			e.mustConverge(d, targets.Preserve)
			e.mustRelease(d, targets.Preserve)
			if want := strings.Replace(original, "9007199254740993", "9007199254740995", 1); read(t, path) != want {
				t.Fatalf("personal JSONC changed: got %q, want %q", read(t, path), want)
			}
		})
	}
}

func TestNewBlockFileGetsItsByteOrderMarkAndRemovalDeletesOnlyANewEmptyFile(t *testing.T) {
	for _, later := range []string{"", "# added later\n"} {
		e := newEnv(t)
		path := filepath.Join(e.root, "PowerShell", "profile with space.ps1")
		d := targets.Desired{Resource: "profile.test", Kind: "block", Path: path, Slot: "profile.test", Content: []byte("# first version"), BOM: true}
		e.mustConverge(d, targets.Preserve)
		if want := "\xef\xbb\xbf\n# >>> dotfiles:profile.test >>>\n# first version\n# <<< dotfiles:profile.test <<<\n"; read(t, path) != want {
			t.Fatalf("got %q", read(t, path))
		}
		if record := e.State().Targets[targets.Key(path, "profile.test")]; record.Before != targets.Absent || len(record.Made) != 3 || record.Made[0] != path {
			t.Fatal("the record does not name what setup created", record)
		}
		write(t, path, read(t, path)+later, 0600)
		e.mustRelease(d, targets.Preserve)
		if later == "" {
			if _, err := os.Lstat(e.root); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("removal left the file or the folders that setup created", err)
			}
		} else if read(t, path) != "\xef\xbb\xbf"+later {
			t.Fatalf("removal changed personal content: %q", read(t, path))
		}
	}
	e := newEnv(t)
	path := filepath.Join(e.root, ".profile")
	write(t, path, "", 0600)
	d := targets.Desired{Resource: "profile.test", Kind: "block", Path: path, Slot: "profile.test", Content: []byte("x")}
	e.mustConverge(d, targets.Preserve)
	e.mustRelease(d, targets.Preserve)
	if read(t, path) != "" {
		t.Fatal("removal deleted or changed a file that existed before")
	}
}

func TestStagedNewByteOrderMarkFileRecoversWithoutTheSetting(t *testing.T) {
	for _, release := range []bool{true, false} {
		e := newEnv(t)
		path := filepath.Join(e.root, "PowerShell", "profile.ps1")
		d := targets.Desired{Resource: "profile.test", Kind: "block", Path: path, Slot: "profile.test", Content: []byte("# first version"), BOM: true}
		staged, _ := targets.Siblings(d)
		targets.SetFault(func(point string) error {
			if point != "write" {
				return nil
			}
			if err := os.Rename(path, staged); err != nil {
				t.Fatal(err)
			}
			return errFault
		})
		if err := e.converge(d, targets.Preserve); !errors.Is(err, errFault) {
			t.Fatal(err)
		}
		targets.SetFault(nil)
		if !strings.HasPrefix(read(t, staged), "\xef\xbb\xbf") {
			t.Fatalf("the fixture needs a staged file with a byte order mark: %q", read(t, staged))
		}
		if release {
			e.mustRelease(d, targets.Preserve)
			if _, err := os.Lstat(e.root); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("the release left the staged file or the folders that setup created", err)
			}
			continue
		}
		e.mustConverge(d, targets.Preserve)
		if _, err := os.Lstat(staged); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the retry left the staged file", err)
		}
		if want := "\xef\xbb\xbf\n# >>> dotfiles:profile.test >>>\n# first version\n# <<< dotfiles:profile.test <<<\n"; read(t, path) != want {
			t.Fatalf("got %q", read(t, path))
		}
	}
}

func TestSlotWriteKeepsTheModeOfAReadOnlyFile(t *testing.T) {
	e := newEnv(t)
	path := filepath.Join(e.root, ".profile")
	write(t, path, "# personal\n", 0400)
	d := targets.Desired{Resource: "profile.test", Kind: "block", Path: path, Slot: "profile.test", Content: []byte("x")}
	e.mustConverge(d, targets.Preserve)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0200 != 0 || !strings.Contains(read(t, path), "dotfiles:profile.test") {
		t.Fatal("publication changed the read-only mode", info.Mode(), err)
	}
	e.mustRelease(d, targets.Preserve)
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0200 != 0 || read(t, path) != "# personal\n" {
		t.Fatal("removal changed the read-only file", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestUnchangedTargetIsNotWrittenAgain(t *testing.T) {
	for _, f := range fixtures() {
		e := newEnv(t)
		if f.outside != nil {
			f.outside(e)
		}
		d := f.first(e)
		e.mustConverge(d, targets.Preserve)
		before, err := os.Lstat(d.Path)
		if err != nil {
			t.Fatal(err)
		}
		seen := record()
		e.mustConverge(d, targets.Preserve)
		after, err := os.Lstat(d.Path)
		if err != nil || !os.SameFile(before, after) || len(*seen) != 0 {
			t.Fatal(f.name, "an unchanged target was written again", *seen, err)
		}
	}
}

func TestEditedBlockMarkersNameTheirFileAndStayUnread(t *testing.T) {
	e := newEnv(t)
	path := filepath.Join(e.root, ".profile")
	d := targets.Desired{Resource: "profile.test", Kind: "block", Path: path, Slot: "profile.test", Content: []byte("x")}
	e.mustConverge(d, targets.Preserve)
	damaged := strings.Replace(read(t, path), "# <<< dotfiles:profile.test <<<", "# edited", 1)
	write(t, path, damaged, 0600)
	live, err := e.writer.Inspect(d, e)
	if err != nil || live.State != targets.StateUnreadable || !strings.Contains(live.Detail, path) {
		t.Fatal("damaged markers must be unreadable and name the file", live, err)
	}
	for _, change := range []func() error{func() error { return e.converge(d, targets.Replace) }, func() error { return e.release(d, targets.Replace) }} {
		if err := change(); err == nil || !strings.Contains(err.Error(), path) || read(t, path) != damaged {
			t.Fatal("an unreadable file was changed", err)
		}
	}
}

func TestBaselineWithoutAnAppliedValueGivesNoOwnership(t *testing.T) {
	for _, f := range fixtures() {
		t.Run(f.name, func(t *testing.T) {
			e := newEnv(t)
			d, choice, origin := prepare(e, f, "personal")
			interruptAt("baseline")
			if err := e.converge(d, choice); !errors.Is(err, errFault) {
				t.Fatal(err)
			}
			targets.SetFault(nil)
			record := e.State().Targets[targets.Key(d.Path, d.Slot)]
			if record.Applied != "" || record.Next != "" || record.Before == "" || record.Before == targets.Absent {
				t.Fatal("expected a record with a baseline only", record)
			}
			if live, err := e.writer.Inspect(d, e); err != nil || live.State != targets.StateChanged || live.Satisfied {
				t.Fatal("a different desired content against a baseline only is changed content", live, err)
			}
			if live, err := e.writer.InspectRecord(targets.Key(d.Path, d.Slot), e); err != nil || live.State != targets.StateBefore {
				t.Fatal(live, err)
			}
			e.mustRelease(d, targets.Preserve)
			if describe(t, e.root) != origin || len(e.baselines()) != 0 {
				t.Fatal("a release without ownership changed the destination or left storage")
			}
		})
	}
}

func TestContentThatEqualsTheDesiredContentGivesNoOwnership(t *testing.T) {
	for _, f := range fixtures() {
		t.Run(f.name, func(t *testing.T) {
			e := newEnv(t)
			if f.outside != nil {
				f.outside(e)
			}
			d := f.first(e)
			e.mustConverge(d, targets.Preserve)
			if err := e.Update(func(s *state.State) error { s.Drop(targets.Key(d.Path, d.Slot)); return nil }); err != nil {
				t.Fatal(err)
			}
			equal := describe(t, e.root)
			live, err := e.writer.Inspect(d, e)
			if err != nil || live.State != targets.StateChanged || live.Fingerprint != live.Want {
				t.Fatal("equal content without a record must not look owned", live, err)
			}
			if err := e.converge(d, targets.Preserve); err != nil || describe(t, e.root) != equal {
				t.Fatal("equal content was changed", err)
			}
			e.unowned(d)
			if live, err := e.writer.Inspect(d, e); err != nil || !live.Satisfied || live.State != targets.StateBefore {
				t.Fatal("equal content that setup does not own needs no action", live, err)
			}
			e.mustRelease(d, targets.Preserve)
			if describe(t, e.root) != equal || len(e.baselines()) != 0 {
				t.Fatal("the release removed content that setup did not own")
			}
		})
	}
}

func TestAbsentEmptyAndUnreadableAreDistinct(t *testing.T) {
	e := newEnv(t)
	path := filepath.Join(e.root, "settings.conf")
	d := targets.Desired{Resource: "config.test", Kind: "file", Path: path, Content: []byte("x")}
	if live, err := e.writer.Inspect(d, e); err != nil || live.State != targets.StateAbsent || live.Fingerprint != targets.Absent {
		t.Fatal(live, err)
	}
	write(t, path, "", 0600)
	if live, err := e.writer.Inspect(d, e); err != nil || live.State != targets.StateChanged || live.Fingerprint == targets.Absent {
		t.Fatal("an empty file is not absent", live, err)
	}
	if err := e.converge(d, targets.Preserve); !errors.Is(err, targets.ErrChanged) {
		t.Fatal(err)
	}
	e.mustConverge(d, targets.Replace)
	e.mustRelease(d, targets.Preserve)
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatal("the empty file was not restored", err)
	}

	settings := filepath.Join(e.root, "settings.json")
	field := targets.Desired{Resource: "config.test", Kind: "field", Path: settings, Slot: "theme", Content: []byte(`"x"`)}
	write(t, settings, `{"theme":""}`, 0600)
	if live, err := e.writer.Inspect(field, e); err != nil || live.State != targets.StateChanged {
		t.Fatal("an empty value is not absent", live, err)
	}
	e.mustConverge(field, targets.Replace)
	e.mustRelease(field, targets.Preserve)
	if read(t, settings) != `{"theme":""}` {
		t.Fatal("the empty value was not restored", read(t, settings))
	}
	for _, unreadable := range []string{"", "{broken", `{"theme":1,"theme":2}`} {
		write(t, settings, unreadable, 0600)
		live, err := e.writer.Inspect(field, e)
		if err != nil || live.State != targets.StateUnreadable || live.Detail == "" {
			t.Fatal("unreadable settings must not look absent", live, err)
		}
		if err := e.converge(field, targets.Replace); err == nil || read(t, settings) != unreadable || len(e.State().Targets) != 0 {
			t.Fatal("unreadable settings were changed", err)
		}
	}
}

func TestRedirectedDestinationStopsTheWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses a symbolic link")
	}
	e := newEnv(t)
	d := fixtures()[0].first(e)
	e.mustConverge(d, targets.Preserve)
	elsewhere := filepath.Join(e.home, "elsewhere")
	if err := os.Rename(filepath.Dir(d.Path), elsewhere); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Dir(d.Path)); err != nil {
		t.Fatal(err)
	}
	before := describe(t, elsewhere)
	if live, err := e.writer.Inspect(d, e); err != nil || live.State != targets.StateUnreadable || !strings.Contains(live.Detail, "redirected") {
		t.Fatal("a redirected destination needs a decision", live, err)
	}
	d.Content = []byte("new")
	for _, change := range []func() error{func() error { return e.converge(d, targets.Replace) }, func() error { return e.release(d, targets.Replace) }} {
		if err := change(); err == nil || !strings.Contains(err.Error(), "redirected") || describe(t, elsewhere) != before {
			t.Fatal("setup wrote through a redirected parent", err)
		}
	}
}

func TestLinkBaselineRestoresTheLinkOfTheOwnerWithoutReadingItsTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture uses a symbolic link")
	}
	e := newEnv(t)
	d := fixtures()[2].first(e)
	if err := os.MkdirAll(filepath.Dir(d.Path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.home, "missing", "personal"), d.Path); err != nil {
		t.Fatal(err)
	}
	origin := describe(t, e.root)
	e.mustConverge(d, targets.Replace)
	if target, err := os.Readlink(d.Path); err != nil || target != d.Link {
		t.Fatal(target, err)
	}
	live, err := e.writer.Inspect(d, e)
	write(t, filepath.Join(d.Link, "edited through the link"), "live", 0600)
	if after, _ := e.writer.Inspect(d, e); err != nil || after != live {
		t.Fatal("an edit through a live link changed its fingerprint", live, after, err)
	}
	e.mustRelease(d, targets.Preserve)
	if describe(t, e.root) != origin || len(e.baselines()) != 0 {
		t.Fatal("the link of the owner was not restored")
	}
}

type upperSlot struct{ targets.Field }

func (s upperSlot) Render(d targets.Desired) ([]byte, error) {
	return s.Field.Render(targets.Desired{Slot: d.Slot, Content: bytes.ToUpper(d.Content)})
}

func TestWriterUsesAddedKindsForWritesAndReleases(t *testing.T) {
	e := newEnv(t)
	e.writer.Kinds = map[string]targets.Kind{"font": targets.FileKind("font"), "field#projection": targets.SlotKind("field", upperSlot{})}
	font := targets.Desired{Resource: "tool.font", Kind: "font", Path: filepath.Join(e.root, "fonts", "Test#1.ttf"), Content: []byte("font bytes")}
	e.mustConverge(font, targets.Preserve)
	if record := e.State().Targets[font.Path]; record.Kind != "font" || read(t, font.Path) != "font bytes" {
		t.Fatal(record)
	}
	settings := filepath.Join(e.root, "settings.json")
	write(t, settings, `{"keep":1}`, 0600)
	projection := targets.Desired{Resource: "desktop.windows-terminal", Kind: "field", Path: settings, Slot: "projection", Content: []byte(`"value"`)}
	plain := targets.Desired{Resource: "desktop.windows-terminal", Kind: "field", Path: settings, Slot: "plain", Content: []byte(`"value"`)}
	e.mustConverge(projection, targets.Preserve)
	e.mustConverge(plain, targets.Preserve)
	if read(t, settings) != `{"keep":1,"projection":"VALUE","plain":"value"}` {
		t.Fatal(read(t, settings))
	}
	e.mustRelease(projection, targets.Preserve)
	e.mustRelease(plain, targets.Preserve)
	e.mustRelease(font, targets.Preserve)
	if read(t, settings) != `{"keep":1}` {
		t.Fatal(read(t, settings))
	}
	if _, err := os.Lstat(filepath.Join(e.root, "fonts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the font and its folder stayed", err)
	}
}

func TestWriterRefusesInvalidDescriptionsAndForeignRecords(t *testing.T) {
	e := newEnv(t)
	d := fixtures()[0].first(e)
	e.mustConverge(d, targets.Preserve)
	foreign := d
	foreign.Resource = "config.other"
	if err := e.converge(foreign, targets.Replace); err == nil || !strings.Contains(err.Error(), "belongs to config.test") {
		t.Fatal("a target of another resource was accepted", err)
	}
	for name, invalid := range map[string]targets.Desired{
		"relative path":  {Resource: "config.test", Kind: "file", Path: "relative"},
		"unknown kind":   {Resource: "config.test", Kind: "socket", Path: d.Path},
		"slot on file":   {Resource: "config.test", Kind: "file", Path: d.Path, Slot: "x"},
		"block no slot":  {Resource: "config.test", Kind: "block", Path: d.Path},
		"wrong key":      {Resource: "config.test", Kind: "file", Path: d.Path, Key: d.Path + "x"},
		"tree no source": {Resource: "config.test", Kind: "tree", Path: d.Path + "2"},
		"relative link":  {Resource: "config.test", Kind: "link", Path: d.Path + "3", Link: "relative"},
	} {
		if _, err := e.writer.Inspect(invalid, e); err == nil {
			t.Fatal("inspected an invalid description:", name)
		}
		if err := e.converge(invalid, targets.Replace); err == nil {
			t.Fatal("wrote an invalid description:", name)
		}
	}
	if err := e.writer.Release(t.Context(), filepath.Join(e.root, "no record"), e, targets.Replace); err != nil {
		t.Fatal("a release without a record must do nothing", err)
	}
}
