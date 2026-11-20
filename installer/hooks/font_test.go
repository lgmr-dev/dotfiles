package hooks_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

type fontFixtureFace struct{ file, family, style, postScript string }

func fontFixtureFaces() []fontFixtureFace {
	faces := []fontFixtureFace{}
	for _, family := range [][3]string{{"HackNerdFont", "Hack Nerd Font", "HackNF"}, {"HackNerdFontMono", "Hack Nerd Font Mono", "HackNFM"}, {"HackNerdFontPropo", "Hack Nerd Font Propo", "HackNFP"}} {
		for _, style := range []string{"Regular", "Bold", "Italic", "BoldItalic"} {
			faces = append(faces, fontFixtureFace{family[0] + "-" + style + ".ttf", family[1], style, family[2] + "-" + style})
		}
	}
	return faces
}

func fontHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type fontEngine struct {
	t        *testing.T
	os       string
	charset  string
	selected map[string]string
	registry map[string]string
	loaded   map[string]bool
	changes  []platform.Command
	fail     error
	queryErr error
}

func (f *fontEngine) query(_ context.Context, c platform.Command) ([]byte, error) {
	f.t.Helper()
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	switch f.os {
	case "linux":
		if filepath.Base(c.Path) != "fc-match" || len(c.Args) != 3 || c.Args[0] != "-f" || c.Args[1] != "%{postscriptname}\n%{file}\n%{charset}\n" {
			f.t.Fatal("unexpected fc-match command", c)
		}
		for _, face := range fontFixtureFaces() {
			if c.Args[2] == face.family+":style="+strings.ReplaceAll(face.style, "BoldItalic", "Bold Italic") {
				if path := f.selected[face.file]; path != "" {
					return []byte(face.postScript + "\n" + path + "\n" + f.charset + "\n"), nil
				}
				return []byte("DejaVuSans\n/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf\n20-7e a0-17f\n"), nil
			}
		}
		f.t.Fatal("unexpected font pattern", c.Args[2])
	case "windows":
		var input struct {
			Faces []struct{ File, Family, PostScript, Style string }
			Paths []string
		}
		if err := json.NewDecoder(c.Stdin).Decode(&input); err != nil || len(input.Faces) != 12 || len(input.Paths) != 12 {
			f.t.Fatal("unexpected font query input", err)
		}
		result := []map[string]any{}
		for _, face := range input.Faces {
			name := "Dotfiles " + face.File + " (TrueType)"
			item := map[string]any{"name": "", "path": "", "hash": "", "glyph": false, "registration": f.registry[name]}
			if path := f.registry[name]; path != "" && f.loaded[path] {
				if data, err := os.ReadFile(path); err == nil {
					item["name"], item["hash"], item["glyph"] = face.PostScript, fontHash(data), true
				}
			}
			result = append(result, item)
		}
		return json.Marshal(result)
	}
	return nil, errors.New("unexpected font query")
}

func (f *fontEngine) Run(_ context.Context, c platform.Command) ([]byte, error) {
	f.t.Helper()
	f.changes = append(f.changes, c)
	if f.fail != nil {
		return nil, f.fail
	}
	switch f.os {
	case "linux":
		if c.Path != "/usr/bin/fc-cache" || len(c.Args) != 2 || c.Args[0] != "-f" {
			f.t.Fatal("unexpected fc-cache command", c)
		}
		f.selected = map[string]string{}
		for _, face := range fontFixtureFaces() {
			path := filepath.Join(c.Args[1], "Dotfiles-HackNerdFont", face.file)
			if sentinelExists(path) {
				f.selected[face.file] = path
			}
		}
	case "windows":
		var input struct {
			Faces                 []struct{ File string }
			Paths, Before, Hashes []string
			Install               bool
		}
		if err := json.NewDecoder(c.Stdin).Decode(&input); err != nil {
			f.t.Fatal(err)
		}
		for i, face := range input.Faces {
			name := "Dotfiles " + face.File + " (TrueType)"
			if f.registry[name] != input.Before[i] {
				return nil, errors.New("Font registration changed after approval")
			}
		}
		for i, face := range input.Faces {
			name, path := "Dotfiles "+face.File+" (TrueType)", input.Paths[i]
			switch {
			case input.Install:
				data, err := os.ReadFile(path)
				if err != nil || fontHash(data) != input.Hashes[i] {
					return nil, errors.New("Owned font file changed before registration")
				}
				f.registry[name], f.loaded[path] = path, true
			case input.Before[i] != "":
				delete(f.registry, name)
				delete(f.loaded, path)
			}
		}
	}
	return nil, nil
}

type fontFixture struct {
	*sentinelEnv
	engine   *fontEngine
	provider *hooks.Fonts
	folder   string
}

var fontResource = catalog.Resource{ID: hooks.FontResource, Name: "Hack Nerd Font"}

func newFontFixture(t *testing.T, system string) *fontFixture {
	t.Helper()
	if system == "linux" {
		desktopSkipWithoutPOSIXPaths(t, "fontconfig on Linux")
	}
	e := newSentinelEnv(t)
	native := &fontEngine{t: t, os: system, charset: "20-7e e000-f8ff", selected: map[string]string{}, registry: map[string]string{}, loaded: map[string]bool{}}
	files := []string{}
	for _, face := range fontFixtureFaces() {
		files = append(files, face.file)
	}
	p := &hooks.Fonts{Context: platform.Context{OS: system, Arch: "amd64"}, Folders: platform.Folders{Home: e.home, LocalAppData: filepath.Join(e.home, "AppData", "Local")}, StateDir: e.state,
		Pins: map[string]catalog.ArchivePin{hooks.FontArchive: {Version: "3.5.1", RequiredFiles: files}}, PowerShell: "/fake/powershell.exe", FontCache: "/usr/bin/fc-cache", FontMatch: "/usr/bin/fc-match",
		Runner: native, Query: native.query}
	p.FontMatch = filepath.Join(filepath.Dir(e.home), "bin", "fc-match")
	sentinelWrite(t, p.FontMatch, "#!/bin/sh\n", 0755)
	folder := filepath.Join(e.home, ".local", "share", "fonts", "Dotfiles-HackNerdFont")
	if system == "windows" {
		folder = filepath.Join(p.Folders.LocalAppData, "Microsoft", "Windows", "Fonts")
	}
	return &fontFixture{sentinelEnv: e, engine: native, provider: p, folder: folder}
}

func (f *fontFixture) archive() {
	f.t.Helper()
	build := filepath.Join(f.state, "tools", hooks.FontArchive, "3.5.1-aaaaaaaaaaaa-1")
	for _, face := range fontFixtureFaces() {
		sentinelWrite(f.t, filepath.Join(build, face.file), "font bytes of "+face.file, 0644)
	}
	if err := os.Symlink(build, filepath.Join(f.state, "tools", hooks.FontArchive, "current")); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fontFixture) path(face fontFixtureFace) string {
	if f.engine.os == "windows" {
		return filepath.Join(f.folder, "Dotfiles-"+face.file)
	}
	return filepath.Join(f.folder, face.file)
}

func (f *fontFixture) records(kind string) []state.Record {
	records := []state.Record{}
	for _, record := range f.State().Targets {
		if record.Kind == kind {
			records = append(records, record)
		}
	}
	return records
}

func TestFontPublicationAndRemovalOwnOnlyTheExactFaces(t *testing.T) {
	for _, system := range []string{"linux", "windows"} {
		t.Run(system, func(t *testing.T) {
			f := newFontFixture(t, system)
			p := f.provider
			if finding := desktopInspect(t, p, f.sentinelEnv, hooks.FontResource, true); finding.Status != engine.Missing {
				t.Fatal("missing fonts are not reported", finding)
			}
			err := p.Apply(context.Background(), fontResource, desktopInspect(t, p, f.sentinelEnv, hooks.FontResource, true), f)
			if err == nil || !strings.Contains(err.Error(), "requires its verified archive") || len(f.engine.changes) != 0 {
				t.Fatal("fonts were activated without the archive", err)
			}
			f.archive()
			desktopApply(t, p, f.sentinelEnv, hooks.FontResource)
			files, fonts := f.records("file"), f.records("font")
			if len(files) != 12 || len(fonts) != 12 {
				t.Fatal("setup did not record twelve font files and twelve registrations", len(files), len(fonts))
			}
			for _, face := range fontFixtureFaces() {
				if sentinelRead(t, f.path(face)) != "font bytes of "+face.file {
					t.Fatal("unexpected font file", face.file)
				}
				record := f.State().Targets[targets.Key(f.path(face), hooks.FontSlot)]
				if record.Resource != hooks.FontResource || !strings.HasPrefix(record.Applied, "font:") || record.Next != "" || record.Before != "absent" {
					t.Fatal("the registration is not a separate finished font record", face.file, record)
				}
				if system == "windows" && f.engine.registry["Dotfiles "+face.file+" (TrueType)"] != f.path(face) {
					t.Fatal("unexpected registration value", face.file, f.engine.registry)
				}
			}
			if system == "linux" && (len(f.engine.changes) != 1 || !slices.Equal(f.engine.changes[0].Args, []string{"-f", filepath.Dir(f.folder)})) {
				t.Fatal("unexpected font cache refresh", f.engine.changes)
			}
			changes := len(f.engine.changes)
			desktopRemove(t, p, f.sentinelEnv, hooks.FontResource)
			if sentinelExists(f.folder) || len(f.engine.registry) != 0 || len(f.engine.changes) != changes+1 {
				t.Fatal("the removal left a font file or a registration", f.engine.registry, len(f.engine.changes))
			}
		})
	}
}

func TestFontReusesPreexistingFacesWithARealNerdGlyph(t *testing.T) {
	for charset, reused := range map[string]bool{"20-7e f120": true, "20-7e e000-f8ff": true, "f120-f121": true, "20-7e f121-f200": false, "20-7e": false, "x-y f11f": false, "1-2-3 20": false} {
		f := newFontFixture(t, "linux")
		f.engine.charset = charset
		for _, face := range fontFixtureFaces() {
			path := filepath.Join(f.home, "personal fonts", face.file)
			sentinelWrite(t, path, "personal "+face.file, 0644)
			f.engine.selected[face.file] = path
		}
		finding := desktopInspect(t, f.provider, f.sentinelEnv, hooks.FontResource, true)
		if reused != (finding.Status == engine.Satisfied && strings.Contains(strings.Join(finding.Notes, " "), "already installed")) {
			t.Fatal("unexpected reuse for the charset", charset, finding)
		}
		if !reused {
			continue
		}
		if err := f.provider.Apply(context.Background(), fontResource, finding, f); err != nil || len(f.State().Targets) != 0 || len(f.engine.changes) != 0 || sentinelExists(f.folder) {
			t.Fatal("setup took ownership of fonts that it reuses", err)
		}
	}
}

func TestFontKeepsAForeignOrChangedRegistration(t *testing.T) {
	f := newFontFixture(t, "windows")
	f.archive()
	name := "Dotfiles HackNerdFont-Bold.ttf (TrueType)"
	f.engine.registry[name] = `C:\personal\Hack.ttf`
	finding := desktopInspect(t, f.provider, f.sentinelEnv, hooks.FontResource, true)
	if finding.Status != engine.Blocked || !strings.Contains(finding.Reason, "a preexisting or changed font registration is retained") || !slices.Equal(finding.Preserved, []string{"current-user font registration: HackNerdFont-Bold.ttf"}) {
		t.Fatal("a foreign registration does not block the install", finding)
	}
	if err := f.provider.Apply(context.Background(), fontResource, finding, f); err == nil || len(f.engine.changes) != 0 || sentinelExists(f.folder) {
		t.Fatal("setup installed beside a foreign registration", err)
	}
	delete(f.engine.registry, name)
	desktopApply(t, f.provider, f.sentinelEnv, hooks.FontResource)
	f.engine.registry[name] = `C:\personal\Hack.ttf`
	if changed := desktopInspect(t, f.provider, f.sentinelEnv, hooks.FontResource, true); changed.Status != engine.Blocked {
		t.Fatal("a changed registration does not block a repair", changed)
	}
	released := desktopInspect(t, f.provider, f.sentinelEnv, hooks.FontResource, false)
	if released.Status != engine.Changed || !strings.Contains(released.Reason, "HackNerdFont-Bold.ttf") {
		t.Fatal("a changed registration is not retained at removal", released)
	}
	if err := f.provider.Remove(context.Background(), fontResource, released, f); err == nil || !strings.Contains(err.Error(), "changed native font registration is retained") {
		t.Fatal("the removal changed a foreign registration", err)
	}
	if f.engine.registry[name] != `C:\personal\Hack.ttf` || len(f.engine.registry) != 12 || len(f.records("font")) != 12 {
		t.Fatal("the refused removal changed a registration or a record", f.engine.registry)
	}
}

func TestFontKeepsAPreexistingDestinationAndAnEditedFile(t *testing.T) {
	f := newFontFixture(t, "linux")
	f.archive()
	personal := filepath.Join(f.folder, "HackNerdFont-Regular.ttf")
	sentinelWrite(t, personal, "personal", 0644)
	finding := desktopInspect(t, f.provider, f.sentinelEnv, hooks.FontResource, true)
	if finding.Status != engine.Blocked || !strings.Contains(finding.Reason, "preexisting font destination is retained") {
		t.Fatal("a preexisting destination does not block the install", finding)
	}
	if err := f.provider.Apply(context.Background(), fontResource, finding, f); err == nil || sentinelRead(t, personal) != "personal" || len(f.State().Targets) != 0 {
		t.Fatal("setup replaced a preexisting font file", err)
	}
	if err := os.Remove(personal); err != nil {
		t.Fatal(err)
	}
	desktopApply(t, f.provider, f.sentinelEnv, hooks.FontResource)
	sentinelWrite(t, personal, "edited", 0644)
	if changed := desktopInspect(t, f.provider, f.sentinelEnv, hooks.FontResource, true); changed.Status != engine.Changed || !slices.Contains(changed.Preserved, personal) {
		t.Fatal("an edited font file is not preserved", changed)
	}
	if changed := desktopInspect(t, f.provider, f.sentinelEnv, hooks.FontResource, false); changed.Status != engine.Changed {
		t.Fatal("an edited font file is not retained at removal", changed)
	}
}

func TestFontFailedActivationLeavesTheFontRecordsUnfinished(t *testing.T) {
	for _, system := range []string{"linux", "windows"} {
		t.Run(system, func(t *testing.T) {
			f := newFontFixture(t, system)
			f.archive()
			f.engine.fail = errors.New("fixture: activation failed")
			err := f.provider.Apply(context.Background(), fontResource, desktopInspect(t, f.provider, f.sentinelEnv, hooks.FontResource, true), f)
			if err == nil || !strings.Contains(err.Error(), "fixture: activation failed") {
				t.Fatal(err)
			}
			if files := f.records("file"); len(files) != 12 || slices.ContainsFunc(files, func(r state.Record) bool { return r.Applied == "" || r.Next != "" }) {
				t.Fatal("the font files are not finished file targets", files)
			}
			if fonts := f.records("font"); len(fonts) != 12 || slices.ContainsFunc(fonts, func(r state.Record) bool { return r.Applied != "" || r.Next == "" }) {
				t.Fatal("a failed activation finished a font record", fonts)
			}
			finding := desktopInspect(t, f.provider, f.sentinelEnv, hooks.FontResource, true)
			if finding.Status != engine.Unhealthy || !strings.Contains(finding.Reason, "native activation is unfinished") {
				t.Fatal("the unfinished activation is not reported", finding)
			}
			f.engine.fail = nil
			desktopApply(t, f.provider, f.sentinelEnv, hooks.FontResource)
			if fonts := f.records("font"); slices.ContainsFunc(fonts, func(r state.Record) bool { return r.Applied == "" || r.Next != "" }) {
				t.Fatal("the second apply did not finish the font records", fonts)
			}
			desktopRemove(t, f.provider, f.sentinelEnv, hooks.FontResource)
		})
	}
}

func TestFontWithoutTheNerdGlyphIsNotReady(t *testing.T) {
	f := newFontFixture(t, "linux")
	f.archive()
	f.engine.charset = "20-7e"
	err := f.provider.Apply(context.Background(), fontResource, desktopInspect(t, f.provider, f.sentinelEnv, hooks.FontResource, true), f)
	if err == nil || !strings.Contains(err.Error(), "native consumption is not ready") || len(f.records("font")) != 12 {
		t.Fatal("fonts without the Nerd Font glyph passed", err)
	}
	if finding := desktopInspect(t, f.provider, f.sentinelEnv, hooks.FontResource, true); finding.Status != engine.Unhealthy {
		t.Fatal(finding)
	}
}

func TestFontMissingInspectionDoesNotInferAbsence(t *testing.T) {
	f := newFontFixture(t, "linux")
	f.engine.queryErr = errors.New("fixture: fc-match is not there")
	if _, err := f.provider.Inspect(context.Background(), fontResource, engine.Want{Present: true}, f); err == nil {
		t.Fatal("a failed inspection gave a result")
	}
	f.provider.FontMatch = ""
	f.engine.queryErr = nil
	if _, err := f.provider.Inspect(context.Background(), fontResource, engine.Want{Present: true}, f); err == nil || !strings.Contains(err.Error(), "fc-match is unavailable") {
		t.Fatal(err)
	}
}

func TestFontDarwinQuerySeparatesScriptAndNames(t *testing.T) {
	f := newFontFixture(t, "darwin")
	var command platform.Command
	f.provider.Query = func(_ context.Context, c platform.Command) ([]byte, error) {
		command = c
		return json.Marshal(make([]struct {
			Name string `json:"name"`
		}, 12))
	}
	if finding := desktopInspect(t, f.provider, f.sentinelEnv, hooks.FontResource, true); finding.Status != engine.Missing {
		t.Fatal(finding)
	}
	if command.Path != "/usr/bin/osascript" || len(command.Args) != 5 || !slices.Equal(command.Args[:3], []string{"-l", "JavaScript", "-e"}) || command.Stdin != nil {
		t.Fatal("the font query must pass its JSON names separately from the static script", command)
	}
	var names []string
	if err := json.Unmarshal([]byte(command.Args[4]), &names); err != nil {
		t.Fatal(err)
	}
	want := []string{}
	for _, face := range fontFixtureFaces() {
		want = append(want, face.postScript)
		if strings.Contains(command.Args[3], face.postScript) {
			t.Fatal("a font name is in the executable source")
		}
	}
	if !slices.Equal(names, want) {
		t.Fatal("the native font inspection changed its exact ordered face names", names)
	}
}

func TestFontQueryProcessFixture(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "font-query-fixture" {
		return
	}
	role := os.Args[len(os.Args)-1]
	var input struct{ Faces []json.RawMessage }
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var err error
	switch role {
	case "progress", "failure", "malformed":
		_, err = fmt.Fprint(os.Stderr, "#< CLIXML\n<Objs Version=\"1.1.0.1\" xmlns=\"http://schemas.microsoft.com/powershell/2004/04\"><Obj S=\"progress\"><MS><PR N=\"Record\"><AV>Preparing modules for first use.</AV></PR></MS></Obj></Objs>")
	case "stdout-bound":
		_, err = fmt.Fprint(os.Stdout, strings.Repeat("x", (1<<20)+1))
	case "stderr-bound":
		_, err = fmt.Fprint(os.Stderr, strings.Repeat("x", (1<<20)+1))
	case "combined-bound":
		_, err = fmt.Fprint(os.Stdout, strings.Repeat("x", 600<<10))
		if err == nil {
			_, err = fmt.Fprint(os.Stderr, strings.Repeat("x", 600<<10))
		}
	default:
		os.Exit(2)
	}
	if err != nil {
		os.Exit(2)
	}
	if role == "malformed" {
		_, err = fmt.Fprint(os.Stdout, "# malformed protocol output\n")
	}
	if err == nil {
		err = json.NewEncoder(os.Stdout).Encode(make([]struct {
			Name string `json:"name"`
		}, len(input.Faces)))
	}
	if err != nil {
		os.Exit(2)
	}
	if role == "failure" {
		fmt.Fprint(os.Stderr, "\nfont inspection fixture failure\x1b\n")
		os.Exit(23)
	}
	os.Exit(0)
}

func fontQueryBoundary(t *testing.T, role string) (engine.Finding, error) {
	t.Helper()
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := newFontFixture(t, "windows")
	f.provider.PowerShell = program
	f.provider.Query = func(ctx context.Context, c platform.Command) ([]byte, error) {
		if c.Path != program || len(c.Args) != 5 || c.Args[3] != "-EncodedCommand" {
			t.Fatal("unexpected font query command", c.Args)
		}
		c.Args = []string{"-test.run=^TestFontQueryProcessFixture$", "--", "font-query-fixture", role}
		return hooks.FontQuery(ctx, c)
	}
	return f.provider.Inspect(context.Background(), fontResource, engine.Want{Present: true}, f)
}

func TestFontQuerySeparatesJSONFromPowerShellProgress(t *testing.T) {
	finding, err := fontQueryBoundary(t, "progress")
	if err != nil || finding.Status != engine.Missing {
		t.Fatal("the font JSON was contaminated by PowerShell progress", finding, err)
	}
}

func TestFontQueryKeepsTheDiagnosticOfAFailedProcess(t *testing.T) {
	_, err := fontQueryBoundary(t, "failure")
	if code, ok := platform.ExitCode(err); !ok || code != 23 || !strings.Contains(err.Error(), "font inspection fixture failure") || strings.ContainsRune(err.Error(), '\x1b') {
		t.Fatal("the native font failure or its diagnostic was lost", err)
	}
}

func TestFontQueryRejectsMalformedStdoutAndBoundsBothStreams(t *testing.T) {
	if _, err := fontQueryBoundary(t, "malformed"); err == nil {
		t.Fatal("the query removed invalid protocol output")
	}
	for _, role := range []string{"stdout-bound", "stderr-bound", "combined-bound"} {
		if _, err := fontQueryBoundary(t, role); err == nil || !strings.Contains(err.Error(), "inspection exceeds 1 MiB") {
			t.Fatal("the query exceeded its total output bound", role, err)
		}
	}
}

func TestFontKindReadsARegistrationThroughTheWriter(t *testing.T) {
	f := newFontFixture(t, "windows")
	f.archive()
	desktopApply(t, f.provider, f.sentinelEnv, hooks.FontResource)
	writer := &targets.Writer{StateDir: f.state, Kinds: f.provider.Kinds()}
	face := fontFixtureFaces()[0]
	key := targets.Key(f.path(face), hooks.FontSlot)
	if live, err := writer.InspectRecord(key, f); err != nil || live.State != targets.StateApplied {
		t.Fatal("the registered kind does not read the registration", live, err)
	}
	if err := writer.Release(context.Background(), key, f, targets.Preserve); err != nil {
		t.Fatal(err)
	}
	if _, has := f.engine.registry["Dotfiles "+face.file+" (TrueType)"]; has || len(f.engine.registry) != 11 || len(f.records("font")) != 11 {
		t.Fatal("the release of one registration record changed another registration", f.engine.registry)
	}
}

func TestFontWithoutFcMatchIsMissingOnlyWhenNothingOfSetupIsInstalled(t *testing.T) {
	f := newFontFixture(t, "linux")
	f.archive()
	match := f.provider.FontMatch
	if err := os.Remove(match); err != nil {
		t.Fatal(err)
	}
	f.engine.queryErr = errors.New("fork/exec fc-match: no such file or directory")
	before := desktopInspect(t, f.provider, f.sentinelEnv, hooks.FontResource, true)
	if before.Status != engine.Missing {
		t.Fatal("fonts without fc-match, without a file and without a record are not missing", before)
	}
	personal := filepath.Join(f.folder, "HackNerdFont-Regular.ttf")
	sentinelWrite(t, personal, "personal", 0644)
	if _, err := f.provider.Inspect(context.Background(), fontResource, engine.Want{Present: true}, f); err == nil {
		t.Fatal("a present font file without fc-match gave a result")
	}
	if err := os.RemoveAll(filepath.Join(f.home, ".local")); err != nil {
		t.Fatal(err)
	}
	key := targets.Key(personal, hooks.FontSlot)
	if err := f.Update(func(s *state.State) error {
		s.Targets[key] = state.Record{Resource: hooks.FontResource, Kind: "font", Next: "font:fixture", Before: "absent"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.provider.Inspect(context.Background(), fontResource, engine.Want{Present: true}, f); err == nil {
		t.Fatal("a font record without fc-match gave a result")
	}
	if err := f.Update(func(s *state.State) error { s.Drop(key); return nil }); err != nil {
		t.Fatal(err)
	}
	sentinelWrite(t, match, "#!/bin/sh\n", 0755)
	f.engine.queryErr = nil
	after := desktopInspect(t, f.provider, f.sentinelEnv, hooks.FontResource, true)
	if after.Status != engine.Missing || after.Token != before.Token {
		t.Fatal("the install of fc-match changed the token of the fonts", before, after)
	}
	if err := f.provider.Apply(context.Background(), fontResource, before, f); err != nil {
		t.Fatal(err)
	}
	if finding := desktopInspect(t, f.provider, f.sentinelEnv, hooks.FontResource, true); finding.Status != engine.Satisfied || len(f.records("font")) != 12 {
		t.Fatal("the apply after the install of fc-match did not verify the fonts", finding)
	}
}
