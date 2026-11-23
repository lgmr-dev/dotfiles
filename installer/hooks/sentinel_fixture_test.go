package hooks_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/hooks"
	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/state"
)

type sentinelEnv struct {
	t     *testing.T
	home  string
	state string
	store *state.Store
}

func (e *sentinelEnv) State() state.State                         { return e.store.View() }
func (e *sentinelEnv) Update(edit func(*state.State) error) error { return e.store.Update(edit) }

func newSentinelEnv(t *testing.T) *sentinelEnv {
	t.Helper()
	root, err := platform.ResolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, directory := filepath.Join(root, "home"), filepath.Join(root, "state")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(directory, runtime.GOOS+"/"+runtime.GOARCH+":"+home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return &sentinelEnv{t: t, home: home, state: directory, store: store}
}

func sentinelWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func sentinelRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sentinelExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

const sentinelInstallScript = `#!/bin/bash
echo "$*" >> "$HOME/sentinel-calls"
last="${@: -1}"
if [ "$last" = "--check" ]; then
	if [ -f "$HOME/sentinel-drift" ]; then
		echo "ok:        $HOME/AGENTS.md"
		echo "DRIFT:     $HOME/.agents/skills/council"
		exit 1
	fi
	if [ -f "$HOME/sentinel-error" ]; then exit 2; fi
	exit 0
fi
if [ "$last" != "--remove" ] && [ -f "$HOME/sentinel-fail" ]; then exit 3; fi
exit 0
`

type sentinelRepository struct {
	t       *testing.T
	git     string
	work    string
	bare    string
	commits map[string]string
}

func (r *sentinelRepository) run(directory string, arguments ...string) string {
	r.t.Helper()
	command := exec.Command(r.git, append([]string{"-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.autocrlf=false", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "init.defaultBranch=main", "-C", directory}, arguments...)...)
	command.Env = slices.DeleteFunc(os.Environ(), func(value string) bool { return strings.HasPrefix(value, "GIT_") })
	command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	output, err := command.CombinedOutput()
	if err != nil {
		r.t.Fatal(arguments, string(output), err)
	}
	return strings.TrimSpace(string(output))
}

func (r *sentinelRepository) release(tag, rules string) string {
	r.t.Helper()
	sentinelWrite(r.t, filepath.Join(r.work, "tools", "install"), sentinelInstallScript, 0755)
	sentinelWrite(r.t, filepath.Join(r.work, "tools", "install.ps1"), "# install\n", 0644)
	sentinelWrite(r.t, filepath.Join(r.work, "rules", "AGENTS.md"), rules, 0644)
	r.run(r.work, "add", ".")
	r.run(r.work, "commit", "--quiet", "-m", tag)
	r.run(r.work, "tag", tag)
	r.run(r.work, "push", "--quiet", r.bare, "refs/tags/"+tag)
	r.commits[tag] = r.run(r.work, "rev-parse", "HEAD")
	return r.commits[tag]
}

func newSentinelRepository(t *testing.T) *sentinelRepository {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("the Sentinel installer fixture needs /bin/bash")
	}
	root, err := platform.ResolvePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &sentinelRepository{t: t, git: git, work: filepath.Join(root, "work"), bare: filepath.Join(root, "sentinel.git"), commits: map[string]string{}}
	for _, directory := range []string{r.work, r.bare} {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	r.run(r.work, "init", "--quiet")
	r.run(r.bare, "init", "--quiet", "--bare")
	return r
}

type sentinelAPI struct {
	server  *httptest.Server
	release string
	commits map[string]string
}

func newSentinelAPI(t *testing.T, commits map[string]string) *sentinelAPI {
	t.Helper()
	api := &sentinelAPI{commits: commits}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tag, isTag := strings.CutPrefix(r.URL.Path, "/repos/owner/sentinel/commits/refs/tags/")
		switch {
		case r.URL.Path == "/repos/owner/sentinel/releases/latest":
			fmt.Fprint(w, api.release)
		case isTag && r.Header.Get("Accept") == "application/vnd.github.sha" && api.commits[tag] != "":
			fmt.Fprint(w, api.commits[tag])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (api *sentinelAPI) latest(tag string) {
	api.release = `{"tag_name":"` + tag + `","draft":false,"prerelease":false,"immutable":true}`
}

func (api *sentinelAPI) url() string { return api.server.URL + "/repos/owner/sentinel/releases/latest" }

type sentinelRunner struct {
	git        string
	installers []platform.Command
	fail       func(platform.Command) error
	fake       bool
}

func (r *sentinelRunner) Run(ctx context.Context, c platform.Command) ([]byte, error) {
	if c.Path != r.git {
		r.installers = append(r.installers, c)
		if r.fail != nil {
			if err := r.fail(c); err != nil {
				return nil, err
			}
		}
		if r.fake {
			return nil, nil
		}
	}
	return platform.Exec{}.Run(ctx, c)
}

type sentinelFixture struct {
	*sentinelEnv
	repository *sentinelRepository
	api        *sentinelAPI
	runner     *sentinelRunner
	floor      string
}

var sentinelResource = catalog.Resource{ID: hooks.SentinelResource, Name: "Sentinel installer", Action: "sentinel"}

func newSentinelFixture(t *testing.T) *sentinelFixture {
	t.Helper()
	repository := newSentinelRepository(t)
	floor := repository.release("v0.4.0", "# Working rules\n")
	api := newSentinelAPI(t, repository.commits)
	api.latest("v0.4.0")
	return &sentinelFixture{sentinelEnv: newSentinelEnv(t), repository: repository, api: api, runner: &sentinelRunner{git: repository.git}, floor: floor}
}

func (f *sentinelFixture) provider() *hooks.Sentinel {
	return &hooks.Sentinel{Context: platform.Context{OS: "linux", Arch: "amd64"}, Home: f.home, StateDir: f.state,
		Repository: "file://" + filepath.ToSlash(f.repository.bare), Releases: f.api.url(), Floor: hooks.SentinelRelease{Tag: "v0.4.0", Commit: f.floor},
		Git: f.repository.git, Shell: "/bin/bash", Runner: f.runner}
}

func (f *sentinelFixture) checkout() string { return filepath.Join(f.state, "sentinel", "checkout") }

func (f *sentinelFixture) inspect(p *hooks.Sentinel, present bool) engine.Finding {
	f.t.Helper()
	finding, err := p.Inspect(context.Background(), sentinelResource, engine.Want{Present: present}, f)
	if err != nil {
		f.t.Fatal(err)
	}
	return finding
}

func (f *sentinelFixture) apply(p *hooks.Sentinel) error {
	f.t.Helper()
	return p.Apply(context.Background(), sentinelResource, f.inspect(p, true), f)
}

func (f *sentinelFixture) install() *hooks.Sentinel {
	f.t.Helper()
	sentinelWrite(f.t, filepath.Join(f.home, ".codex", "config.toml"), "", 0600)
	p := f.provider()
	if err := f.apply(p); err != nil {
		f.t.Fatal(err)
	}
	if finding := f.inspect(p, true); finding.Status != engine.Satisfied {
		f.t.Fatal("the installed Sentinel is not satisfied", finding)
	}
	return p
}

func (f *sentinelFixture) calls() []string {
	data, err := os.ReadFile(filepath.Join(f.home, "sentinel-calls"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func (f *sentinelFixture) head() string {
	return f.repository.run(f.checkout(), "rev-parse", "HEAD")
}
