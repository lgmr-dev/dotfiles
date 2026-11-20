package hooks_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

const (
	vscodeID  = "integration.vscode-managed"
	kiroID    = "integration.kiro"
	t3ID      = "integration.t3-code"
	t3ThemeID = "integration.t3-code-theme"
)

func settingsJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	var values map[string]any
	if err := json.Unmarshal([]byte(profileReadFile(t, path)), &values); err != nil {
		t.Fatal(err)
	}
	return values
}

func settingsChanged(t *testing.T, e *profileEnv, id string) {
	t.Helper()
	if finding := e.inspect(id, true); finding.Status != engine.Changed || len(finding.Preserved) == 0 {
		t.Fatalf("a personal value did not need an approval: %+v", finding)
	}
	if err := e.apply(id, false); !errors.Is(err, targets.ErrChanged) {
		t.Fatal("setup replaced a personal value without approval", err)
	}
}

func TestVSCodeSettingsUseIndependentlyObservedFolders(t *testing.T) {
	for system, want := range map[string][]string{
		"darwin":  {"Library", "Application Support", "Code", "User", "settings.json"},
		"linux":   {".config", "Code", "User", "settings.json"},
		"windows": {"roaming", "Code", "User", "settings.json"},
	} {
		e := newProfileEnv(t, system)
		units := e.units(vscodeID)
		fields := []string{}
		for _, unit := range units {
			if unit.Path != filepath.Join(append([]string{e.home}, want...)...) || unit.Kind != "field" {
				t.Fatal(system, unit.Path, unit.Kind)
			}
			fields = append(fields, unit.Slot)
		}
		expected := []string{"workbench.colorTheme", "workbench.preferredDarkColorTheme", "workbench.preferredLightColorTheme", "window.autoDetectColorScheme", "editor.fontFamily", "terminal.integrated.fontFamily"}
		if system != "darwin" {
			expected = append(expected, "update.mode")
		}
		if !slices.Equal(fields, expected) {
			t.Fatal(system, "managed VS Code fields", fields)
		}
	}
}

func TestVSCodeJSONCSettingsLifecyclePreservesPersonalCommentsAndFields(t *testing.T) {
	for _, prior := range []string{"", `"workbench.colorTheme" : "personal", /* after theme */`} {
		t.Run(prior, func(t *testing.T) {
			e := newProfileEnv(t, "linux")
			path := e.units(vscodeID)[0].Path
			original := "\xef\xbb\xbf// personal header\r\n{\r\n" + prior + "\r\n" +
				`"url":"https://example.org/a/*b*/", // inline` + "\r\n" +
				`"nested":{"workbench.colorTheme":"keep",},"huge":9007199254740993,` + "\r\n}\r\n"
			profileWrite(t, path, original)
			if err := os.Chmod(path, 0640); err != nil {
				t.Fatal(err)
			}
			if prior != "" {
				settingsChanged(t, e, vscodeID)
			}
			e.mustApply(vscodeID, prior != "")
			data := profileReadFile(t, path)
			if !strings.Contains(data, `"workbench.colorTheme":"Rosé Pine"`) && !strings.Contains(data, `"workbench.colorTheme" : "Rosé Pine"`) || !strings.Contains(data, `"update.mode":"none"`) {
				t.Fatal("desired settings missing", data)
			}
			profileWrite(t, path, strings.Replace(data, "9007199254740993", "9007199254740995", 1))
			if finding := e.inspect(vscodeID, true); finding.Status != engine.Satisfied {
				t.Fatalf("a personal edit outside the managed fields changed the ownership: %+v", finding)
			}
			e.mustRemove(vscodeID, false)
			want := strings.Replace(original, "9007199254740993", "9007199254740995", 1)
			if got := profileReadFile(t, path); got != want {
				t.Fatalf("personal JSONC changed: got %q, want %q", got, want)
			}
			if info, err := os.Lstat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0640 {
				t.Fatal("the mode of the settings file changed", info, err)
			}
		})
	}
}

func TestVSCodeJSONCRemovalPreservesAddedComment(t *testing.T) {
	for _, comment := range []string{"", "// personal note\n"} {
		e := newProfileEnv(t, "linux")
		path := e.units(vscodeID)[0].Path
		e.mustApply(vscodeID, false)
		profileWrite(t, path, comment+profileReadFile(t, path))
		e.mustRemove(vscodeID, false)
		data, err := os.ReadFile(path)
		if comment == "" && !os.IsNotExist(err) || comment != "" && (err != nil || !strings.Contains(string(data), comment)) {
			t.Fatal("removal lost comment or left a newly empty document", string(data), err)
		}
		if comment == "" {
			if _, err := os.Lstat(filepath.Join(e.home, ".config")); !os.IsNotExist(err) {
				t.Fatal("removal left the folders that setup created", err)
			}
		}
	}
}

func TestVSCodeOnMacOSReleasesTheUpdateModeAndReportsIt(t *testing.T) {
	for _, prior := range []string{"", `"manual"`, `"none"`} {
		t.Run(prior, func(t *testing.T) {
			e := newProfileEnv(t, "linux")
			e.p.Folders.Config = filepath.Join(e.home, "Library", "Application Support")
			path := e.units(vscodeID)[0].Path
			original := "{\"editor.fontSize\":17}\n"
			if prior != "" {
				original = "{\"update.mode\":" + prior + ",\"editor.fontSize\":17}\n"
			}
			profileWrite(t, path, original)
			e.mustApply(vscodeID, prior != "" && prior != `"none"`)
			if !strings.Contains(profileReadFile(t, path), `"update.mode":"none"`) {
				t.Fatal("the pinned VS Code route did not stop the updater")
			}
			if finding := e.inspect(vscodeID, true); len(finding.Notes) != 0 {
				t.Fatal("the note belongs to macOS only", finding.Notes)
			}

			e.p.Context.OS, e.p.Context.Arch = "darwin", "arm64"
			e.p.Folders.Config = filepath.Join(e.home, ".config")
			if e.units(vscodeID)[0].Path != path {
				t.Fatal("the fixture must keep the settings path")
			}
			finding := e.inspect(vscodeID, true)
			if finding.Status != engine.Unhealthy || !slices.Equal(finding.Notes, []string{"update.mode: none"}) {
				t.Fatalf("the recorded update.mode field needs a release on macOS: %+v", finding)
			}
			e.mustApply(vscodeID, false)
			if _, recorded := e.State().Targets[targets.Key(path, "update.mode")]; recorded {
				t.Fatal("the update.mode record stayed on macOS")
			}
			data := profileReadFile(t, path)
			finding = e.inspect(vscodeID, true)
			switch prior {
			case "":
				if strings.Contains(data, "update.mode") || len(finding.Notes) != 0 {
					t.Fatal("the release did not restore the absent baseline", data, finding.Notes)
				}
			case `"manual"`:
				if !strings.Contains(data, `"update.mode":"manual"`) || len(finding.Notes) != 0 {
					t.Fatal("the release did not restore the personal baseline", data, finding.Notes)
				}
			default:
				if !strings.Contains(data, `"update.mode":"none"`) || !slices.Equal(finding.Notes, []string{"update.mode: none"}) {
					t.Fatal("check does not report a personal update.mode: none", data, finding.Notes)
				}
			}
			if !strings.Contains(data, `"workbench.colorTheme":"Rosé Pine"`) {
				t.Fatal("the release removed another managed field", data)
			}
			e.mustRemove(vscodeID, false)
			if got := profileReadFile(t, path); got != original {
				t.Fatalf("removal did not restore the settings: %q, want %q", got, original)
			}
		})
	}
}

func TestKiroSettingsOnlyOwnTheSelfUpdateSwitch(t *testing.T) {
	for name, original := range map[string]string{
		"absent":   "",
		"personal": "{\n  \"chat.defaultModel\": \"personal\",\n  \"telemetry.enabled\": false\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			e := newProfileEnv(t, "linux")
			path := filepath.Join(e.home, ".kiro", "settings", "cli.json")
			if units := e.units(kiroID); len(units) != 1 || units[0].Path != path || units[0].Slot != "app.disableAutoupdates" {
				t.Fatal("Kiro CLI settings target", units)
			}
			if original != "" {
				profileWrite(t, path, original)
			}
			e.mustApply(kiroID, false)
			values := settingsJSON(t, path)
			if values["app.disableAutoupdates"] != true || original != "" && values["chat.defaultModel"] != "personal" || original == "" && len(values) != 1 {
				t.Fatal("Kiro self-update stays enabled or personal settings changed", values)
			}
			profileWrite(t, path, "{\n  \"app.disableAutoupdates\": true,\n  \"chat.greeting.enabled\": false\n}\n")
			if finding := e.inspect(kiroID, true); finding.Status != engine.Satisfied {
				t.Fatalf("a Kiro rewrite of personal settings invalidated ownership: %+v", finding)
			}
			if original != "" {
				profileWrite(t, path, strings.Replace(original, "{\n", "{\n  \"app.disableAutoupdates\": true,\n", 1))
			}
			e.mustRemove(kiroID, false)
			data := profileReadFile(t, path)
			if original == "" {
				if strings.Contains(data, "disableAutoupdates") || settingsJSON(t, path)["chat.greeting.enabled"] != false {
					t.Fatal("removal kept the owned field or lost a personal one", data)
				}
				return
			}
			if data != original {
				t.Fatalf("removal did not restore personal settings: got %q, want %q", data, original)
			}
		})
	}
}

func TestKiroSettingsRemovalDeletesOnlyTheFoldersItCreated(t *testing.T) {
	e := newProfileEnv(t, "linux")
	e.mustApply(kiroID, false)
	e.mustRemove(kiroID, false)
	if tree := profileTree(t, e.home); tree != "./" {
		t.Fatal("removal left a Kiro folder that setup created", tree)
	}
}

func TestKiroSettingsRequireApprovalOfAPersonalUpdateChoice(t *testing.T) {
	e := newProfileEnv(t, "linux")
	path := e.units(kiroID)[0].Path
	original := "{\"app.disableAutoupdates\":false,\"chat.defaultModel\":\"personal\"}\n"
	profileWrite(t, path, original)
	settingsChanged(t, e, kiroID)
	if got := profileReadFile(t, path); got != original {
		t.Fatal("an unapproved write changed the settings", got)
	}
	e.mustApply(kiroID, true)
	if settingsJSON(t, path)["app.disableAutoupdates"] != true {
		t.Fatal("approved Kiro settings still update in the background")
	}
	e.mustRemove(kiroID, false)
	if got := profileReadFile(t, path); got != original {
		t.Fatalf("removal did not restore the personal choice: %q", got)
	}
}

func TestKiroSettingsFollowKiroHomeAndRefuseItsLaterMove(t *testing.T) {
	e := newProfileEnv(t, "linux")
	work := filepath.Join(filepath.Dir(e.home), "kiro work")
	e.p.KiroHome = work
	path := filepath.Join(work, "settings", "cli.json")
	if e.units(kiroID)[0].Path != path {
		t.Fatal("Kiro settings ignore KIRO_HOME")
	}
	e.mustApply(kiroID, false)
	e.p.KiroHome = ""
	finding := e.inspect(kiroID, true)
	if finding.Status != engine.Blocked || !strings.Contains(finding.Reason, path) || !strings.Contains(finding.Reason, "KIRO_HOME") {
		t.Fatalf("a moved KIRO_HOME silently retargeted owned settings: %+v", finding)
	}
	if err := e.apply(kiroID, false); err == nil {
		t.Fatal("setup wrote the Kiro settings at a new location")
	}
	for _, invalid := range []string{"relative", e.home, filepath.Dir(e.home), string(filepath.Separator)} {
		e.p.KiroHome = invalid
		if got := e.units(kiroID)[0].Path; got != filepath.Join(e.home, ".kiro", "settings", "cli.json") {
			t.Fatal("an invalid KIRO_HOME did not fall back to the home folder", invalid, got)
		}
	}
	e.mustRemove(kiroID, false)
	if _, err := os.Lstat(work); !os.IsNotExist(err) {
		t.Fatal("removal left the Kiro folder that setup created", err)
	}
}

func TestT3SettingsOwnTheirFieldsAndWriteARealThemeFile(t *testing.T) {
	for name, original := range map[string]string{
		"absent":   "",
		"personal": "{\n  \"favorites\": [\n    {\n      \"provider\": \"codex\",\n      \"model\": \"personal\"\n    }\n  ],\n  \"projectSettingsFolded\": true\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			e := newProfileEnv(t, "linux")
			settings := filepath.Join(e.home, ".t3", "userdata", "settings.json")
			theme := filepath.Join(e.home, ".t3", "userdata", "themes", "rose-pine-moon.json")
			if original != "" {
				profileWrite(t, settings, original)
			}
			e.mustApply(t3ThemeID, false)
			e.mustApply(t3ID, false)
			values := settingsJSON(t, settings)
			if values["defaultTheme"] != "rose-pine-moon" || values["pullRequestMergeMethod"] != "squash" || !reflect.DeepEqual(values["storageCleanup"], map[string]any{"worktreeOnMerge": true, "worktreeOnDelete": true}) {
				t.Fatal("T3 settings were not applied", values)
			}
			if original != "" && values["projectSettingsFolded"] != true {
				t.Fatal("personal T3 settings changed", values)
			}
			if info, err := os.Lstat(theme); err != nil || !info.Mode().IsRegular() {
				t.Fatal("T3 ignores a theme that is not a regular file", info, err)
			}
			if got := settingsJSON(t, theme); got["name"] != "Rosé Pine Moon" || len(got) != 4 {
				t.Fatal("the T3 theme file is wrong", got)
			}
			profileWrite(t, settings, `{
	"storageCleanup": {
		"worktreeOnMerge": true,
		"worktreeOnDelete": true
	},
	"enableProviderUpdateChecks": false,
	"projectSettingsFolded": true,
	"sidebarAutoSettleAfterDays": null,
	"idleCompaction": "onReturn",
	"autoResumeLimitedThreads": true,
	"sidebarAutoSettleOnMerge": false,
	"defaultThreadEnvMode": "worktree",
	"branchNamingMode": "semantic",
	"removeAgentCreditsOnMerge": true,
	"pullRequestMergeMethod": "squash",
	"defaultTheme": "rose-pine-moon"
}`)
			if finding := e.inspect(t3ID, true); finding.Status != engine.Satisfied {
				t.Fatalf("a T3 rewrite of its settings invalidated ownership: %+v", finding)
			}
			e.mustRemove(t3ID, false)
			e.mustRemove(t3ThemeID, false)
			if _, err := os.Lstat(theme); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("removal left the T3 theme file", err)
			}
			if values := settingsJSON(t, settings); !reflect.DeepEqual(values, map[string]any{"projectSettingsFolded": true}) {
				t.Fatal("removal kept an owned field or lost a personal one", values)
			}
		})
	}
}

func TestT3SettingsRequireApprovalOfAPersonalChoice(t *testing.T) {
	e := newProfileEnv(t, "linux")
	settings := filepath.Join(e.home, ".t3", "userdata", "settings.json")
	original := "{\n  \"pullRequestMergeMethod\": \"merge\",\n  \"defaultThreadEnvMode\": \"local\"\n}\n"
	profileWrite(t, settings, original)
	e.mustApply(t3ThemeID, false)
	settingsChanged(t, e, t3ID)
	e.mustApply(t3ID, true)
	if values := settingsJSON(t, settings); values["pullRequestMergeMethod"] != "squash" || values["defaultThreadEnvMode"] != "worktree" {
		t.Fatal("approved T3 settings kept the personal choice", values)
	}
	e.mustRemove(t3ID, false)
	e.mustRemove(t3ThemeID, false)
	if got := profileReadFile(t, settings); got != original {
		t.Fatalf("removal did not restore the personal choice: %q", got)
	}
	if _, err := os.Lstat(filepath.Join(e.home, ".t3", "userdata", "themes")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("removal left the themes folder that setup created", err)
	}
}

func TestSettingsLockStopsAWriteWhileTheApplicationHoldsIt(t *testing.T) {
	e := newProfileEnv(t, "linux")
	path := e.units(kiroID)[0].Path
	original := "{\"chat.defaultModel\":\"personal\"}\n"
	profileWrite(t, path, original)
	if err := os.Mkdir(path+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []bool{false, true} {
		var err error
		if remove {
			err = e.p.Remove(t.Context(), e.resource(kiroID), engine.Finding{}, e)
		} else {
			err = e.apply(kiroID, false)
		}
		if (err == nil) != remove {
			t.Fatal("the settings lock of the application did not stop the write", remove, err)
		}
	}
	if got := profileReadFile(t, path); got != original {
		t.Fatal("setup wrote while the application held its lock", got)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	e.mustApply(kiroID, false)
	e.mustRemove(kiroID, false)
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || profileReadFile(t, path) != original {
		t.Fatal("setup left a lock or changed the personal settings", entries, err)
	}
}
