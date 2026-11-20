package v2import_test

import (
	"path/filepath"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/targets"
	"github.com/luisgui1757/dotfiles/installer/v2import"
)

const (
	goldenBlockFingerprint  = "4736038ed685ee967155551ad424eda1c95964390c5480c88f29022ab8341558"
	goldenFieldsFingerprint = "3f04df67fc7d0043f217bbc439736b1553bd1851edf41b4ddd3ecc3186588951"
)

func TestMappingAcceptsTheFingerprintsThatReleaseV2Computed(t *testing.T) {
	f := newFixture(t, "linux")
	profile := filepath.Join(f.home, ".tmux.plugins.conf")
	write(t, profile, "# personal\n\n# >>> dotfiles:tmux.plugins >>>\nrun-shell \"/x/plugins.sh\"\n# <<< dotfiles:tmux.plugins <<<\nalias after=1\n", 0644)
	before := v2import.Observation{Provider: "profile-block", Scope: "user", Identity: "identity", Fingerprint: "before"}
	after := before
	after.Present, after.Healthy, after.Fingerprint = true, true, goldenBlockFingerprint
	f.document(filepath.Join(f.state, f.recovery(plugins), "profile.json"), map[string]any{"schema": 1, "resource": plugins, "before": before, "entries": []profileEntry{{Path: profile}}})
	f.v1.Receipts[plugins] = v2import.Receipt{Ownership: "created", Before: before, After: after, Status: "ready", Recovery: f.recovery(plugins), OperationID: operation(plugins)}

	settings := filepath.Join(f.home, ".config", "Code", "User", "settings.json")
	write(t, settings, "{\n  // personal comment\n  \"editor.fontSize\": 13,\n  \"workbench.colorTheme\": \"Rosé <Pine>\",\n  \"update.mode\": \"none\", // trailing\n}\n", 0644)
	after.Fingerprint = goldenFieldsFingerprint
	f.document(filepath.Join(f.state, f.recovery(vscode), "profile.json"), map[string]any{"schema": 1, "resource": vscode, "before": before,
		"fields": []string{"workbench.colorTheme", "update.mode"}, "entries": []profileEntry{{Path: settings}}})
	f.v1.Receipts[vscode] = v2import.Receipt{Ownership: "created", Before: before, After: after, Status: "ready", Recovery: f.recovery(vscode), OperationID: operation(vscode)}
	f.v1.Selected = []string{"tmux"}
	f.save()
	st := mustMap(t, f).State
	owned(t, st, targets.Key(profile, plugins), plugins, "block", "absent")
	owned(t, st, targets.Key(settings, "workbench.colorTheme"), vscode, "field", "absent")
	owned(t, st, targets.Key(settings, "update.mode"), vscode, "field", "absent")
}
