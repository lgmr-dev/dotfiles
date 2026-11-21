package hooks

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/engine"
	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/state"
)

// SentinelRepository and SentinelReleases are the Sentinel source
// (https://github.com/luisgui1757/sentinel) and its latest-release API.
// SentinelResource is the resource of the Sentinel provider.
const (
	SentinelRepository = "https://github.com/luisgui1757/sentinel.git"
	SentinelReleases   = "https://api.github.com/repos/luisgui1757/sentinel/releases/latest"
	SentinelResource   = "integration.sentinel-installer"
)

// SentinelFloor is the oldest Sentinel release that setup installs, with the
// commit of its tag.
var SentinelFloor = SentinelRelease{Tag: "v0.4.0", Commit: "4c24718612672ed31902d1f18c84361c2fcf6661"}

var sentinelFixedRelease = false

var (
	sentinelTag       = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	sentinelOperation = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// SentinelRelease is a release tag and the commit of that tag.
type SentinelRelease struct{ Tag, Commit string }

func sentinelOlder(a, b string) bool {
	x, y := sentinelTag.FindStringSubmatch(a), sentinelTag.FindStringSubmatch(b)
	for i := 1; i < 4 && x != nil && y != nil; i++ {
		p, _ := strconv.Atoi(x[i])
		q, _ := strconv.Atoi(y[i])
		if p != q {
			return p < q
		}
	}
	return false
}

var sentinelHosts = []string{"codex", "claude", "copilot", "kiro", "opencode"}

var sentinelCLIs = map[string]string{"codex": "tool.codex", "claude": "tool.claude", "copilot": "tool.copilot", "kiro": "tool.kiro", "opencode": "tool.opencode"}

// SentinelLocations holds the agent folders that the process environment
// sets. An empty field means the default folder under the home folder.
// KiroHome is the raw KIRO_HOME value. Installed names the agents that
// DiscoverSentinelAgents found.
type SentinelLocations struct {
	CodexHome, ConfigHome, ClaudeConfigDir, KiroHome, CopilotHome string
	Installed                                                     []string
}

func sentinelKiroFolder(home, value string) string {
	fallback := filepath.Join(home, ".kiro")
	if value == "" || !filepath.IsAbs(value) || strings.ContainsAny(value, "\x00\r\n") {
		return fallback
	}
	value = filepath.Clean(value)
	if filepath.Dir(value) == value {
		return fallback
	}
	if relative, err := filepath.Rel(value, home); err == nil && (relative == "." || filepath.IsLocal(relative)) {
		return fallback
	}
	return value
}

// DiscoverSentinelAgents reports the agents whose command exists, and Kiro
// also when one of its macOS applications exists. It runs nothing.
func DiscoverSentinelAgents(target platform.Context, home string) []string {
	found := []string{}
	for _, agent := range []struct {
		name     string
		commands []string
	}{{"codex", []string{"codex"}}, {"claude", []string{"claude"}}, {"copilot", []string{"copilot"}}, {"kiro", []string{"kiro", "kiro-cli"}}, {"opencode", []string{"opencode"}}} {
		installed := false
		for _, command := range agent.commands {
			_, err := exec.LookPath(command)
			installed = installed || err == nil
		}
		if agent.name == "kiro" && target.OS == "darwin" {
			for _, name := range []string{"Kiro.app", "Kiro CLI.app"} {
				for _, app := range []string{filepath.Join("/Applications", name), filepath.Join(home, "Applications", name)} {
					info, err := os.Stat(app)
					installed = installed || err == nil && info.IsDir()
				}
			}
		}
		if installed {
			found = append(found, agent.name)
		}
	}
	return found
}

type sentinelHomes struct {
	CodexHome   string `json:"CODEX_HOME"`
	ClaudeHome  string `json:"CLAUDE_CONFIG_DIR"`
	CopilotHome string `json:"COPILOT_HOME"`
	ConfigHome  string `json:"XDG_CONFIG_HOME"`
	KiroHome    string `json:"KIRO_HOME"`
}

type sentinelRecord struct {
	Schema    int           `json:"schema"`
	Operation string        `json:"operation"`
	Action    string        `json:"action"`
	Commit    string        `json:"commit"`
	Tag       string        `json:"tag,omitempty"`
	Hosts     []string      `json:"hosts"`
	Homes     sentinelHomes `json:"homes"`
	Complete  bool          `json:"complete"`
}

// Sentinel is the engine.Provider of SentinelResource. It runs the installer
// of a Sentinel release, and Sentinel decides which files it changes: its
// managed block in each rules file, the skill folders that hold its
// .sentinel-owned marker, and the Claude Code output style
// output-styles/sentinel.md.
//
// Install and update take the latest Sentinel release: GitHub must report it
// as published and immutable, and git gives the commit of its tag. Setup
// fetches that commit into <StateDir>/sentinel/checkout and runs the
// installer of that checkout. The commit hash is the integrity check: git
// verifies each fetched object against it, and setup compares each checkout
// file with it. Check, repair and removal use the commit in
// <StateDir>/sentinel/record.json.
//
// Home is the home folder. Repository, Releases and Floor are the Sentinel
// source, the latest-release API and the oldest release that setup installs.
// Latest returns the latest release; nil asks Releases. Client sends the
// Releases requests; nil uses a new http.Client. Token is the GITHUB_TOKEN
// value, which setup sends only to https://api.github.com and never writes.
// Git is the git executable. Shell is /bin/bash, or Windows PowerShell.
// Runner runs the fetch and the Sentinel installer. Query runs the read-only
// commands; nil uses SentinelQuery. Wanted returns the closure of the
// selected and kept resources of the run: an agent whose CLI is in it counts
// as a host.
type Sentinel struct {
	Context              platform.Context
	Home                 string
	Locations            SentinelLocations
	StateDir             string
	Repository, Releases string
	Floor                SentinelRelease
	Latest               func(context.Context) (SentinelRelease, error)
	Client               *http.Client
	Token                string
	Git, Shell           string
	Runner               platform.Runner
	Query                func(context.Context, platform.Command) ([]byte, error)
	Wanted               func() []string

	looked        bool
	latest        SentinelRelease
	latestFailure error
}

// NewSentinel returns the provider with the Sentinel source, the floor
// release and the foreground runner. It is pure. The wiring sets Locations
// and Wanted.
func NewSentinel(host platform.Host, home, stateDir, git, shell string) *Sentinel {
	return &Sentinel{Context: host.Context, Home: home, StateDir: stateDir, Repository: SentinelRepository, Releases: SentinelReleases,
		Floor: SentinelFloor, Token: host.GitHubToken, Git: git, Shell: shell, Runner: platform.Exec{}}
}

func (p *Sentinel) directory() string { return filepath.Join(p.StateDir, "sentinel") }
func (p *Sentinel) checkout() string  { return filepath.Join(p.directory(), "checkout") }
func (p *Sentinel) record() string    { return filepath.Join(p.directory(), "record.json") }

func (p *Sentinel) query(ctx context.Context, c platform.Command) ([]byte, error) {
	if p.Query != nil {
		return p.Query(ctx, c)
	}
	return SentinelQuery(ctx, c)
}

func (p *Sentinel) validate(r catalog.Resource) error {
	if r.ID != SentinelResource || !filepath.IsAbs(p.StateDir) || filepath.Clean(p.StateDir) != p.StateDir || !filepath.IsAbs(p.Home) {
		return errors.New("the Sentinel provider needs its resource, an absolute state folder and the home folder")
	}
	if bound, err := platform.BindDestination(p.checkout()); err != nil || bound != p.checkout() {
		return errors.Join(errors.New("a parent folder of the Sentinel checkout was redirected; examine "+p.directory()), err)
	}
	return nil
}

func (p *Sentinel) readRecord() (*sentinelRecord, error) {
	data, err := platform.ReadDocument(p.record())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var record sentinelRecord
	if err == nil {
		err = platform.Decode(data, &record)
	}
	if err == nil && (record.Schema != 1 || !sentinelOperation.MatchString(record.Operation) || !sentinelCommit(record.Commit) || record.Tag != "" && !sentinelTag.MatchString(record.Tag) ||
		!slices.Contains([]string{"install", "update", "repair", "remove"}, record.Action)) {
		err = errors.New("invalid identity")
	}
	for _, host := range record.Hosts {
		if !slices.Contains(sentinelHosts, host) {
			err = errors.Join(err, fmt.Errorf("unknown host %q", host))
		}
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read the Sentinel record %s: %w", p.record(), err)
	}
	return &record, nil
}

func (p *Sentinel) saveRecord(record sentinelRecord) error {
	if err := platform.PrepareDirectory(p.directory()); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return platform.SaveDocument(p.record(), append(data, '\n'))
}

func (p *Sentinel) intent(action string, release SentinelRelease, hosts []string, homes sentinelHomes) (sentinelRecord, error) {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return sentinelRecord{}, err
	}
	return sentinelRecord{Schema: 1, Operation: hex.EncodeToString(seed), Action: action, Commit: release.Commit, Tag: release.Tag, Hosts: hosts, Homes: homes}, nil
}

func (p *Sentinel) homes(record *sentinelRecord) sentinelHomes {
	if record != nil {
		return record.Homes
	}
	return sentinelHomes{p.Locations.CodexHome, p.Locations.ClaudeConfigDir, p.Locations.CopilotHome, p.Locations.ConfigHome, p.Locations.KiroHome}
}

func (p *Sentinel) hosts(homes sentinelHomes) ([]string, error) {
	folder := func(value string, parts ...string) string {
		if value != "" {
			return value
		}
		return filepath.Join(append([]string{p.Home}, parts...)...)
	}
	roots := map[string]string{
		"codex":    folder(homes.CodexHome, ".codex"),
		"claude":   folder(homes.ClaudeHome, ".claude"),
		"copilot":  folder(homes.CopilotHome, ".copilot"),
		"kiro":     sentinelKiroFolder(p.Home, homes.KiroHome),
		"opencode": filepath.Join(folder(homes.ConfigHome, ".config"), "opencode"),
	}
	wanted := []string{}
	if p.Wanted != nil {
		wanted = p.Wanted()
	}
	found := []string{}
	for _, host := range sentinelHosts {
		present := slices.Contains(p.Locations.Installed, host) || slices.Contains(wanted, sentinelCLIs[host])
		_, err := os.Lstat(roots[host])
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("cannot check for %s at %s: %w", host, roots[host], err)
		}
		if present || err == nil {
			found = append(found, host)
		}
	}
	return found, nil
}

func sentinelUnion(lists ...[]string) []string {
	result := []string{}
	for _, host := range sentinelHosts {
		if slices.ContainsFunc(lists, func(list []string) bool { return slices.Contains(list, host) }) {
			result = append(result, host)
		}
	}
	return result
}

func (p *Sentinel) installed(record *sentinelRecord) SentinelRelease {
	if record.Tag == "" && record.Commit == p.Floor.Commit {
		return p.Floor
	}
	return SentinelRelease{record.Tag, record.Commit}
}

func (p *Sentinel) belowFloor(record *sentinelRecord) bool {
	installed := p.installed(record)
	return installed.Tag == "" || sentinelOlder(installed.Tag, p.Floor.Tag)
}

func sentinelName(release SentinelRelease) string {
	if release.Tag != "" {
		return release.Tag
	}
	return "commit " + release.Commit
}

func (p *Sentinel) target(ctx context.Context, record *sentinelRecord) (release SentinelRelease, note, blocked string) {
	latest, err := p.resolve(ctx)
	if record == nil {
		if err != nil {
			return SentinelRelease{}, "", "Setup cannot find the latest Sentinel release: " + err.Error() + ". Check the network, the proxy settings (HTTPS_PROXY) and GITHUB_TOKEN, then apply again."
		}
		return latest, "", ""
	}
	installed := p.installed(record)
	if p.belowFloor(record) {
		if err != nil {
			return p.Floor, "Setup could not check for a newer Sentinel release: " + err.Error() + ". It installs " + p.Floor.Tag + ".", ""
		}
		return latest, "", ""
	}
	switch {
	case err != nil:
	case installed.Tag != "" && latest.Tag == installed.Tag && latest.Commit != installed.Commit:
		err = fmt.Errorf("the tag %s now names the commit %s, and setup installed %s; setup refuses it", latest.Tag, latest.Commit, installed.Commit)
	case installed.Tag != "" && sentinelOlder(latest.Tag, installed.Tag):
		err = fmt.Errorf("the latest release %s is older than the installed release %s; setup refuses a downgrade", latest.Tag, installed.Tag)
	case latest.Commit != installed.Commit:
		return latest, "Sentinel " + latest.Tag + " is available, and " + sentinelName(installed) + " is installed. Update tools installs it.", ""
	default:
		return latest, "", ""
	}
	return installed, "Setup could not check for a newer Sentinel release: " + err.Error() + ". It keeps " + sentinelName(installed) + ".", ""
}

func (p *Sentinel) installer(checkout string, hosts []string, homes sentinelHomes, mode string) (platform.Command, error) {
	environment := []string{"HOME=" + p.Home, "CODEX_HOME=" + homes.CodexHome, "CLAUDE_CONFIG_DIR=" + homes.ClaudeHome,
		"COPILOT_HOME=" + homes.CopilotHome, "XDG_CONFIG_HOME=" + homes.ConfigHome, "KIRO_HOME=" + homes.KiroHome}
	if p.Context.OS == "windows" {
		if p.Shell == "" {
			return platform.Command{}, errors.New("Sentinel needs Windows PowerShell, which setup did not find")
		}
		arguments := []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(checkout, "tools", "install.ps1"),
			"-Global", "-Hosts", strings.Join(hosts, ","), "-Skills", "core,ui,formal,ops"}
		if mode != "" {
			arguments = append(arguments, "-"+strings.ToUpper(mode[:1])+mode[1:])
		}
		return platform.Command{Path: p.Shell, Args: arguments, Env: environment}, nil
	}
	arguments := []string{filepath.Join(checkout, "tools", "install"), "--global", "--hosts", strings.Join(hosts, ","), "--skills", "core,ui,formal,ops"}
	if mode != "" {
		arguments = append(arguments, "--"+mode)
	}
	return platform.Command{Path: p.Shell, Args: arguments, Env: environment}, nil
}

func sentinelKiroHome(home string, homes sentinelHomes, hosts []string) error {
	if !slices.Contains(hosts, "kiro") || homes.KiroHome == "" || sentinelKiroFolder(home, homes.KiroHome) == filepath.Clean(homes.KiroHome) {
		return nil
	}
	return fmt.Errorf("Kiro ignores KIRO_HOME=%s and reads %s. Unset KIRO_HOME or set it to an absolute folder outside your home folder path, then apply again.", homes.KiroHome, sentinelKiroFolder(home, homes.KiroHome))
}

func sentinelHomesText(homes sentinelHomes) string {
	values := []string{}
	for _, item := range []struct{ name, value string }{{"CODEX_HOME", homes.CodexHome}, {"CLAUDE_CONFIG_DIR", homes.ClaudeHome},
		{"COPILOT_HOME", homes.CopilotHome}, {"XDG_CONFIG_HOME", homes.ConfigHome}, {"KIRO_HOME", homes.KiroHome}} {
		if item.value == "" {
			item.value = "(not set)"
		}
		values = append(values, item.name+"="+item.value)
	}
	return strings.Join(values, ", ")
}

func sentinelFindings(output []byte) string {
	findings := []string{}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "ok:") && !strings.HasPrefix(line, "warning: ") && len(findings) < 12 {
			findings = append(findings, line)
		}
	}
	if len(findings) == 0 {
		return "Sentinel check found differences"
	}
	return "Sentinel check: " + strings.Join(findings, "; ")
}

type sentinelObservation struct {
	Present  bool
	Record   *sentinelRecord
	Target   string
	Checkout state.Record
	Recorded bool
	Commit   string
	Issue    string
}

func (o sentinelObservation) token() string {
	data, err := json.Marshal(o)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Inspect reports the installation that the record describes. It only
// reads. Without a record a wanted Sentinel is Missing, or Blocked when no
// release or no coding agent is available. With a record, the check of the
// Sentinel installer runs only from a checkout that verification proves to
// be the recorded commit: exit 0 is healthy, exit 1 means that installed
// files differ, and each other exit is an error. A newer release makes a
// healthy installation Outdated. A failed lookup is a note. For a removal
// the result is Satisfied with a record and Absent without one: a checkout
// that differs from its target record never keeps Sentinel.
func (p *Sentinel) Inspect(ctx context.Context, r catalog.Resource, want engine.Want, rec engine.Records) (engine.Finding, error) {
	if err := ctx.Err(); err != nil {
		return engine.Finding{}, err
	}
	if err := p.validate(r); err != nil {
		return engine.Finding{}, err
	}
	record, err := p.readRecord()
	if err != nil {
		return engine.Finding{Status: engine.Unknown, Reason: err.Error(), Pending: true, Token: sentinelObservation{Present: want.Present, Issue: err.Error()}.token()}, nil
	}
	o := sentinelObservation{Present: want.Present, Record: record}
	o.Checkout, o.Recorded = rec.State().Targets[SentinelCheckoutKey(p.StateDir)]
	f := engine.Finding{Pending: true}
	if !want.Present {
		f.Status = engine.Satisfied
		if record == nil {
			f.Status = engine.Absent
		} else {
			f.Version = sentinelName(p.installed(record))
		}
		f.Token = o.token()
		return f, nil
	}
	release, note, blocked := p.target(ctx, record)
	o.Target = release.Commit
	if note != "" {
		f.Notes = append(f.Notes, note)
	}
	homes := p.homes(record)
	hosts, err := p.hosts(homes)
	if err != nil {
		return engine.Finding{}, err
	}
	if len(hosts) == 0 {
		blocked = strings.TrimSpace(blocked + " Sentinel found no coding agent. Install Codex, Claude Code, Copilot CLI, Kiro or OpenCode, then apply again.")
	}
	if _, err := p.installer(p.checkout(), hosts, homes, ""); err != nil {
		blocked = strings.TrimSpace(blocked + " " + err.Error() + ".")
	}
	if current := p.homes(nil); current != homes {
		f.Notes = append(f.Notes, "A Sentinel folder variable changed after the install. Setup keeps the recorded values "+sentinelHomesText(homes)+". To move Sentinel, untick it and apply, then select it again.")
	}
	if err := sentinelKiroHome(p.Home, homes, hosts); err != nil {
		blocked = strings.TrimSpace(blocked + " " + err.Error())
	}
	if owned, live, err := p.ownedCheckout(rec); err == nil && live != sentinelAbsent && !owned {
		f.Notes = append(f.Notes, "The Sentinel checkout "+p.checkout()+" has content that setup did not record. Setup keeps it when it needs another checkout.")
	}
	work := func(status engine.Status, reason string) (engine.Finding, error) {
		f.Status, f.Reason, f.Token = status, reason, o.token()
		if blocked != "" {
			f.Status, f.Reason = engine.Blocked, blocked
		}
		return f, nil
	}
	if record == nil {
		return work(engine.Missing, "")
	}
	f.Version = sentinelName(p.installed(record))
	if !record.Complete {
		return work(engine.Unhealthy, "an earlier Sentinel "+record.Action+" did not finish; apply runs the Sentinel installer again")
	}
	if p.belowFloor(record) {
		return work(engine.Unhealthy, "Sentinel "+f.Version+" is older than "+p.Floor.Tag+", the oldest release that setup supports; repair installs "+sentinelName(release))
	}
	commit, err := p.verify(ctx, p.checkout())
	o.Commit = commit
	switch {
	case err != nil:
		o.Issue = err.Error()
		return work(engine.Unhealthy, err.Error())
	case commit == "":
		return work(engine.Unhealthy, "the Sentinel checkout is missing; repair fetches the installed commit again")
	case commit != record.Commit:
		return work(engine.Unhealthy, "the Sentinel checkout holds "+commit+", and setup installed "+record.Commit+"; repair fetches the installed commit again")
	case len(hosts) == 0:
		return work(engine.Unhealthy, "no coding agent to check")
	}
	command, err := p.installer(p.checkout(), hosts, homes, "check")
	if err != nil {
		return work(engine.Unhealthy, err.Error())
	}
	output, err := p.query(ctx, command)
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "warning: ") {
			f.Notes = append(f.Notes, "Sentinel "+strings.TrimSpace(line))
		}
	}
	if err != nil {
		if code, ok := platform.ExitCode(err); ok && code == 1 {
			o.Issue = sentinelFindings(output)
			return work(engine.Unhealthy, o.Issue)
		}
		return engine.Finding{}, fmt.Errorf("Sentinel check failed: %w", err)
	}
	f.Status, f.Token = engine.Satisfied, o.token()
	if release.Commit != record.Commit {
		f.Status, f.Reason = engine.Outdated, "Sentinel "+sentinelName(release)+" is available"
		if blocked != "" {
			f.Notes = append(f.Notes, blocked)
		}
	}
	return f, nil
}

func (p *Sentinel) run(ctx context.Context, rec engine.Records, action string, c platform.Command) error {
	if p.Runner == nil {
		return errors.New("the Sentinel provider has no command runner")
	}
	mark := SentinelResource + ":" + action
	if err := rec.Update(func(s *state.State) error { s.Mark(mark); return nil }); err != nil {
		return err
	}
	clear := func() error { return rec.Update(func(s *state.State) error { s.Clear(mark); return nil }) }
	if _, err := p.Runner.Run(ctx, c); err != nil {
		if _, started := platform.ExitCode(err); !started {
			return errors.Join(fmt.Errorf("the Sentinel installer did not start: %w", err), clear())
		}
		return err
	}
	return clear()
}

// Apply installs a release for the current hosts. An install and an update
// fetch the latest release, as Inspect reported it. A repair fetches the
// recorded commit, except that a repair of an install older than the floor
// fetches the latest release. Apply saves its intent in the record first.
// Then it fetches and verifies the commit and runs the Sentinel installer
// under the pending mark "<resource>:<action>". A failed fetch deletes
// nothing. A run after an interruption runs the installer again.
func (p *Sentinel) Apply(ctx context.Context, r catalog.Resource, approved engine.Finding, rec engine.Records) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.validate(r); err != nil {
		return err
	}
	record, err := p.readRecord()
	if err != nil {
		return err
	}
	homes := p.homes(record)
	hosts, err := p.hosts(homes)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return errors.New("Sentinel found no coding agent")
	}
	if err := sentinelKiroHome(p.Home, homes, hosts); err != nil {
		return err
	}
	action, release := "install", SentinelRelease{}
	switch {
	case record == nil:
		blocked := ""
		if release, _, blocked = p.target(ctx, nil); blocked != "" {
			return errors.New(blocked)
		}
	case p.belowFloor(record):
		action = "repair"
		release, _, _ = p.target(ctx, record)
	case approved.Status == engine.Outdated:
		action = "update"
		release, _, _ = p.target(ctx, record)
	default:
		action, release = "repair", p.installed(record)
	}
	if !sentinelCommit(release.Commit) {
		return fmt.Errorf("the approved Sentinel commit %q is not a commit", release.Commit)
	}
	intent, err := p.intent(action, release, hosts, homes)
	if err != nil {
		return err
	}
	if record != nil {
		intent.Hosts = sentinelUnion(record.Hosts, hosts)
	}
	if err := p.saveRecord(intent); err != nil {
		return err
	}
	checkout, err := p.prepare(ctx, release.Commit, rec)
	if err != nil {
		return err
	}
	command, err := p.installer(checkout, hosts, homes, "")
	if err != nil {
		return err
	}
	if err := p.run(ctx, rec, action, command); err != nil {
		return fmt.Errorf("the Sentinel installer failed; examine its output, run setup again and confirm that the command exited: %w", err)
	}
	intent.Complete = true
	return p.saveRecord(intent)
}

// Remove saves its intent, then runs the removal of the Sentinel installer
// from a verified checkout of the recorded commit, or of the floor for an
// install older than the floor, for each recorded or current host. It uses
// the recorded folders. Then it deletes the record, and the checkout while
// the checkout matches its target record. A checkout with other content
// stays, its target record goes away, and SentinelLeftovers lists it.
// Without a record the installer does not run.
func (p *Sentinel) Remove(ctx context.Context, r catalog.Resource, _ engine.Finding, rec engine.Records) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.validate(r); err != nil {
		return err
	}
	record, err := p.readRecord()
	if err != nil {
		return err
	}
	if record != nil {
		homes := p.homes(record)
		current, err := p.hosts(homes)
		if err != nil {
			return err
		}
		release := p.Floor
		if !p.belowFloor(record) {
			release = p.installed(record)
		}
		intent, err := p.intent("remove", release, sentinelUnion(record.Hosts, current), homes)
		if err != nil {
			return err
		}
		if err := p.saveRecord(intent); err != nil {
			return err
		}
		if len(intent.Hosts) != 0 {
			checkout, err := p.prepare(ctx, release.Commit, rec)
			if err != nil {
				return err
			}
			command, err := p.installer(checkout, intent.Hosts, homes, "remove")
			if err != nil {
				return err
			}
			if err := p.run(ctx, rec, "remove", command); err != nil {
				return fmt.Errorf("the Sentinel installer could not remove its files; examine its output, run setup again and confirm that the command exited: %w", err)
			}
		}
	}
	if err := os.Remove(p.record()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := p.releaseCheckout(ctx, rec); err != nil {
		return err
	}
	_, err = platform.RemoveEmptyDirectory(p.directory())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
