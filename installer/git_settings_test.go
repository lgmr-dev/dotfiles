package installer

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func gitSettingsFixture(t *testing.T) (Controller, string, string) {
	t.Helper()
	root, err := resolveConfigPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, repository := filepath.Join(root, "home"), filepath.Join(root, "checkout with space ü")
	defaults, err := os.ReadFile(filepath.Join("..", "git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(repository, "git", "config")
	writeConfigFixture(t, source, string(defaults))
	hooks, err := os.ReadDir(filepath.Join("..", "git", "hooks"))
	if err != nil {
		t.Fatal(err)
	}
	for _, hook := range hooks {
		script, err := os.ReadFile(filepath.Join("..", "git", "hooks", hook.Name()))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(repository, "git", "hooks", hook.Name())
		writeConfigFixture(t, path, string(script))
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS == "windows" {
		fragment, err := os.ReadFile(filepath.Join("..", "windows-terminal", "settings.fragment.jsonc"))
		if err != nil {
			t.Fatal(err)
		}
		writeConfigFixture(t, filepath.Join(repository, "windows-terminal", "settings.fragment.jsonc"), string(fragment))
	}
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	folders := ConfigFolders{Home: home, Config: filepath.Join(home, ".config"), LocalAppData: filepath.Join(home, "local"), AppData: filepath.Join(home, "roaming"), Documents: filepath.Join(home, "documents")}
	c, err := NewNativeController(repository, "git-settings-fixture", filepath.Join(home, "state"), nativePlatform(t), folders)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &Catalog{Schema: 1}
	for _, resource := range c.Catalog.Resources {
		if slices.Contains([]string{"git", "sentinel-no-attribution", "git-privacy-guard", "integration.git"}, resource.ID) {
			resource.Requires = slices.DeleteFunc(slices.Clone(resource.Requires), func(id string) bool { return id != "integration.git" })
			catalog.Resources = append(catalog.Resources, resource)
		}
	}
	c.Catalog = catalog
	return c, source, filepath.Join(folders.Config, "git", "config")
}

func gitSettingsCommand(t *testing.T, home, directory string, args ...string) string {
	t.Helper()
	output, err := gitSettingsRun(home, directory, "fixture@example.invalid", args...)
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, output, err)
	}
	return output
}

func gitSettingsRun(home, directory, address string, args ...string) (string, error) {
	command := exec.Command("git", args...)
	command.Dir = directory
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") && !slices.Contains([]string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME"}, strings.ToUpper(key)) {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "HOME="+home, "USERPROFILE="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL="+address, "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL="+address)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func TestGitGlobalWritesNeverModifyTheCheckout(t *testing.T) {
	c, source, target := gitSettingsFixture(t)
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
	gitSettingsCommand(t, c.Home, c.Home, "config", "--global", "user.email", "fixture@example.invalid")
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("git config --global wrote personal settings into the checkout", err)
	}
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatal("Git's writable global configuration is not a regular file", info, err)
	}
	check, err := c.Dispatch(context.Background(), Request{Schema: 1, Mode: "check"})
	if err != nil || check.Status != "ready" {
		t.Fatal("personal Git settings invalidated managed defaults", check, err)
	}
	dispatchApproved(t, c, Request{Schema: 1, Mode: "update"})
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{}})
	if got := gitSettingsCommand(t, c.Home, c.Home, "config", "--global", "user.email"); got != "fixture@example.invalid" {
		t.Fatal("removal lost personal Git settings", got)
	}
}

func TestGitDefaultsRebasePullAndPruneFetch(t *testing.T) {
	c, _, _ := gitSettingsFixture(t)
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
	for _, key := range []string{"pull.rebase", "fetch.prune"} {
		if got := gitSettingsCommand(t, c.Home, c.Home, "config", "--bool", "--get", key); got != "true" {
			t.Fatal("missing Git default", key, got)
		}
	}
}

func TestGitRemovalWorksWhenDefaultsSourceIsMissing(t *testing.T) {
	c, source, _ := gitSettingsFixture(t)
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
	gitSettingsCommand(t, c.Home, c.Home, "config", "--global", "user.email", "fixture@example.invalid")
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{}})
	if got := gitSettingsCommand(t, c.Home, c.Home, "config", "--global", "user.email"); got != "fixture@example.invalid" {
		t.Fatal(got)
	}
}

func TestGitLockFinalizationPreservesForeignLockFile(t *testing.T) {
	root, err := resolveConfigPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "config")
	writeConfigFixture(t, target+".lock", "in-progress native Git write")
	journal := profileJournal{Resource: gitSettingsResource, Operation: strings.Repeat("a", 64), Entries: []profilePublication{{Path: target}}}
	if err := finishProfileSettingsLocks(root, journal); err != nil {
		t.Fatal("foreign Git lock prevented finalization", err)
	}
	got, err := os.ReadFile(target + ".lock")
	if err != nil || string(got) != "in-progress native Git write" {
		t.Fatal("finalization changed a foreign Git lock", string(got), err)
	}
}

func TestGitIncludeEditsRequireReviewAndRemainPreserved(t *testing.T) {
	c, source, target := gitSettingsFixture(t)
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
	personal := filepath.Join(c.Home, "extra.gitconfig")
	writeConfigFixture(t, personal, "[user]\n\temail = include@example.invalid\n")
	gitSettingsCommand(t, c.Home, c.Home, "config", "--global", "--add", "include.path", personal)
	check, err := c.Dispatch(context.Background(), Request{Schema: 1, Mode: "check"})
	if err != nil || check.Status == "ready" {
		t.Fatal("edited managed include was not detected", check, err)
	}
	edited, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{}})
	retained, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(edited, retained) {
		t.Fatal("removal did not preserve the edited include", err)
	}
	defaults, err := os.ReadFile(source)
	if err != nil || bytes.Contains(defaults, []byte("include@example.invalid")) {
		t.Fatal("include edit wrote into the checkout", err)
	}
}

func TestGitDefaultIncludePreservesPersonalPrecedence(t *testing.T) {
	for _, location := range []string{"xdg", "home"} {
		t.Run(location, func(t *testing.T) {
			c, _, target := gitSettingsFixture(t)
			personal := target
			if location == "home" {
				personal = filepath.Join(c.Home, ".gitconfig")
			}
			original := "[pull]\n\trebase = false\n[fetch]\n\tprune = false\n"
			writeConfigFixture(t, personal, original)
			dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
			for _, key := range []string{"pull.rebase", "fetch.prune"} {
				if got := gitSettingsCommand(t, c.Home, c.Home, "config", "--bool", "--get", key); got != "false" {
					t.Fatal("managed defaults overrode personal Git settings", key, got)
				}
			}
			dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{}})
			got, err := os.ReadFile(personal)
			if err != nil || string(got) != original {
				t.Fatal("Git removal changed the original personal configuration", runtime.GOOS, err)
			}
		})
	}
}

func TestGitSentinelAttributionFollowsTheSelection(t *testing.T) {
	c, _, _ := gitSettingsFixture(t)
	attribution := func() string {
		return gitSettingsCommand(t, c.Home, c.Home, "config", "--default", "unset", "--get", "sentinel.attribution")
	}
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git", "sentinel-no-attribution"}})
	if got := attribution(); got != "off" {
		t.Fatal("the selection did not turn Sentinel attribution off", got)
	}
	check, err := c.Dispatch(context.Background(), Request{Schema: 1, Mode: "check"})
	if err != nil || check.Status != "ready" {
		t.Fatal("check rejected the applied attribution setting", check, err)
	}
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
	if got := attribution(); got != "unset" {
		t.Fatal("unticking kept Sentinel attribution off", got)
	}
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"sentinel-no-attribution"}})
	if got := attribution(); got != "off" {
		t.Fatal("ticking after an existing Git install kept the attribution", got)
	}
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{}})
	if got := attribution(); got != "unset" {
		t.Fatal("removal kept Sentinel attribution off", got)
	}
}

func TestGitPrivacyGuardStopsPersonalAddressesAndRunsOwnHooks(t *testing.T) {
	c, source, _ := gitSettingsFixture(t)
	setting := func(key string) string {
		return gitSettingsCommand(t, c.Home, c.Home, "config", "--global", "--default", "unset", "--get", key)
	}
	ready := func() bool {
		check, err := c.Dispatch(context.Background(), Request{Schema: 1, Mode: "check"})
		return err == nil && check.Status == "ready"
	}
	ownHook := func(path, script string) {
		writeConfigFixture(t, path, "#!/bin/sh\n"+script+"\n")
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	const personal = "alice@mail.example-isp.net"
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git", "git-privacy-guard"}})
	if setting("user.useConfigOnly") != "true" || setting("core.hooksPath") == "unset" || !ready() {
		t.Fatal("the selection did not set a ready identity rule and hooks path")
	}
	for _, name := range []string{"guard", "pre-commit", "pre-push"} {
		hook := filepath.Join(filepath.Dir(source), "hooks", name)
		if err := os.Rename(hook, hook+".moved"); err != nil {
			t.Fatal(err)
		}
		if ready() {
			t.Fatal("check accepted a guard without its hook", name)
		}
		if err := os.Mkdir(hook, 0o700); err != nil {
			t.Fatal(err)
		}
		if ready() {
			t.Fatal("check accepted a folder in place of the hook", name)
		}
		if err := os.Remove(hook); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(hook+".moved", hook); err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" {
			if err := os.Chmod(hook, 0o600); err != nil {
				t.Fatal(err)
			}
			if ready() {
				t.Fatal("check accepted a hook that is not executable", name)
			}
			if err := os.Chmod(hook, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	remote, clone := filepath.Join(c.Home, "remote.git"), filepath.Join(c.Home, "clone")
	gitSettingsCommand(t, c.Home, c.Home, "init", "--bare", "--initial-branch=main", remote)
	gitSettingsCommand(t, c.Home, c.Home, "clone", remote, clone)
	marker, pushed := filepath.Join(c.Home, "own-hook-ran"), filepath.Join(c.Home, "own-pre-push-input")
	ownHook(filepath.Join(clone, ".git", "hooks", "pre-commit"), "echo ran > '"+filepath.ToSlash(marker)+"'")
	ownHook(filepath.Join(clone, ".git", "hooks", "pre-push"), "cat > '"+filepath.ToSlash(pushed)+"'")
	if output, err := gitSettingsRun(c.Home, clone, personal, "commit", "--allow-empty", "-m", "personal"); err == nil || !strings.Contains(output, "Git privacy guard") {
		t.Fatal("the guard accepted a commit with a personal address", output)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the own hook ran after the guard refused the commit")
	}
	gitSettingsCommand(t, c.Home, clone, "commit", "--allow-empty", "-m", "public\n\nCo-Authored-By: Bot <noreply@anthropic.com>")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the guard did not run the own hook of the repository", err)
	}
	gitSettingsCommand(t, c.Home, clone, "push", "origin", "main")
	if input, err := os.ReadFile(pushed); err != nil || !strings.Contains(string(input), "refs/heads/main") {
		t.Fatal("the own pre-push hook did not get the pushed refs", string(input), err)
	}
	for _, step := range []struct {
		name, address string
		args          []string
	}{
		{"personal author", personal, []string{"-c", "core.hooksPath=.git/hooks", "commit", "--allow-empty", "-m", "personal"}},
		{"personal trailer", "fixture@example.invalid", []string{"commit", "--allow-empty", "-m", "public\n\nSigned-off-by: A <" + personal + ">"}},
		{"bare upper-case trailer", "fixture@example.invalid", []string{"commit", "--allow-empty", "-m", "public\n\nSIGNED-OFF-BY: " + personal}},
		{"trailer with two addresses", "fixture@example.invalid", []string{"commit", "--allow-empty", "-m", "public\n\nSigned-off-by: A <" + personal + "> <noreply@anthropic.com>"}},
		{"personal author name", "fixture@example.invalid", []string{"-c", "core.hooksPath=.git/hooks", "commit", "--allow-empty", "--author", "A " + personal + " <fixture@example.invalid>", "-m", "public"}},
		{"personal Cc trailer", "fixture@example.invalid", []string{"commit", "--allow-empty", "-m", "public\n\nCc: A <" + personal + ">"}},
		{"personal address in the body", "fixture@example.invalid", []string{"commit", "--allow-empty", "-m", "public\n\nAsk " + personal + " for details."}},
		{"Unicode local part in the body", "fixture@example.invalid", []string{"commit", "--allow-empty", "-m", "public\n\nAsk jos\u00e9@personal-isp.de for details."}},
	} {
		if output, err := gitSettingsRun(c.Home, clone, step.address, step.args...); err != nil {
			t.Fatal("a commit before the push failed", step.name, output, err)
		}
		if output, err := gitSettingsRun(c.Home, clone, "fixture@example.invalid", "push", "origin", "main"); err == nil || !strings.Contains(output, "Git privacy guard") {
			t.Fatal("the guard pushed a commit with a", step.name, output)
		}
		gitSettingsCommand(t, c.Home, clone, "reset", "--hard", "origin/main")
	}
	refused := func(name string, args ...string) {
		t.Helper()
		if output, err := gitSettingsRun(c.Home, clone, "fixture@example.invalid", args...); err == nil || !strings.Contains(output, "Git privacy guard") {
			t.Fatal("the guard pushed", name, output)
		}
	}
	refused("a personal ref name", "push", "origin", "HEAD:refs/heads/"+personal)
	gitSettingsCommand(t, c.Home, clone, "tag", "-a", "noted", "-m", "Ask "+personal)
	refused("a personal tag message", "push", "origin", "noted")
	gitSettingsCommand(t, c.Home, clone, "tag", "-a", personal, "-m", "release")
	gitSettingsCommand(t, c.Home, clone, "tag", "renamed", personal)
	refused("a renamed tag with a personal name", "push", "origin", "renamed")
	if keygen, err := exec.LookPath("ssh-keygen"); err == nil {
		key := filepath.Join(c.Home, "signing-key")
		if output, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
			t.Fatal(string(output), err)
		}
		for _, side := range []struct{ name, tagger string }{{"first", "fixture@example.invalid"}, {"second", personal}} {
			gitSettingsCommand(t, c.Home, clone, "checkout", "-q", "-b", side.name, "main")
			gitSettingsCommand(t, c.Home, clone, "commit", "--allow-empty", "-m", side.name)
			if output, err := gitSettingsRun(c.Home, clone, side.tagger, "-c", "gpg.format=ssh", "-c", "user.signingkey="+key, "tag", "-s", side.name+"-tag", "-m", side.name); err != nil {
				t.Fatal(output, err)
			}
		}
		gitSettingsCommand(t, c.Home, clone, "checkout", "-q", "main")
		gitSettingsCommand(t, c.Home, clone, "merge", "-q", "--no-ff", "--no-edit", "first-tag", "second-tag")
		refused("a second merged signed tag with a personal tagger", "-c", "log.showSignature=true", "push", "origin", "HEAD:refs/heads/merged")
		gitSettingsCommand(t, c.Home, clone, "reset", "--hard", "origin/main")
	}
	certificate := base64.StdEncoding.EncodeToString([]byte("\x00subject emailAddress\x16" + personal + "\x00"))
	now := fmt.Sprintf("%d +0000", time.Now().Unix())
	parent := gitSettingsCommand(t, c.Home, clone, "rev-parse", "HEAD")
	for name, header := range map[string][]string{
		"an S/MIME signature":         {"gpgsig -----BEGIN SIGNED MESSAGE-----", " " + certificate, " -----END SIGNED MESSAGE-----"},
		"a PGP signature":             {"gpgsig -----BEGIN PGP SIGNATURE-----", " ", " " + certificate, " =AbCd", " -----END PGP SIGNATURE-----"},
		"a signature of a merged tag": {"mergetag object " + parent, " type commit", " tag v1", " tagger Fixture <fixture@example.invalid> " + now, " ", " release", " -----BEGIN SIGNED MESSAGE-----", " " + certificate, " -----END SIGNED MESSAGE-----"},
	} {
		lines := append([]string{
			"tree " + gitSettingsCommand(t, c.Home, clone, "rev-parse", "HEAD^{tree}"),
			"parent " + parent,
			"author Fixture <fixture@example.invalid> " + now,
			"committer Fixture <fixture@example.invalid> " + now,
		}, header...)
		object := filepath.Join(c.Home, "signed-commit")
		writeConfigFixture(t, object, strings.Join(append(lines, "", "signed", ""), "\n"))
		commit := gitSettingsCommand(t, c.Home, clone, "hash-object", "-t", "commit", "-w", object)
		refused(name+" with a personal address", "push", "origin", commit+":refs/heads/signed")
	}
	gitSettingsCommand(t, c.Home, clone, "tag", "tree", "HEAD^{tree}")
	refused("a tag of a tree", "push", "origin", "tree")
	if output, err := gitSettingsRun(c.Home, clone, personal, "-c", "core.hooksPath=.git/hooks", "commit", "--allow-empty", "-m", "replaced"); err != nil {
		t.Fatal(output, err)
	}
	clean := gitSettingsCommand(t, c.Home, clone, "commit-tree", "HEAD^{tree}", "-p", "HEAD~1", "-m", "replaced")
	gitSettingsCommand(t, c.Home, clone, "replace", "HEAD", clean)
	refused("a commit behind a replacement object", "push", "origin", "HEAD:refs/heads/replaced")
	gitSettingsCommand(t, c.Home, clone, "replace", "-d", "HEAD")
	gitSettingsCommand(t, c.Home, clone, "reset", "--hard", "origin/main")
	if output, err := gitSettingsRun(c.Home, clone, personal, "tag", "-a", "inner", "-m", "inner"); err != nil {
		t.Fatal(output, err)
	}
	gitSettingsCommand(t, c.Home, clone, "tag", "-a", "outer", "inner", "-m", "outer")
	if output, err := gitSettingsRun(c.Home, clone, "fixture@example.invalid", "push", "origin", "outer"); err == nil || !strings.Contains(output, "Git privacy guard") {
		t.Fatal("the guard pushed a tag of a tag", output)
	}
	gitSettingsCommand(t, c.Home, clone, "checkout", "-q", "main")
	if output, err := gitSettingsRun(c.Home, clone, personal, "-c", "core.hooksPath=.git/hooks", "commit", "--allow-empty", "-m", "published personal"); err != nil {
		t.Fatal(output, err)
	}
	gitSettingsCommand(t, c.Home, clone, "-c", "core.hooksPath=.git/hooks", "push", "origin", "main")
	other := filepath.Join(c.Home, "other")
	gitSettingsCommand(t, c.Home, c.Home, "clone", remote, other)
	gitSettingsCommand(t, c.Home, other, "commit", "--allow-empty", "-m", "moved")
	gitSettingsCommand(t, c.Home, other, "push", "origin", "main")
	gitSettingsCommand(t, c.Home, clone, "checkout", "-q", "-b", "stale", "main")
	gitSettingsCommand(t, c.Home, clone, "commit", "--allow-empty", "-m", "stale")
	gitSettingsCommand(t, c.Home, clone, "push", "origin", "HEAD:refs/heads/stale")
	gitSettingsCommand(t, c.Home, clone, "checkout", "-q", "-b", "topic")
	if output, err := gitSettingsRun(c.Home, clone, personal, "-c", "core.hooksPath=.git/hooks", "commit", "--allow-empty", "-m", "published topic"); err != nil {
		t.Fatal(output, err)
	}
	gitSettingsCommand(t, c.Home, clone, "-c", "core.hooksPath=.git/hooks", "push", "origin", "topic")
	gitSettingsCommand(t, c.Home, other, "fetch", "origin")
	gitSettingsCommand(t, c.Home, other, "checkout", "-q", "-b", "topic", "origin/topic")
	gitSettingsCommand(t, c.Home, other, "commit", "--allow-empty", "-m", "topic moved")
	gitSettingsCommand(t, c.Home, other, "tag", "-a", "v9", "-m", "release")
	gitSettingsCommand(t, c.Home, other, "push", "origin", "topic", "v9")
	gitSettingsCommand(t, c.Home, clone, "commit", "--allow-empty", "-m", "topic child")
	gitSettingsCommand(t, c.Home, clone, "push", "origin", "HEAD:refs/heads/topic-child")
	gitSettingsCommand(t, c.Home, remote, "config", "uploadpack.allowFilter", "true")
	url := filepath.ToSlash(remote)
	if !strings.HasPrefix(url, "/") {
		url = "/" + url
	}
	partial := filepath.Join(c.Home, "partial")
	gitSettingsCommand(t, c.Home, c.Home, "clone", "--filter=blob:none", "file://"+url, partial)
	for _, branch := range []string{"one", "two", "three"} {
		gitSettingsCommand(t, c.Home, other, "push", "origin", "HEAD:refs/heads/"+branch)
		gitSettingsCommand(t, c.Home, other, "commit", "--allow-empty", "-m", branch)
	}
	gitSettingsCommand(t, c.Home, other, "push", "origin", "HEAD:refs/heads/four")
	objects := func() string {
		return gitSettingsCommand(t, c.Home, partial, "count-objects", "-v")
	}
	gitSettingsCommand(t, c.Home, partial, "commit", "--allow-empty", "-m", "partial")
	before := objects()
	gitSettingsCommand(t, c.Home, partial, "push", "origin", "HEAD:refs/heads/partial")
	if after := objects(); after != before {
		t.Fatal("a push from a partial clone downloaded objects", before, after)
	}
	gitSettingsCommand(t, c.Home, other, "checkout", "-q", "-b", "solo")
	if output, err := gitSettingsRun(c.Home, other, personal, "-c", "core.hooksPath=.git/hooks", "commit", "--allow-empty", "-m", "published solo"); err != nil {
		t.Fatal(output, err)
	}
	gitSettingsCommand(t, c.Home, other, "-c", "core.hooksPath=.git/hooks", "push", "origin", "solo")
	single := filepath.Join(c.Home, "single")
	gitSettingsCommand(t, c.Home, c.Home, "clone", "--filter=blob:none", "--single-branch", "--branch", "solo", "file://"+url, single)
	gitSettingsCommand(t, c.Home, single, "commit", "--allow-empty", "-m", "solo child")
	writeConfigFixture(t, filepath.Join(other, "large.bin"), strings.Repeat("x", 1<<20))
	gitSettingsCommand(t, c.Home, other, "add", "large.bin")
	gitSettingsCommand(t, c.Home, other, "commit", "-m", "large")
	gitSettingsCommand(t, c.Home, other, "push", "origin", "solo")
	large := gitSettingsCommand(t, c.Home, other, "rev-parse", "HEAD:large.bin")
	gitSettingsCommand(t, c.Home, single, "push", "origin", "HEAD:refs/heads/solo-child")
	if strings.Contains(gitSettingsCommand(t, c.Home, single, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)"), large) {
		t.Fatal("the retry of the guard downloaded file contents into a partial clone")
	}
	sha256Remote, sha256Clone := filepath.Join(c.Home, "sha256.git"), filepath.Join(c.Home, "sha256")
	gitSettingsCommand(t, c.Home, c.Home, "init", "--bare", "--object-format=sha256", "--initial-branch=main", sha256Remote)
	gitSettingsCommand(t, c.Home, c.Home, "clone", sha256Remote, sha256Clone)
	gitSettingsCommand(t, c.Home, sha256Clone, "commit", "--allow-empty", "-m", "sha256")
	gitSettingsCommand(t, c.Home, sha256Clone, "push", "origin", "HEAD:refs/heads/main", "HEAD:refs/heads/gone")
	gitSettingsCommand(t, c.Home, sha256Clone, "push", "origin", "--delete", "gone")
	scratch := filepath.Join(c.Home, "scratch")
	gitSettingsCommand(t, c.Home, c.Home, "init", scratch)
	if output, err := gitSettingsRun(c.Home, scratch, personal, "commit", "--allow-empty", "-m", "local"); err != nil {
		t.Fatal("the guard stopped a commit in a clone without a remote", output, err)
	}
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
	if setting("user.useConfigOnly") != "unset" || setting("core.hooksPath") != "unset" {
		t.Fatal("unticking kept the privacy guard")
	}
}

func TestGitDivergentPullRebasesWithoutAMergeCommit(t *testing.T) {
	c, _, _ := gitSettingsFixture(t)
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
	remote, first, second := filepath.Join(c.Home, "remote.git"), filepath.Join(c.Home, "first"), filepath.Join(c.Home, "second")
	git := func(directory string, args ...string) string {
		return gitSettingsCommand(t, c.Home, directory, args...)
	}
	git(c.Home, "init", "--bare", "--initial-branch=main", remote)
	git(c.Home, "clone", remote, first)
	writeConfigFixture(t, filepath.Join(first, "base"), "base\n")
	git(first, "add", "base")
	git(first, "commit", "-m", "base")
	git(first, "push", "origin", "main")
	git(c.Home, "clone", remote, second)
	writeConfigFixture(t, filepath.Join(second, "local"), "local\n")
	git(second, "add", "local")
	git(second, "commit", "-m", "local")
	localBefore := git(second, "rev-parse", "HEAD")
	writeConfigFixture(t, filepath.Join(first, "remote"), "remote\n")
	git(first, "add", "remote")
	git(first, "commit", "-m", "remote")
	remoteHead := git(first, "rev-parse", "HEAD")
	git(first, "push", "origin", "main")
	git(second, "pull")
	if git(second, "rev-parse", "HEAD") == localBefore || git(second, "rev-parse", "HEAD^") != remoteHead || git(second, "rev-list", "--count", "--merges", "HEAD") != "0" {
		t.Fatal("divergent pull did not replay the local commit above the remote tip")
	}
	git(first, "push", "origin", "HEAD:temporary")
	git(second, "fetch")
	git(first, "push", "origin", "--delete", "temporary")
	git(second, "fetch")
	if git(second, "for-each-ref", "--format=%(refname)", "refs/remotes/origin/temporary") != "" {
		t.Fatal("fetch retained the deleted remote branch")
	}
}

func TestGitIncludePreservesBOMAndUnsetRestoresDefault(t *testing.T) {
	c, _, target := gitSettingsFixture(t)
	original := "\xef\xbb\xbf# personal\r\n"
	writeConfigFixture(t, target, original)
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
	gitSettingsCommand(t, c.Home, c.Home, "config", "--global", "pull.rebase", "false")
	if got := gitSettingsCommand(t, c.Home, c.Home, "config", "--bool", "pull.rebase"); got != "false" {
		t.Fatal("personal override did not win", got)
	}
	gitSettingsCommand(t, c.Home, c.Home, "config", "--global", "--unset", "pull.rebase")
	if got := gitSettingsCommand(t, c.Home, c.Home, "config", "--bool", "pull.rebase"); got != "true" {
		t.Fatal("unsetting personal override did not reveal the default", got)
	}
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{}})
	got, err := os.ReadFile(target)
	if err != nil || !bytes.HasPrefix(got, []byte(original)) {
		t.Fatal("Git integration lost the original BOM or comments", err)
	}
}

func TestGitPublicationRespectsNativeApplicationLock(t *testing.T) {
	c, _, target := gitSettingsFixture(t)
	request := Request{Schema: 1, Mode: "apply", Selected: []string{"git"}}
	preview, err := c.Dispatch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	writeConfigFixture(t, target+".lock", "owned by Git")
	request.ExpectedPlan = preview.Plan.ID
	if _, err := c.Dispatch(context.Background(), request); err == nil {
		t.Fatal("installer ignored Git's application lock")
	}
	data, err := os.ReadFile(target + ".lock")
	if err != nil || string(data) != "owned by Git" {
		t.Fatal("installer changed the native Git lock", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("installer published while Git held its lock", err)
	}
}

func TestGitRemovalStaysAtApprovedPathAfterEnvironmentChanges(t *testing.T) {
	c, _, original := gitSettingsFixture(t)
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
	gitSettingsCommand(t, c.Home, c.Home, "config", "--global", "user.email", "fixture@example.invalid")
	driver := c.Driver.(*NativeDriver)
	driver.shellSelection = nil
	moved := filepath.Join(c.Home, "different-xdg", "git", "config")
	driver.Profiles.Targets[gitSettingsResource][0].Path = moved
	check, err := c.Dispatch(context.Background(), Request{Schema: 1, Mode: "check"})
	if err != nil || check.Status != "needs-action" || !strings.Contains(check.Message, "path changed") {
		t.Fatal("changed discovery path was not explained", check, err)
	}
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{}})
	data, err := os.ReadFile(original)
	if err != nil || bytes.Contains(data, []byte("dotfiles:integration.git")) || !bytes.Contains(data, []byte("fixture@example.invalid")) {
		t.Fatal("removal failed to preserve personal data at its original target", string(data), err)
	}
	if _, err := os.Lstat(moved); !os.IsNotExist(err) {
		t.Fatal("removal wrote a new discovery path", err)
	}
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
	data, err = os.ReadFile(moved)
	if err != nil || !bytes.Contains(data, []byte("dotfiles:integration.git")) {
		t.Fatal("explicit removal did not permit a fresh Git target", err)
	}
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{}})
	if _, err := os.Lstat(moved); !os.IsNotExist(err) {
		t.Fatal("new target did not retain its independent absent baseline", err)
	}
}

func TestGitRejectsUTF16WithoutChangingTheFile(t *testing.T) {
	c, _, target := gitSettingsFixture(t)
	original := []byte{0xff, 0xfe, '[', 0, 'x', 0, ']', 0, '\n', 0}
	writeConfigFixture(t, target, string(original))
	result, err := c.Dispatch(context.Background(), Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Operations) != 1 || result.Plan.Operations[0].Action != "pending" || !strings.Contains(result.Plan.Operations[0].Reason, "UTF-8") {
		t.Fatal("UTF-16 Git configuration was not blocked", result)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(original, got) {
		t.Fatal("invalid Git configuration changed", err)
	}
}

func TestGitNewWindowsConfigurationDoesNotSeedABOM(t *testing.T) {
	c, _, target := gitSettingsFixture(t)
	driver := c.Driver.(*NativeDriver)
	driver.shellSelection = nil
	driver.Profiles.Windows = true
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
	data, err := os.ReadFile(target)
	if err != nil || bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		t.Fatal("new Git configuration inherited the PowerShell-only BOM", err)
	}
}

func TestGitRedirectedDirectoryCannotPublishIntoCheckout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory-link layout; Windows XDG overlap is covered separately")
	}
	c, source, target := gitSettingsFixture(t)
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(target)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(source), filepath.Dir(target)); err != nil {
		t.Fatal(err)
	}
	preview, err := c.Dispatch(context.Background(), Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
	if err != nil {
		t.Fatal(err)
	}
	blocked := false
	for _, op := range preview.Plan.Operations {
		if op.Resource == gitSettingsResource && (op.Action == "pending" || op.Action == "blocked") {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("checkout redirect was allowed", preview.Plan)
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("checkout changed", err)
	}
	if got := gitSettingsCommand(t, c.Home, c.Home, "config", "--get", "pull.rebase"); got != "true" {
		t.Fatal(got)
	}
}

func TestGitMissingBaselineDoesNotBlockIndependentSelection(t *testing.T) {
	c, _, _ := gitSettingsFixture(t)
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
	state, err := LoadState(c.StatePath, c.Home)
	if err != nil {
		t.Fatal(err)
	}
	receipt := state.Receipts[gitSettingsResource]
	d := c.Driver.(*NativeDriver).Profiles
	if err := os.Remove(d.baselinePath(receipt.Recovery)); err != nil {
		t.Fatal(err)
	}
	preview, err := c.Dispatch(context.Background(), Request{Schema: 1, Mode: "apply", Selected: []string{}})
	if err != nil {
		t.Fatal("Git evidence prevented an independent preview", err)
	}
	for _, op := range preview.Plan.Operations {
		if op.Resource == gitSettingsResource && (op.Action != "retain" || op.Observed.Pending == "") {
			t.Fatal("missing evidence did not remain local and protected", op)
		}
	}
}

func TestGitHistoricalLocksDoNotBlockAfterParentRedirect(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX redirected parent fixture")
	}
	c, _, target := gitSettingsFixture(t)
	// A personal Git folder pre-exists, so removal keeps it for the redirect.
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{"git"}})
	dispatchApproved(t, c, Request{Schema: 1, Mode: "apply", Selected: []string{}})
	moved := parent + "-moved"
	if err := os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, parent); err != nil {
		t.Fatal(err)
	}
	d := c.Driver.(*NativeDriver).Profiles
	for _, mode := range []string{"apply", "abandon"} {
		if _, err := d.FinishTransaction(context.Background(), Plan{Mode: mode}, nil); err != nil {
			t.Fatal("historical Git journal blocked unrelated finalization", mode, err)
		}
	}
}
