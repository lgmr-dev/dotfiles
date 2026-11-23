package hooks_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/engine"
)

const gitID = "integration.git"

func gitEnv(t *testing.T, system string) (e *profileEnv, source, target string) {
	t.Helper()
	e = newProfileEnv(t, system)
	checkout := filepath.Join(filepath.Dir(e.home), "checkout with space ü")
	source = filepath.Join(checkout, "git", "config")
	profileWrite(t, source, profileReadFile(t, filepath.Join(e.p.Checkout, "git", "config")))
	e.p.Checkout = checkout
	return e, source, filepath.Join(e.home, ".config", "git", "config")
}

func gitRun(t *testing.T, home string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = home
	for _, variable := range os.Environ() {
		key, _, _ := strings.Cut(variable, "=")
		if key = strings.ToUpper(key); !strings.HasPrefix(key, "GIT_") && key != "HOME" && key != "USERPROFILE" && key != "XDG_CONFIG_HOME" {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, output, err)
	}
	return strings.TrimSpace(string(output))
}

func TestGitDefaultsApplyAndPersonalSettingsKeepPrecedence(t *testing.T) {
	e, source, _ := gitEnv(t, "linux")
	e.mustApply(gitID, false)
	for _, key := range []string{"pull.rebase", "fetch.prune"} {
		if got := gitRun(t, e.home, "config", "--bool", "--get", key); got != "true" {
			t.Fatal("missing Git default", key, got)
		}
	}
	gitRun(t, e.home, "config", "--global", "user.email", "fixture@example.invalid")
	if finding := e.inspect(gitID, true); finding.Status != engine.Satisfied {
		t.Fatalf("a personal Git setting outside the block changed the ownership: %+v", finding)
	}
	if data := profileReadFile(t, source); strings.Contains(data, "fixture@example.invalid") {
		t.Fatal("a global Git write changed the checkout")
	}
	for _, location := range []string{"xdg", "home"} {
		t.Run(location, func(t *testing.T) {
			e, _, target := gitEnv(t, "linux")
			personal := target
			if location == "home" {
				personal = filepath.Join(e.home, ".gitconfig")
			}
			original := "[pull]\n\trebase = false\n[fetch]\n\tprune = false\n"
			profileWrite(t, personal, original)
			e.mustApply(gitID, false)
			for _, key := range []string{"pull.rebase", "fetch.prune"} {
				if got := gitRun(t, e.home, "config", "--bool", "--get", key); got != "false" {
					t.Fatal("managed defaults overrode personal Git settings", key, got)
				}
			}
			e.mustRemove(gitID, false)
			if got := profileReadFile(t, personal); got != original {
				t.Fatalf("Git removal changed the original personal configuration: %q", got)
			}
		})
	}
}

func TestGitSentinelAttributionFollowsTheSelection(t *testing.T) {
	e, _, _ := gitEnv(t, "linux")
	attribution := func() string {
		return gitRun(t, e.home, "config", "--default", "unset", "--get", "sentinel.attribution")
	}
	e.selected = []string{"tool.git", "sentinel-no-attribution"}
	e.mustApply(gitID, false)
	if got := attribution(); got != "off" {
		t.Fatal("the selection did not turn Sentinel attribution off", got)
	}
	e.selected = []string{"tool.git"}
	if finding := e.inspect(gitID, true); finding.Status != engine.Unhealthy {
		t.Fatalf("a deselected attribution choice needs a repair: %+v", finding)
	}
	e.mustApply(gitID, false)
	if got := attribution(); got != "unset" {
		t.Fatal("the deselected choice left the attribution setting", got)
	}
}

func TestGitIncludeGoesAfterTheBOMAndRemovalRestoresTheFile(t *testing.T) {
	e, _, target := gitEnv(t, "linux")
	original := "\xef\xbb\xbf# personal\r\n"
	profileWrite(t, target, original)
	e.mustApply(gitID, false)
	if got := profileReadFile(t, target); !strings.HasPrefix(got, "\xef\xbb\xbf\r\n# >>> dotfiles:integration.git >>>\r\n[include]\r\n") || !strings.HasSuffix(got, "# <<< dotfiles:integration.git <<<\r\n# personal\r\n") {
		t.Fatalf("the include block is not at the start, after the byte order mark: %q", got)
	}
	gitRun(t, e.home, "config", "--global", "pull.rebase", "false")
	if got := gitRun(t, e.home, "config", "--bool", "pull.rebase"); got != "false" {
		t.Fatal("personal override did not win", got)
	}
	gitRun(t, e.home, "config", "--global", "--unset", "pull.rebase")
	if got := gitRun(t, e.home, "config", "--bool", "pull.rebase"); got != "true" {
		t.Fatal("unsetting personal override did not reveal the default", got)
	}
	e.mustRemove(gitID, false)
	if got := profileReadFile(t, target); !strings.HasPrefix(got, original) || strings.Contains(got, "dotfiles:") {
		t.Fatalf("Git integration lost the original BOM or comments: %q", got)
	}
}

func TestGitRejectsUTF16WithoutChangingTheFile(t *testing.T) {
	e, _, target := gitEnv(t, "linux")
	original := string([]byte{0xff, 0xfe, '[', 0, 'x', 0, ']', 0, '\n', 0})
	profileWrite(t, target, original)
	finding := e.inspect(gitID, true)
	if finding.Status != engine.Unknown || !strings.Contains(finding.Reason, "UTF-8") {
		t.Fatalf("UTF-16 Git configuration was not blocked: %+v", finding)
	}
	if err := e.apply(gitID, true); err == nil {
		t.Fatal("setup wrote a UTF-16 Git configuration")
	}
	if got := profileReadFile(t, target); got != original {
		t.Fatal("invalid Git configuration changed")
	}
}

func TestGitNewWindowsConfigurationDoesNotSeedABOM(t *testing.T) {
	e, _, target := gitEnv(t, "windows")
	e.mustApply(gitID, false)
	if data := profileReadFile(t, target); strings.HasPrefix(data, "\xef\xbb\xbf") || !strings.HasPrefix(data, "\n# >>> dotfiles:integration.git >>>\n") {
		t.Fatalf("new Git configuration inherited the PowerShell-only BOM: %q", data)
	}
}

func TestGitPublicationRespectsNativeApplicationLock(t *testing.T) {
	e, _, target := gitEnv(t, "linux")
	profileWrite(t, target+".lock", "owned by Git")
	if err := e.apply(gitID, false); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatal("installer ignored Git's application lock", err)
	}
	if got := profileReadFile(t, target+".lock"); got != "owned by Git" {
		t.Fatal("installer changed the native Git lock")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("installer published while Git held its lock", err)
	}
	if err := os.Remove(target + ".lock"); err != nil {
		t.Fatal(err)
	}
	e.mustApply(gitID, false)
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil || len(entries) != 1 {
		t.Fatal("the settings lock of setup stayed beside the Git configuration", entries, err)
	}
}

func TestGitRemovalWorksWhenDefaultsSourceIsMissing(t *testing.T) {
	e, source, _ := gitEnv(t, "linux")
	e.mustApply(gitID, false)
	gitRun(t, e.home, "config", "--global", "user.email", "fixture@example.invalid")
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if finding := e.inspect(gitID, true); finding.Status != engine.Blocked || !strings.Contains(finding.Reason, "Git defaults source is unavailable") {
		t.Fatalf("a missing defaults source did not block the publication: %+v", finding)
	}
	e.mustRemove(gitID, false)
	if got := gitRun(t, e.home, "config", "--global", "user.email"); got != "fixture@example.invalid" {
		t.Fatal(got)
	}
}

func TestGitRemovalStaysAtApprovedPathAfterEnvironmentChanges(t *testing.T) {
	e, _, original := gitEnv(t, "linux")
	e.mustApply(gitID, false)
	gitRun(t, e.home, "config", "--global", "user.email", "fixture@example.invalid")
	moved := filepath.Join(e.home, "different-xdg", "git", "config")
	e.p.Folders.GitConfig = moved
	finding := e.inspect(gitID, true)
	if finding.Status != engine.Blocked || !strings.Contains(finding.Reason, "path changed") {
		t.Fatalf("changed discovery path was not explained: %+v", finding)
	}
	if err := e.apply(gitID, false); err == nil {
		t.Fatal("setup published at a new Git path while it owns the earlier one")
	}
	e.mustRemove(gitID, false)
	data := profileReadFile(t, original)
	if strings.Contains(data, "dotfiles:integration.git") || !strings.Contains(data, "fixture@example.invalid") {
		t.Fatal("removal failed to preserve personal data at its original target", data)
	}
	if _, err := os.Lstat(moved); !os.IsNotExist(err) {
		t.Fatal("removal wrote a new discovery path", err)
	}
	e.mustApply(gitID, false)
	if !strings.Contains(profileReadFile(t, moved), "dotfiles:integration.git") {
		t.Fatal("explicit removal did not permit a fresh Git target")
	}
	e.mustRemove(gitID, false)
	if _, err := os.Lstat(filepath.Join(e.home, "different-xdg")); !os.IsNotExist(err) {
		t.Fatal("new target did not retain its independent absent baseline", err)
	}
}

func TestGitConfigurationInsideTheCheckoutIsBlocked(t *testing.T) {
	e, source, _ := gitEnv(t, "linux")
	before := profileReadFile(t, source)
	for _, path := range []string{source, filepath.Join(e.p.Checkout, "personal", "config")} {
		e.p.Folders.GitConfig = path
		finding := e.inspect(gitID, true)
		if finding.Status != engine.Blocked || !strings.Contains(finding.Reason, "inside the dotfiles checkout") {
			t.Fatalf("a Git configuration inside the checkout was accepted: %+v", finding)
		}
		if err := e.apply(gitID, true); err == nil {
			t.Fatal("setup published into the checkout")
		}
	}
	e.p.Folders.GitConfig, e.p.Folders.GitIssue = "", "Git cannot report its global configuration."
	if finding := e.inspect(gitID, true); finding.Status != engine.Blocked || finding.Reason != "Git cannot report its global configuration." {
		t.Fatalf("the Git discovery issue did not block the publication: %+v", finding)
	}
	if after := profileReadFile(t, source); after != before {
		t.Fatal("the checkout source changed")
	}
}

func TestGitIncludeEditsRequireReviewAndRemainPreserved(t *testing.T) {
	e, source, target := gitEnv(t, "linux")
	e.mustApply(gitID, false)
	personal := filepath.Join(e.home, "extra.gitconfig")
	profileWrite(t, personal, "[user]\n\temail = include@example.invalid\n")
	gitRun(t, e.home, "config", "--global", "--add", "include.path", personal)
	edited := profileReadFile(t, target)
	for _, present := range []bool{true, false} {
		finding := e.inspect(gitID, present)
		if finding.Status != engine.Changed || len(finding.Preserved) != 1 {
			t.Fatalf("edited managed include was not detected: %+v", finding)
		}
	}
	if err := e.apply(gitID, false); err == nil {
		t.Fatal("setup replaced an edited include without approval")
	}
	if got := profileReadFile(t, target); got != edited {
		t.Fatal("an unapproved write changed the edited include")
	}
	if bytes.Contains([]byte(profileReadFile(t, source)), []byte("include@example.invalid")) {
		t.Fatal("include edit wrote into the checkout")
	}
	e.mustApply(gitID, true)
	saved, err := filepath.Glob(filepath.Join(e.p.StateDir, "baselines", "replaced-*"))
	if err != nil || len(saved) != 1 || !strings.Contains(profileReadFile(t, saved[0]), "extra.gitconfig") {
		t.Fatal("the approved replacement did not save the edited block", saved, err)
	}
	e.mustRemove(gitID, false)
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("removal did not restore the absent baseline", err)
	}
}
