package hooks_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/hooks"
	"github.com/lgmr-dev/dotfiles/installer/targets"
)

const (
	terminalID   = "integration.windows-terminal"
	terminalGUID = "{8a0e8c9b-2b4c-5842-ac1b-29cd17efc89b}"
)

func terminalEnv(t *testing.T) (*profileEnv, string) {
	t.Helper()
	e := newProfileEnv(t, "windows")
	return e, filepath.Join(e.p.Folders.LocalAppData, "Microsoft", "Windows Terminal", "settings.json")
}

func terminalSetting(t *testing.T, path, field string) (string, bool) {
	t.Helper()
	var value any
	decoder := json.NewDecoder(strings.NewReader(profileReadFile(t, path)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	for _, part := range strings.Split(field, "/") {
		object, ok := value.(map[string]any)
		if !ok {
			return "", false
		}
		if value, ok = object[part]; !ok {
			return "", false
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded), true
}

func terminalEdit(t *testing.T, path, old, new string) {
	t.Helper()
	data := profileReadFile(t, path)
	if strings.Count(data, old) != 1 {
		t.Fatalf("the fixture edit needs exactly one %q in %s", old, data)
	}
	profileWrite(t, path, strings.Replace(data, old, new, 1))
}

func terminalSatisfied(t *testing.T, e *profileEnv, message string) {
	t.Helper()
	if finding := e.inspect(terminalID, true); finding.Status != engine.Satisfied {
		t.Fatalf("%s: %+v", message, finding)
	}
}

func TestWindowsTerminalUnitIsOneProjectionSlot(t *testing.T) {
	e, path := terminalEnv(t)
	units := e.units(terminalID)
	if len(units) != 1 || units[0].Path != path || units[0].Kind != "field" || units[0].Slot != hooks.TerminalSlot || units[0].Key != targets.Key(path, "windows-terminal") {
		t.Fatalf("Windows Terminal unit: %+v", units)
	}
	if _, ok := hooks.ProfileKinds()["field#"+hooks.TerminalSlot]; !ok {
		t.Fatal("the projection kind is not registered")
	}
	pwsh, err := json.Marshal(`"` + filepath.Join(e.p.StateDir, "tools", "tool.powershell", "current", "pwsh.exe") + `"`)
	if err != nil {
		t.Fatal(err)
	}
	e.mustApply(terminalID, false)
	if got, _ := terminalSetting(t, path, "profiles/list"); !strings.Contains(got, `"commandline":`+string(pwsh)) {
		t.Fatal("the managed profile does not start the managed PowerShell through its stable path", got)
	}
	if _, err := newProfileEnv(t, "linux").p.Desired(terminalID, e); err == nil {
		t.Fatal("Windows Terminal settings are available on Windows only")
	}
}

func TestWindowsTerminalSettingsPreservePersonalNestedFieldsAndAllOtherEntries(t *testing.T) {
	e, path := terminalEnv(t)
	original := `// personal header
{
	"defaultProfile":"{my-shell}",
	"profiles":{"personal":true,"defaults":{"font":{"face":"Personal Font","weight":"bold"},"cursorShape":"bar"},"list":[{"guid":"{personal}","name":"Personal"},{"guid":"` + terminalGUID + `","name":"Previous","icon":"personal.ico"}]},
	"actions":[{"id":"personal","command":"personal"}],
	"keybindings":[{"keys":["ctrl+alt+p","alt+f9"],"id":"personal"},{"keys":"ctrl+c","id":"old","name":"My copy label"}],
	"schemes":[{"name":"personal","red":"#abcdef"},{"name":"rose-pine","red":"#123456","personal":true}],
	"themes":[{"name":"personal","window":{}},{"name":"rose-pine","window":{"myPreference":true}}],
	"huge":9007199254740993, // keep exact
}
`
	profileWrite(t, path, original)
	finding := e.inspect(terminalID, true)
	if finding.Status != engine.Changed {
		t.Fatalf("overlapping personal settings did not require an approval: %+v", finding)
	}
	if err := e.apply(terminalID, false); !errors.Is(err, targets.ErrChanged) || profileReadFile(t, path) != original {
		t.Fatal("setup replaced personal Terminal settings without approval", err)
	}
	e.mustApply(terminalID, true)
	data := profileReadFile(t, path)
	for _, kept := range []string{`"defaultProfile":"{my-shell}"`, `"weight":"bold"`, `"cursorShape":"bar"`, `"icon":"personal.ico"`, `"name":"My copy label"`, `"myPreference":true`, `9007199254740993`, `// keep exact`, `"keys":["ctrl+alt+p","alt+f9"]`, "// personal header"} {
		if !strings.Contains(data, kept) {
			t.Fatal("lost personal settings", kept, data)
		}
	}
	if !strings.Contains(data, `"historySize":32767`) || !strings.Contains(data, "tool.powershell") {
		t.Fatal("managed settings missing", data)
	}
	profileWrite(t, path, strings.Replace(data, `"weight":"bold"`, `"weight":"light","features":{"ss01":1}`, 1))
	terminalSatisfied(t, e, "unowned nested preference invalidated ownership")

	checkout := filepath.Join(filepath.Dir(e.home), "checkout")
	fragment := profileReadFile(t, filepath.Join(e.p.Checkout, "windows-terminal", "settings.fragment.jsonc"))
	profileWrite(t, filepath.Join(checkout, "windows-terminal", "settings.fragment.jsonc"), strings.Replace(fragment, `"size": 12`, `"size": 13`, 1))
	e.p.Checkout = checkout
	if finding := e.inspect(terminalID, true); finding.Status != engine.Unhealthy {
		t.Fatalf("a new reviewed recipe needs an update: %+v", finding)
	}
	e.mustApply(terminalID, false)
	if !strings.Contains(profileReadFile(t, path), `"size":13`) {
		t.Fatal("the update did not write the new reviewed value")
	}
	e.mustRemove(terminalID, false)
	data = profileReadFile(t, path)
	for _, kept := range []string{`"face":"Personal Font"`, `"weight":"light"`, `"features":{"ss01":1}`, `"name":"Previous"`, `"id":"old"`, `"red":"#123456"`, `"defaultProfile":"{my-shell}"`, `"myPreference":true`, `9007199254740993`} {
		if !strings.Contains(data, kept) {
			t.Fatal("removal lost baseline or later personal setting", kept, data)
		}
	}
	if strings.Contains(data, `"historySize"`) || strings.Contains(data, `"size":13`) {
		t.Fatal("removal retained owned setting", data)
	}
}

func TestWindowsTerminalPreservesEachPersonalDefaultAndLaterUserChanges(t *testing.T) {
	for _, prior := range []string{"", `""`, `"{61c54bbd-c2c6-5271-96e7-009a87ff44bf}"`, `"{personal}"`} {
		for _, later := range []bool{false, true} {
			e, path := terminalEnv(t)
			original := "{}"
			if prior != "" {
				original = `{"defaultProfile":` + prior + `}`
			}
			profileWrite(t, path, original)
			e.mustApply(terminalID, false)
			want := prior
			if want == "" || want == `""` {
				want = `"` + terminalGUID + `"`
			}
			if got, _ := terminalSetting(t, path, "defaultProfile"); got != want {
				t.Fatal("existing startup choice changed", got, want)
			}
			if later {
				terminalEdit(t, path, `"defaultProfile":`+want, `"defaultProfile":"{later-personal}"`)
				terminalSatisfied(t, e, "later default choice was treated as owned drift")
			}
			e.mustRemove(terminalID, false)
			value, present := terminalSetting(t, path, "defaultProfile")
			switch {
			case later:
				if !present || value != `"{later-personal}"` {
					t.Fatal("removal lost later startup preference", value)
				}
			case prior == "":
				if present {
					t.Fatal("removal retained created default", value)
				}
			default:
				if !present || value != prior {
					t.Fatal("removal lost earlier startup preference", value, prior)
				}
			}
		}
	}
}

func TestWindowsTerminalRemovalRetainsPersonalFieldsAddedToNewManagedEntries(t *testing.T) {
	e, path := terminalEnv(t)
	e.mustApply(terminalID, false)
	terminalEdit(t, path, `"font":{"face":"Hack Nerd Font"`, `"font":{"weight":"bold","face":"Hack Nerd Font"`)
	terminalEdit(t, path, `{"guid":"`+terminalGUID+`",`, `{"guid":"`+terminalGUID+`","icon":"my-icon.ico",`)
	terminalSatisfied(t, e, "personal additions to managed entries invalidated ownership")
	e.mustRemove(terminalID, false)
	data := profileReadFile(t, path)
	for _, kept := range []string{`"weight":"bold"`, `"icon":"my-icon.ico"`, terminalGUID} {
		if !strings.Contains(data, kept) {
			t.Fatal("removal lost a personal addition", kept, data)
		}
	}
	if strings.Contains(data, "Hack Nerd Font") || strings.Contains(data, "commandline") {
		t.Fatal("removal retained owned profile fields", data)
	}
}

func TestWindowsTerminalFreshRemovalDeletesOnlyTheEmptyOwnedDocument(t *testing.T) {
	e, path := terminalEnv(t)
	e.mustApply(terminalID, false)
	e.mustRemove(terminalID, false)
	if tree := profileTree(t, e.home); tree != "./" {
		t.Fatal("removal left the document or the folders that setup created", tree)
	}
	e.mustApply(terminalID, false)
	profileWrite(t, path, "// personal note\n"+profileReadFile(t, path))
	e.mustRemove(terminalID, false)
	if got := profileReadFile(t, path); got != "// personal note\n{}\n" {
		t.Fatalf("removal lost a personal comment: %q", got)
	}
}

func TestWindowsTerminalRejectsAmbiguousSettingsWithoutAChange(t *testing.T) {
	for name, bad := range map[string]string{
		"profiles-array":         `{"profiles":[]}`,
		"invalid-font":           `{"profiles":{"defaults":{"font":"wrong"}}}`,
		"case-duplicate-scheme":  `{"schemes":[{"name":"rose-pine"},{"name":"ROSE-PINE"}]}`,
		"duplicate-profile":      `{"profiles":{"list":[{"guid":"` + terminalGUID + `"},{"guid":"` + terminalGUID + `"}]}}`,
		"grouped-overlap":        `{"keybindings":[{"keys":["ctrl+c","ctrl+p"],"id":"Terminal.CopyToClipboard"}]}`,
		"default-number":         `{"defaultProfile":17}`,
		"duplicate-owned-parent": `{"profiles":{"defaults":{"font":{"face":"one","face":"two"}}}}`,
		"duplicate-top-level":    `{"initialRows":50,"initialRows":60}`,
		"null-parent":            `{"profiles":null}`,
		"array-parent":           `{"profiles":{"defaults":[]}}`,
		"invalid-array":          `{"profiles":{"list":{}}}`,
		"invalid-entry":          `{"keybindings":[null]}`,
		"invalid-key-type":       `{"keybindings":[{"keys":5}]}`,
		"duplicate-chord":        `{"keybindings":[{"keys":"ctrl+shift+f","id":"one"},{"keys":"SHIFT+CTRL+f","id":"two"}]}`,
		"case-scheme":            `{"schemes":[{"name":"ROSE-PINE"}]}`,
		"case-theme":             `{"themes":[{"name":"Rose-Pine"}]}`,
		"case-action":            `{"actions":[{"id":"dotfiles.closetab"}]}`,
		"guid-braces":            `{"profiles":{"list":[{"guid":"{` + terminalGUID + `}"}]}}`,
		"guid-without-braces":    `{"profiles":{"list":[{"guid":"` + strings.Trim(terminalGUID, "{}") + `"}]}}`,
		"invalid-identity":       `{"schemes":[{"name":12}]}`,
		"trailing-content":       `{} {}`,
		"unterminated-comment":   `{/* personal`,
		"invalid-utf8":           "\xff",
		"nul":                    "{\"personal\":\"\x00\"}",
		"empty":                  "",
		"oversized":              strings.Repeat(" ", 256<<10+1),
	} {
		t.Run(name, func(t *testing.T) {
			e, path := terminalEnv(t)
			profileWrite(t, path, bad)
			if finding := e.inspect(terminalID, true); finding.Status != engine.Unknown || finding.Reason == "" {
				t.Fatalf("unsafe Terminal settings accepted: %+v", finding)
			}
			if err := e.apply(terminalID, true); err == nil {
				t.Fatal("setup wrote unsafe Terminal settings")
			}
			if got := profileReadFile(t, path); got != bad {
				t.Fatal("inspection changed unsafe settings")
			}
		})
	}
	for name, good := range map[string]string{
		"whitespace-object": " {}\n",
		"personal-jsonc":    "\xef\xbb\xbf// original header\n{\"personal\":9007199254740993,\"profiles\":{\"defaults\":{\"font\":{\"features\":{\"ss01\":1},},},},}\n",
		"grouped-personal":  `{"keybindings":[{"keys":["ctrl+alt+p","alt+f9"],"id":"personal"}]}`,
		"guid-case":         `{"profiles":{"list":[{"guid":"` + strings.ToUpper(terminalGUID) + `","icon":"personal.ico"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			e, path := terminalEnv(t)
			profileWrite(t, path, good)
			if finding := e.inspect(terminalID, true); finding.Status != engine.Missing {
				t.Fatalf("personal settings without a managed value need no approval: %+v", finding)
			}
			e.mustApply(terminalID, false)
			e.mustRemove(terminalID, false)
			if got := profileReadFile(t, path); name != "guid-case" && got != good {
				t.Fatalf("removal did not restore the personal settings: %q, want %q", got, good)
			}
		})
	}
}

func TestWindowsTerminalLegacyCombinedBindingsRequireNativeMigration(t *testing.T) {
	for _, keys := range []string{`"ctrl+c"`, `["ctrl+c"]`, `"SHIFT+CTRL+f"`} {
		e, path := terminalEnv(t)
		original := `{"actions":[{"keys":` + keys + `,"command":"personal"}]}`
		profileWrite(t, path, original)
		finding := e.inspect(terminalID, true)
		if finding.Status != engine.Unknown || !strings.Contains(finding.Reason, "launch the supported unpackaged stable Terminal once") {
			t.Fatalf("legacy overlap lacked actionable migration boundary: %+v", finding)
		}
		if got := profileReadFile(t, path); got != original {
			t.Fatal("legacy settings changed during discovery")
		}
	}
}

func TestWindowsTerminalNativeEquivalentSerializationKeepsOwnership(t *testing.T) {
	e, path := terminalEnv(t)
	e.mustApply(terminalID, false)
	data := []byte(profileReadFile(t, path))
	data = bytes.ReplaceAll(data, []byte("#e0def4"), []byte("#E0DEF4"))
	data = bytes.ReplaceAll(data, []byte("#191724ff"), []byte("#191724"))
	data = bytes.Replace(data, []byte(`"size":12`), []byte(`"size":12.0`), 1)
	data = bytes.Replace(data, []byte(`"command":"closeTab"`), []byte(`"command":{ "action": /* native equivalent */ "closeTab" }`), 1)
	if !bytes.Contains(data, []byte(`"size":12.0`)) || !bytes.Contains(data, []byte("native equivalent")) || !bytes.Contains(data, []byte("#E0DEF4")) {
		t.Fatal("the fixture did not change the native spelling", string(data))
	}
	profileWrite(t, path, string(data))
	terminalSatisfied(t, e, "equivalent native serialization invalidated owned settings")
	for _, command := range []string{`{"action":"closeTab","index":0}`, `{"action":"closePane"}`, `"closePane"`} {
		profileWrite(t, path, strings.Replace(string(data), `{ "action": /* native equivalent */ "closeTab" }`, command, 1))
		if finding := e.inspect(terminalID, true); finding.Status != engine.Changed {
			t.Fatalf("a different close-tab command kept the ownership: %s: %+v", command, finding)
		}
	}
	profileWrite(t, path, string(data))
	e.mustRemove(terminalID, false)
}

func TestWindowsTerminalNativeCloseTabSaveKeepsOwnershipAndRawBaseline(t *testing.T) {
	e, path := terminalEnv(t)
	spelling := `"close` + "\\u0054" + `ab"`
	original := `{"actions":[{"id":"Dotfiles.CloseTab","command":` + spelling + `}]}`
	profileWrite(t, path, original)
	e.mustApply(terminalID, true)
	record := e.State().Targets[targets.Key(path, hooks.TerminalSlot)]
	baseline, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(record.Before, "value:"))
	var projection struct {
		Values map[string][]byte `json:"values"`
	}
	if err != nil || json.Unmarshal(baseline, &projection) != nil || string(projection.Values["close-tab/command"]) != spelling {
		t.Fatalf("the baseline did not keep the raw owned value: %q %v", baseline, err)
	}
	terminalEdit(t, path, `"command":"closeTab"`, `"command":{"action":"closeTab"}`)
	terminalSatisfied(t, e, "native close-tab serialization lost ownership")
	e.mustApply(terminalID, false)
	e.mustRemove(terminalID, false)
	if data := profileReadFile(t, path); !strings.Contains(data, `"command":`+spelling) {
		t.Fatal("removal did not restore original raw action", data)
	}
}

func TestWindowsTerminalRemovalRestoresDefaultWhenOwnedProfileDisappears(t *testing.T) {
	e, path := terminalEnv(t)
	profileWrite(t, path, `{"defaultProfile":"{original-personal}"}`)
	e.mustApply(terminalID, false)
	terminalEdit(t, path, `"defaultProfile":"{original-personal}"`, `"defaultProfile":"`+terminalGUID+`"`)
	e.mustRemove(terminalID, false)
	if got, _ := terminalSetting(t, path, "defaultProfile"); got != `"{original-personal}"` {
		t.Fatal("removal left the default referring to its removed profile", got)
	}
}

func TestWindowsTerminalLaterManagedDefaultSurvivesWhileProfileRemains(t *testing.T) {
	for _, scenario := range []string{"update", "original-profile", "later-personal-field"} {
		t.Run(scenario, func(t *testing.T) {
			e, path := terminalEnv(t)
			original := `{"defaultProfile":"{original-personal}"}`
			if scenario == "original-profile" {
				original = `{"defaultProfile":"{original-personal}","profiles":{"list":[{"guid":"` + terminalGUID + `","name":"Original profile"}]}}`
			}
			profileWrite(t, path, original)
			e.mustApply(terminalID, scenario == "original-profile")
			terminalEdit(t, path, `"defaultProfile":"{original-personal}"`, `"defaultProfile":"`+terminalGUID+`"`)
			if scenario == "later-personal-field" {
				terminalEdit(t, path, `{"guid":"`+terminalGUID+`",`, `{"guid":"`+terminalGUID+`","icon":"personal.ico",`)
			}
			if scenario == "update" {
				e.mustApply(terminalID, false)
			} else {
				e.mustRemove(terminalID, false)
			}
			if got, _ := terminalSetting(t, path, "defaultProfile"); got != `"`+terminalGUID+`"` {
				t.Fatal("changed a valid later default choice", got)
			}
			if list, _ := terminalSetting(t, path, "profiles/list"); !strings.Contains(list, terminalGUID) {
				t.Fatal("valid retained default has no profile", list)
			}
		})
	}
}

func TestWindowsTerminalOwnedEditNeedsApprovalAndRetainsFirstBaseline(t *testing.T) {
	e, path := terminalEnv(t)
	profileWrite(t, path, `{"profiles":{"defaults":{"font":{"size":9}}}}`)
	e.mustApply(terminalID, true)
	terminalEdit(t, path, `"size":12`, `"size":14`)
	if finding := e.inspect(terminalID, true); finding.Status != engine.Changed {
		t.Fatalf("owned user edit was not reported: %+v", finding)
	}
	if err := e.apply(terminalID, false); !errors.Is(err, targets.ErrChanged) {
		t.Fatal("owned user edit was silently overwritten", err)
	}
	if got, _ := terminalSetting(t, path, "profiles/defaults/font/size"); got != "14" {
		t.Fatal("the user edit did not stay", got)
	}
	e.mustApply(terminalID, true)
	saved, err := filepath.Glob(filepath.Join(e.p.StateDir, "baselines", "replaced-*"))
	if err != nil || len(saved) != 1 {
		t.Fatal("the approved replacement did not save the edited settings", saved, err)
	}
	e.mustRemove(terminalID, false)
	if got, _ := terminalSetting(t, path, "profiles/defaults/font/size"); got != "9" {
		t.Fatal("the second approval replaced the first baseline", got)
	}
	if _, err := os.Lstat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatal("setup left its settings lock", err)
	}
}
