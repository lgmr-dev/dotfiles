package hooks_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/hooks"
)

const terminalV2Fingerprint = "cfdd459b3d918e7b86e1724f30e6008662155b16c9173b797774a4ea3b02a553"

func TestTerminalImportFingerprintEqualsTheFingerprintOfReleaseV2(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "terminal-settings-v2.jsonc"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := hooks.TerminalImportFingerprint(data)
	if err != nil || got != terminalV2Fingerprint {
		t.Fatalf("the settings that release v2 wrote must give the fingerprint that release v2 recorded: %q %v", got, err)
	}
	personal := strings.Replace(string(data), "// personal", "// another personal comment", 1)
	if got, err := hooks.TerminalImportFingerprint([]byte(personal)); err != nil || got != terminalV2Fingerprint {
		t.Fatalf("content outside the reviewed values must not change the fingerprint: %q %v", got, err)
	}
	edited := strings.Replace(string(data), `"initialRows":50`, `"initialRows":51`, 1)
	if edited == string(data) {
		t.Fatal("the fixture lost its reviewed value")
	}
	if got, err := hooks.TerminalImportFingerprint([]byte(edited)); err != nil || got == terminalV2Fingerprint || got == "" {
		t.Fatalf("a changed reviewed value must change the fingerprint: %q %v", got, err)
	}
	for _, none := range []string{"", "{\n  \"experimental.personal\": true\n}\n"} {
		if got, err := hooks.TerminalImportFingerprint([]byte(none)); err != nil || got != "" {
			t.Fatalf("settings without a reviewed value have no fingerprint: %q %v", got, err)
		}
	}
}
