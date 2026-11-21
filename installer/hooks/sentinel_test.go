package hooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/hooks"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

func TestSentinelInstallsTheLatestReleaseAndRecordsTheCheckout(t *testing.T) {
	f := newSentinelFixture(t)
	sentinelWrite(t, filepath.Join(f.home, ".codex", "config.toml"), "", 0600)
	p := f.provider()
	if finding := f.inspect(p, true); finding.Status != engine.Missing || !finding.Pending {
		t.Fatal("a missing Sentinel is not reported", finding)
	}
	if err := f.apply(p); err != nil {
		t.Fatal(err)
	}
	if finding := f.inspect(p, true); finding.Status != engine.Satisfied || finding.Version != "v0.4.0" {
		t.Fatal("the installed release is not satisfied", finding)
	}
	if got := f.calls(); len(got) != 2 || got[0] != "--global --hosts codex --skills core,ui,formal,ops" || got[1] != "--global --hosts codex --skills core,ui,formal,ops --check" {
		t.Fatal("unexpected Sentinel installer calls", got)
	}
	if f.head() != f.floor {
		t.Fatal("the checkout does not hold the commit of the release")
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(sentinelRead(t, filepath.Join(f.state, "sentinel", "record.json"))), &record); err != nil {
		t.Fatal(err)
	}
	if record["schema"] != float64(1) || record["action"] != "install" || record["commit"] != f.floor || record["tag"] != "v0.4.0" || record["complete"] != true ||
		fmt.Sprint(record["hosts"]) != "[codex]" || len(record["operation"].(string)) != 64 || len(record) != 8 {
		t.Fatal("the record changed its format", record)
	}
	homes := record["homes"].(map[string]any)
	if !slices.Equal(slices.Sorted(maps.Keys(homes)), []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "COPILOT_HOME", "KIRO_HOME", "XDG_CONFIG_HOME"}) {
		t.Fatal("the record does not hold the five folder variables", homes)
	}
	st := f.State()
	target := st.Targets[hooks.SentinelCheckoutKey(f.state)]
	live, err := hooks.SentinelCheckoutFingerprint(f.checkout())
	if err != nil || target.Resource != hooks.SentinelResource || target.Kind != "tree" || target.Applied != live || target.Next != "" || target.Before != "absent" {
		t.Fatal("the checkout is not a recorded tree target", target, live, err)
	}
	if len(st.Pending) != 0 || len(hooks.SentinelLeftovers(f.state)) != 0 {
		t.Fatal("a completed install left a pending mark or a checkout", st.Pending, hooks.SentinelLeftovers(f.state))
	}
}

func TestSentinelTwoReleasesInSequenceLeaveNoPreservedCheckout(t *testing.T) {
	f := newSentinelFixture(t)
	f.install()
	next := f.repository.release("v0.5.0", "# Working rules, second release\n")
	f.api.latest("v0.5.0")
	p := f.provider()
	finding, err := p.Inspect(context.Background(), sentinelResource, engine.Want{Present: true, Update: true}, f)
	if err != nil || finding.Status != engine.Outdated || !strings.Contains(strings.Join(finding.Notes, " "), "Sentinel v0.5.0 is available, and v0.4.0 is installed") {
		t.Fatal("the newer release is not offered", finding, err)
	}
	if err := p.Apply(context.Background(), sentinelResource, finding, f); err != nil {
		t.Fatal(err)
	}
	if after := f.inspect(p, true); after.Status != engine.Satisfied || after.Version != "v0.5.0" {
		t.Fatal("the update did not reach the newer release", after)
	}
	if f.head() != next {
		t.Fatal("the checkout does not hold the second release")
	}
	entries, err := os.ReadDir(filepath.Join(f.state, "sentinel"))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{"checkout", "record.json"}) || len(hooks.SentinelLeftovers(f.state)) != 0 {
		t.Fatal("two releases in sequence left a preserved checkout", names, hooks.SentinelLeftovers(f.state))
	}
	live, err := hooks.SentinelCheckoutFingerprint(f.checkout())
	if target := f.State().Targets[hooks.SentinelCheckoutKey(f.state)]; err != nil || target.Applied != live || target.Next != "" {
		t.Fatal("the record does not match the new checkout", target, err)
	}
	if !strings.Contains(sentinelRead(t, filepath.Join(f.state, "sentinel", "record.json")), `"action": "update"`) {
		t.Fatal("the record does not hold the update")
	}
}

func TestSentinelMovesAsideACheckoutWithoutARecordOneTime(t *testing.T) {
	f := newSentinelFixture(t)
	f.install()
	if err := f.Update(func(s *state.State) error { s.Drop(hooks.SentinelCheckoutKey(f.state)); return nil }); err != nil {
		t.Fatal(err)
	}
	f.repository.release("v0.5.0", "# second\n")
	f.api.latest("v0.5.0")
	p := f.provider()
	finding, err := p.Inspect(context.Background(), sentinelResource, engine.Want{Present: true, Update: true}, f)
	if err != nil || finding.Status != engine.Outdated || !strings.Contains(strings.Join(finding.Notes, " "), "content that setup did not record") {
		t.Fatal("the unrecorded checkout is not reported", finding, err)
	}
	if err := p.Apply(context.Background(), sentinelResource, finding, f); err != nil {
		t.Fatal(err)
	}
	left := hooks.SentinelLeftovers(f.state)
	if len(left) != 1 || !strings.HasPrefix(filepath.Base(left[0]), "checkout.preserved-") || f.repository.run(left[0], "rev-parse", "HEAD") != f.floor {
		t.Fatal("the checkout without a record was not moved aside", left)
	}
	f.repository.release("v0.6.0", "# third\n")
	f.api.latest("v0.6.0")
	p = f.provider()
	finding, err = p.Inspect(context.Background(), sentinelResource, engine.Want{Present: true, Update: true}, f)
	if err != nil || finding.Status != engine.Outdated {
		t.Fatal(finding, err)
	}
	if err := p.Apply(context.Background(), sentinelResource, finding, f); err != nil {
		t.Fatal(err)
	}
	if again := hooks.SentinelLeftovers(f.state); !slices.Equal(again, left) {
		t.Fatal("a recorded checkout was moved aside", again)
	}
}

func TestSentinelCheckoutFingerprintFollowsCommitsAndIgnoresGitWorkFiles(t *testing.T) {
	f := newSentinelFixture(t)
	f.install()
	before, err := hooks.SentinelCheckoutFingerprint(f.checkout())
	if err != nil {
		t.Fatal(err)
	}
	f.repository.run(f.checkout(), "status")
	f.repository.run(f.checkout(), "update-index", "--refresh")
	sentinelWrite(t, filepath.Join(f.checkout(), ".git", "logs", "HEAD"), "a log line\n", 0644)
	sentinelWrite(t, filepath.Join(f.checkout(), ".git", "FETCH_HEAD"), "", 0644)
	if after, err := hooks.SentinelCheckoutFingerprint(f.checkout()); err != nil || after != before {
		t.Fatal("normal Git use changed the fingerprint", err)
	}
	f.repository.run(f.checkout(), "update-ref", "refs/heads/personal", f.floor)
	if after, err := hooks.SentinelCheckoutFingerprint(f.checkout()); err != nil || after == before {
		t.Fatal("a new branch did not change the fingerprint", err)
	}
	f.repository.run(f.checkout(), "update-ref", "-d", "refs/heads/personal")
	if err := os.Remove(filepath.Join(f.checkout(), ".git", "packed-refs")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	sentinelWrite(t, filepath.Join(f.checkout(), "note.txt"), "personal\n", 0644)
	sentinelWrite(t, filepath.Join(f.checkout(), ".git", "stashed"), "personal\n", 0644)
	f.repository.run(f.checkout(), "hash-object", "-w", filepath.Join(f.checkout(), ".git", "stashed"))
	if err := os.Remove(filepath.Join(f.checkout(), "note.txt")); err != nil {
		t.Fatal(err)
	}
	if after, err := hooks.SentinelCheckoutFingerprint(f.checkout()); err != nil || after == before {
		t.Fatal("a new Git object did not change the fingerprint", err)
	}
	if absent, err := hooks.SentinelCheckoutFingerprint(filepath.Join(f.state, "none")); err != nil || absent != targets.Absent {
		t.Fatal(absent, err)
	}
	writer := &targets.Writer{StateDir: f.state, Kinds: hooks.SentinelKinds()}
	live, err := writer.InspectRecord(hooks.SentinelCheckoutKey(f.state), f)
	if err != nil || live.State != targets.StateChanged {
		t.Fatal("the registered kind does not report the changed checkout", live, err)
	}
}

func TestSentinelRemovalDeletesTheRecordAndAMatchingCheckout(t *testing.T) {
	f := newSentinelFixture(t)
	p := f.install()
	finding := f.inspect(p, false)
	if finding.Status != engine.Satisfied {
		t.Fatal("an installed Sentinel is not removable", finding)
	}
	if err := p.Remove(context.Background(), sentinelResource, finding, f); err != nil {
		t.Fatal(err)
	}
	if calls := f.calls(); calls[len(calls)-1] != "--global --hosts codex --skills core,ui,formal,ops --remove" {
		t.Fatal("the removal did not run the Sentinel installer", calls)
	}
	if sentinelExists(filepath.Join(f.state, "sentinel")) || len(f.State().Targets) != 0 || len(f.State().Pending) != 0 {
		t.Fatal("the removal left the checkout, the record or a mark", f.State())
	}
	if after := f.inspect(p, false); after.Status != engine.Absent {
		t.Fatal("a removed Sentinel is not absent", after)
	}
}

func TestSentinelRemovalLeavesAChangedCheckoutAndCheckListsIt(t *testing.T) {
	f := newSentinelFixture(t)
	p := f.install()
	sentinelWrite(t, filepath.Join(f.checkout(), ".git", "stashed"), "personal\n", 0644)
	f.repository.run(f.checkout(), "hash-object", "-w", filepath.Join(f.checkout(), ".git", "stashed"))
	finding := f.inspect(p, false)
	if finding.Status != engine.Satisfied || len(finding.Preserved) != 0 {
		t.Fatal("a checkout that differs from its record keeps Sentinel on the machine", finding)
	}
	if err := p.Remove(context.Background(), sentinelResource, finding, f); err != nil {
		t.Fatal(err)
	}
	if calls := f.calls(); !strings.HasSuffix(calls[len(calls)-1], "--remove") {
		t.Fatal("the removal did not run the Sentinel installer", calls)
	}
	if sentinelExists(filepath.Join(f.state, "sentinel", "record.json")) || len(f.State().Targets) != 0 {
		t.Fatal("the removal left a record", f.State().Targets)
	}
	if left := hooks.SentinelLeftovers(f.state); !slices.Equal(left, []string{f.checkout()}) {
		t.Fatal("check does not list the checkout that the removal left", left)
	}
	if after := f.inspect(p, false); after.Status != engine.Absent {
		t.Fatal("the left checkout keeps Sentinel on the machine", after)
	}
}

func TestSentinelChecksOnlyAVerifiedCheckoutOfTheRecordedCommit(t *testing.T) {
	f := newSentinelFixture(t)
	p := f.install()
	sentinelWrite(t, filepath.Join(f.home, "sentinel-drift"), "", 0600)
	if finding := f.inspect(p, true); finding.Status != engine.Unhealthy || !strings.Contains(finding.Reason, "DRIFT") || strings.Contains(finding.Reason, "ok:") {
		t.Fatal("drift is not a repairable health issue", finding)
	}
	if err := os.Remove(filepath.Join(f.home, "sentinel-drift")); err != nil {
		t.Fatal(err)
	}
	sentinelWrite(t, filepath.Join(f.home, "sentinel-error"), "", 0600)
	if _, err := p.Inspect(context.Background(), sentinelResource, engine.Want{Present: true}, f); err == nil || !strings.Contains(err.Error(), "Sentinel check failed") {
		t.Fatal("an installer error was reported as health", err)
	}
	if err := os.Remove(filepath.Join(f.home, "sentinel-error")); err != nil {
		t.Fatal(err)
	}
	install := filepath.Join(f.checkout(), "tools", "install")
	for name, damage := range map[string]func(){
		"tools/install": func() { sentinelWrite(t, install, "#!/bin/bash\nexit 0\n", 0755) },
		"tools/extra":   func() { sentinelWrite(t, filepath.Join(f.checkout(), "tools", "extra"), "", 0644) },
		"executable":    func() { sentinelWrite(t, install, sentinelInstallScript, 0644) },
	} {
		checks := len(f.calls())
		damage()
		finding := f.inspect(p, true)
		if finding.Status != engine.Unhealthy || !strings.Contains(finding.Reason, name) || len(f.calls()) != checks {
			t.Fatal("setup ran Sentinel from a checkout that is not the recorded commit", name, finding)
		}
		if err := p.Apply(context.Background(), sentinelResource, finding, f); err != nil {
			t.Fatal(name, err)
		}
		if after := f.inspect(p, true); after.Status != engine.Satisfied || f.head() != f.floor {
			t.Fatal("the repair did not fetch the recorded commit again", name, after)
		}
	}
	if left := hooks.SentinelLeftovers(f.state); len(left) != 3 {
		t.Fatal("each edited checkout stays for the owner", left)
	}
}

func TestSentinelVerificationIgnoresGitIndexFlagsFiltersAndReplacements(t *testing.T) {
	f := newSentinelFixture(t)
	p := f.install()
	git := func(arguments ...string) string { return f.repository.run(f.checkout(), arguments...) }
	rules := filepath.Join(f.checkout(), "rules", "AGENTS.md")
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		git("update-index", flag, "rules/AGENTS.md")
		sentinelWrite(t, rules, "# edited\n", 0644)
		if status := git("status", "--porcelain"); status != "" {
			t.Fatal("the fixture flag does not hide the edit from git status", status)
		}
		if finding := f.inspect(p, true); finding.Status != engine.Unhealthy || !strings.Contains(finding.Reason, "rules/AGENTS.md has other content") {
			t.Fatal("an edited file with an index flag verified", flag, finding)
		}
		sentinelWrite(t, rules, "# Working rules\n", 0644)
		git("update-index", strings.Replace(flag, "--", "--no-", 1), "rules/AGENTS.md")
	}
	pinned := filepath.Join(t.TempDir(), "pinned")
	sentinelWrite(t, pinned, "# Working rules\n", 0644)
	sentinelWrite(t, filepath.Join(f.checkout(), ".git", "info", "attributes"), "rules/AGENTS.md filter=hide\n", 0644)
	git("config", "filter.hide.clean", "cat '"+filepath.ToSlash(pinned)+"'")
	sentinelWrite(t, rules, "# changed\n", 0644)
	if git("hash-object", "rules/AGENTS.md") != git("rev-parse", f.floor+":rules/AGENTS.md") {
		t.Fatal("the fixture filter does not give the edited file the pinned hash")
	}
	if finding := f.inspect(p, true); finding.Status != engine.Unhealthy || !strings.Contains(finding.Reason, "rules/AGENTS.md") {
		t.Fatal("a clean filter hid an edited file", finding)
	}
	git("config", "--unset", "filter.hide.clean")
	sentinelWrite(t, rules, "# replaced\n", 0644)
	git("commit", "--quiet", "-am", "replacement")
	git("replace", f.floor, git("rev-parse", "HEAD"))
	git("update-ref", "--no-deref", "HEAD", f.floor)
	if git("ls-tree", f.floor, "rules/AGENTS.md") == git("--no-replace-objects", "ls-tree", f.floor, "rules/AGENTS.md") {
		t.Fatal("the fixture replacement does not change the pinned tree")
	}
	if finding := f.inspect(p, true); finding.Status != engine.Unhealthy || !strings.Contains(finding.Reason, "rules/AGENTS.md") {
		t.Fatal("a replacement object hid an edited file", finding)
	}
}

func TestSentinelInstallerCommandFollowsPlatformAndHosts(t *testing.T) {
	for _, target := range []platform.Context{{OS: "linux", Arch: "amd64"}, {OS: "darwin", Arch: "arm64"}, {OS: "windows", Arch: "amd64"}} {
		t.Run(target.OS, func(t *testing.T) {
			f := newSentinelFixture(t)
			f.runner.fake = true
			config, kiro := filepath.Join(f.home, "xdg"), filepath.Join(filepath.Dir(f.home), "work kiro")
			p := f.provider()
			p.Context, p.Shell = target, "/fake/shell"
			p.Query = func(ctx context.Context, c platform.Command) ([]byte, error) {
				if c.Path == "/fake/shell" {
					return nil, nil
				}
				return hooks.SentinelQuery(ctx, c)
			}
			p.Locations = hooks.SentinelLocations{CodexHome: filepath.Join(f.home, "codex home"), ConfigHome: config, KiroHome: kiro, Installed: []string{"claude"}}
			p.Wanted = func() []string { return []string{"tool.kiro"} }
			sentinelWrite(t, filepath.Join(p.Locations.CodexHome, "config.toml"), "", 0600)
			sentinelWrite(t, filepath.Join(config, "opencode", "opencode.json"), "{}", 0600)
			if err := f.apply(p); err != nil {
				t.Fatal(err)
			}
			want := []string{filepath.Join(f.checkout(), "tools", "install"), "--global", "--hosts", "codex,claude,kiro,opencode", "--skills", "core,ui,formal,ops"}
			if target.OS == "windows" {
				want = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(f.checkout(), "tools", "install.ps1"),
					"-Global", "-Hosts", "codex,claude,kiro,opencode", "-Skills", "core,ui,formal,ops"}
			}
			if len(f.runner.installers) != 1 || f.runner.installers[0].Path != "/fake/shell" || !slices.Equal(f.runner.installers[0].Args, want) {
				t.Fatal("unexpected installer command", f.runner.installers)
			}
			for _, value := range []string{"HOME=" + f.home, "CODEX_HOME=" + p.Locations.CodexHome, "XDG_CONFIG_HOME=" + config, "COPILOT_HOME=", "CLAUDE_CONFIG_DIR=", "KIRO_HOME=" + kiro} {
				if !slices.Contains(f.runner.installers[0].Env, value) {
					t.Fatal("the installer does not get the folder variable unchanged", value, f.runner.installers[0].Env)
				}
			}
			finding := f.inspect(p, false)
			if err := p.Remove(context.Background(), sentinelResource, finding, f); err != nil {
				t.Fatal(err)
			}
			last := f.runner.installers[len(f.runner.installers)-1].Args
			if mode := last[len(last)-1]; mode != map[bool]string{true: "-Remove", false: "--remove"}[target.OS == "windows"] {
				t.Fatal("unexpected removal mode", last)
			}
		})
	}
}

func TestSentinelRemovesWithTheFoldersOfTheInstall(t *testing.T) {
	f := newSentinelFixture(t)
	f.runner.fake = true
	p := f.provider()
	p.Query = func(ctx context.Context, c platform.Command) ([]byte, error) {
		if c.Path == "/bin/bash" {
			return nil, nil
		}
		return hooks.SentinelQuery(ctx, c)
	}
	p.Locations = hooks.SentinelLocations{CodexHome: filepath.Join(f.home, "first codex")}
	sentinelWrite(t, filepath.Join(p.Locations.CodexHome, "config.toml"), "", 0600)
	if err := f.apply(p); err != nil {
		t.Fatal(err)
	}
	p.Locations = hooks.SentinelLocations{CodexHome: filepath.Join(f.home, "second codex")}
	finding := f.inspect(p, true)
	if finding.Status != engine.Satisfied || !strings.Contains(strings.Join(finding.Notes, " "), "Setup keeps the recorded values CODEX_HOME="+filepath.Join(f.home, "first codex")) {
		t.Fatal("a changed folder variable is not reported", finding)
	}
	if err := p.Remove(context.Background(), sentinelResource, f.inspect(p, false), f); err != nil {
		t.Fatal(err)
	}
	last := f.runner.installers[len(f.runner.installers)-1]
	if !slices.Contains(last.Env, "CODEX_HOME="+filepath.Join(f.home, "first codex")) || !slices.Contains(last.Args, "--remove") {
		t.Fatal("the removal does not use the folders of the install", last)
	}
}

func TestSentinelRefusesAKiroHomeThatKiroIgnoresAndNeedsAnAgent(t *testing.T) {
	for _, value := range []string{"relative kiro", string(filepath.Separator)} {
		f := newSentinelFixture(t)
		p := f.provider()
		p.Locations = hooks.SentinelLocations{KiroHome: value}
		p.Wanted = func() []string { return []string{"tool.kiro"} }
		if finding := f.inspect(p, true); finding.Status != engine.Blocked || !strings.Contains(finding.Reason, "Kiro ignores KIRO_HOME") {
			t.Fatal("an ignored KIRO_HOME does not block Sentinel", value, finding)
		}
		if err := f.apply(p); err == nil || len(f.runner.installers) != 0 || sentinelExists(f.checkout()) {
			t.Fatal("Sentinel ran with an ignored KIRO_HOME", value, err)
		}
	}
	f := newSentinelFixture(t)
	p := f.provider()
	if finding := f.inspect(p, true); finding.Status != engine.Blocked || !strings.Contains(finding.Reason, "Sentinel found no coding agent") {
		t.Fatal("Sentinel without an agent is not blocked", finding)
	}
	p.Wanted = func() []string { return []string{"tool.opencode"} }
	if err := f.apply(p); err != nil {
		t.Fatal(err)
	}
	if calls := f.calls(); calls[0] != "--global --hosts opencode --skills core,ui,formal,ops" {
		t.Fatal("a selected OpenCode is not a Sentinel host", calls)
	}
}

func TestSentinelAsksGitHubForAnImmutableRelease(t *testing.T) {
	f := newSentinelFixture(t)
	sentinelWrite(t, filepath.Join(f.home, ".codex", "config.toml"), "", 0600)
	f.api.commits = map[string]string{"v0.5.0": strings.Repeat("4", 40), "v0.2.0": strings.Repeat("2", 40)}
	for release, want := range map[string]string{
		`{"tag_name":"v0.5.0","draft":false,"prerelease":false,"immutable":true}`:     "",
		`{"tag_name":"v0.5.0","draft":false,"prerelease":false,"immutable":false}`:    "not a published and immutable",
		`{"tag_name":"v0.5.0","draft":false,"prerelease":true,"immutable":true}`:      "not a published and immutable",
		`{"tag_name":"v0.5.0","draft":true,"prerelease":false,"immutable":true}`:      "not a published and immutable",
		`{"tag_name":"v0.5.0-rc1","draft":false,"prerelease":false,"immutable":true}`: "not a published and immutable",
		`{"tag_name":"v0.2.0","draft":false,"prerelease":false,"immutable":true}`:     "older than v0.4.0",
	} {
		f.api.release = release
		finding := f.inspect(f.provider(), true)
		switch {
		case want == "" && finding.Status != engine.Missing:
			t.Fatal("setup did not take the latest release", finding)
		case want != "" && (finding.Status != engine.Blocked || !strings.Contains(finding.Reason, want)):
			t.Fatal("setup accepted a release that it must refuse", release, finding)
		}
	}
}

type sentinelTransport func(*http.Request) (*http.Response, error)

func (f sentinelTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSentinelSendsTheGitHubTokenOnlyToTheGitHubAPI(t *testing.T) {
	tags := strings.TrimSuffix(hooks.SentinelReleases, "/releases/latest") + "/commits/refs/tags/v0.5.0"
	for _, token := range []string{"", "fixture-token"} {
		sent := map[string]string{}
		f := newSentinelFixture(t)
		sentinelWrite(t, filepath.Join(f.home, ".codex", "config.toml"), "", 0600)
		p := f.provider()
		p.Releases, p.Token = hooks.SentinelReleases, token
		p.Client = &http.Client{Transport: sentinelTransport(func(r *http.Request) (*http.Response, error) {
			sent[r.URL.String()] = r.Header.Get("Authorization")
			status, header, body := http.StatusOK, http.Header{}, ""
			switch r.URL.String() {
			case hooks.SentinelReleases:
				status = http.StatusFound
				header.Set("Location", "https://uploads.api.github.com/latest")
			case "https://uploads.api.github.com/latest":
				body = `{"tag_name":"v0.5.0","draft":false,"prerelease":false,"immutable":true}`
			case tags:
				body = strings.Repeat("4", 40)
			default:
				status = http.StatusNotFound
			}
			return &http.Response{StatusCode: status, Status: fmt.Sprint(status), Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})}
		if finding := f.inspect(p, true); finding.Status != engine.Missing {
			t.Fatal("setup did not take the latest release", finding)
		}
		bearer := ""
		if token != "" {
			bearer = "Bearer " + token
		}
		if want := map[string]string{hooks.SentinelReleases: bearer, "https://uploads.api.github.com/latest": "", tags: bearer}; !maps.Equal(sent, want) {
			t.Fatal("setup sent the wrong Authorization headers", sent)
		}
	}
}

func TestSentinelKeepsTheInstalledReleaseWithoutAValidNewerOne(t *testing.T) {
	f := newSentinelFixture(t)
	second := f.repository.release("v0.5.0", "# second\n")
	f.api.latest("v0.5.0")
	f.install()
	for want, change := range map[string]func(){
		"the latest release v0.4.0 is older than the installed release v0.5.0; setup refuses a downgrade": func() { f.api.latest("v0.4.0") },
		"the tag v0.5.0 now names the commit " + f.floor: func() {
			f.api.latest("v0.5.0")
			f.api.commits = map[string]string{"v0.4.0": f.floor, "v0.5.0": f.floor}
		},
		"Setup could not check for a newer Sentinel release": func() { f.api.release = "not json" },
	} {
		change()
		finding := f.inspect(f.provider(), true)
		notes := strings.Join(finding.Notes, " ")
		if finding.Status != engine.Satisfied || !strings.Contains(notes, want) || !strings.Contains(notes, "It keeps v0.5.0.") || f.head() != second {
			t.Fatal("setup did not keep the installed release", want, finding)
		}
	}
}

func TestSentinelPendingMarkCoversTheInstallerCall(t *testing.T) {
	f := newSentinelFixture(t)
	sentinelWrite(t, filepath.Join(f.home, ".codex", "config.toml"), "", 0600)
	p := f.provider()
	f.runner.fail = func(platform.Command) error {
		if marks := f.State().Pending; !slices.Equal(marks, []string{"integration.sentinel-installer:install"}) {
			t.Fatal("the installer started without its pending mark", marks)
		}
		return errors.New("fixture: the command did not start")
	}
	if err := f.apply(p); err == nil || !strings.Contains(err.Error(), "did not start") || len(f.State().Pending) != 0 {
		t.Fatal("a command that did not start left its mark", err, f.State().Pending)
	}
	f.runner.fail = nil
	sentinelWrite(t, filepath.Join(f.home, "sentinel-fail"), "", 0600)
	if err := f.apply(p); err == nil || !slices.Equal(f.State().Pending, []string{"integration.sentinel-installer:repair"}) {
		t.Fatal("a failed installer did not keep its mark", err, f.State().Pending)
	}
	if finding := f.inspect(p, true); finding.Status != engine.Unhealthy || !strings.Contains(finding.Reason, "did not finish; apply runs the Sentinel installer again") {
		t.Fatal("an unfinished install is not reported", finding)
	}
	if err := os.Remove(filepath.Join(f.home, "sentinel-fail")); err != nil {
		t.Fatal(err)
	}
	if err := f.Update(func(s *state.State) error { s.Clear("integration.sentinel-installer:repair"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.apply(p); err != nil {
		t.Fatal(err)
	}
	if finding := f.inspect(p, true); finding.Status != engine.Satisfied || len(f.State().Pending) != 0 || len(hooks.SentinelLeftovers(f.state)) != 0 {
		t.Fatal("the second run did not complete the install", finding, f.State().Pending)
	}
}

func TestSentinelRepairsAndRemovesAnInstallOlderThanTheFloor(t *testing.T) {
	f := newSentinelFixture(t)
	f.install()
	next := f.repository.release("v0.5.0", "# second\n")
	f.api.latest("v0.5.0")
	p := f.provider()
	p.Floor = hooks.SentinelRelease{Tag: "v0.5.0", Commit: next}
	checks := len(f.calls())
	finding := f.inspect(p, true)
	if finding.Status != engine.Unhealthy || !strings.Contains(finding.Reason, "is older than v0.5.0, the oldest release that setup supports; repair installs v0.5.0") || len(f.calls()) != checks {
		t.Fatal("an install older than the floor is not a health issue", finding)
	}
	older := f.provider()
	older.Floor = p.Floor
	if err := older.Remove(context.Background(), sentinelResource, f.inspect(older, false), f); err != nil {
		t.Fatal(err)
	}
	if calls := f.calls(); !strings.HasSuffix(calls[len(calls)-1], "--remove") || sentinelExists(filepath.Join(f.state, "sentinel")) {
		t.Fatal("the removal of an older install did not use the floor release", calls)
	}
}

func TestSentinelLookupFailureBlocksOnlyAFirstInstall(t *testing.T) {
	f := newSentinelFixture(t)
	sentinelWrite(t, filepath.Join(f.home, ".codex", "config.toml"), "", 0600)
	p := f.provider()
	p.Latest = func(context.Context) (hooks.SentinelRelease, error) {
		return hooks.SentinelRelease{}, errors.New("offline")
	}
	if finding := f.inspect(p, true); finding.Status != engine.Blocked || !strings.Contains(finding.Reason, "Setup cannot find the latest Sentinel release: offline") {
		t.Fatal("a failed lookup does not block the first install", finding)
	}
	if err := f.apply(p); err == nil || sentinelExists(filepath.Join(f.state, "sentinel")) {
		t.Fatal("setup installed without a release", err)
	}
}

func TestSentinelRecordWithoutOwnershipMakesAnImportedCheckoutRemovable(t *testing.T) {
	f := newSentinelFixture(t)
	p := f.install()
	live, err := hooks.SentinelCheckoutFingerprint(f.checkout())
	if err != nil {
		t.Fatal(err)
	}
	key := hooks.SentinelCheckoutKey(f.state)
	if err := f.Update(func(s *state.State) error {
		s.Targets[key] = state.Record{Resource: hooks.SentinelResource, Kind: "tree", Before: "fp:" + live}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if finding := f.inspect(p, true); finding.Status != engine.Satisfied {
		t.Fatal("an imported checkout is not usable", finding)
	}
	if err := p.Remove(context.Background(), sentinelResource, f.inspect(p, false), f); err != nil {
		t.Fatal(err)
	}
	if len(f.State().Targets) != 0 || !slices.Equal(hooks.SentinelLeftovers(f.state), []string{f.checkout()}) || sentinelExists(filepath.Join(f.state, "sentinel", "record.json")) {
		t.Fatal("the removal deleted a checkout without ownership, or left a record", f.State().Targets, hooks.SentinelLeftovers(f.state))
	}
}

func TestSentinelFinishesTheCheckoutRecordAfterAnInterruptedPublication(t *testing.T) {
	f := newSentinelFixture(t)
	p := f.install()
	key := hooks.SentinelCheckoutKey(f.state)
	live := f.State().Targets[key].Applied
	if err := f.Update(func(s *state.State) error {
		s.Targets[key] = state.Record{Resource: hooks.SentinelResource, Kind: "tree", Next: live, Before: "absent"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background(), sentinelResource, engine.Finding{Status: engine.Unhealthy}, f); err != nil {
		t.Fatal(err)
	}
	if record := f.State().Targets[key]; record.Applied != live || record.Next != "" || len(hooks.SentinelLeftovers(f.state)) != 0 {
		t.Fatal("the checkout record stays unfinished after the next run", record)
	}
}
