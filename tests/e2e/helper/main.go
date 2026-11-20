package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type resource struct {
	ID                    string              `json:"id"`
	Capability            bool                `json:"capability"`
	Order                 int                 `json:"order"`
	Platforms             []string            `json:"platforms"`
	PlatformArchitectures map[string][]string `json:"platform_architectures"`
	Shared                bool                `json:"shared"`
	Bindings              map[string]struct {
		Provider string `json:"provider"`
		Package  string `json:"package"`
	} `json:"bindings"`
}

type pin struct {
	Version       string            `json:"version"`
	Commands      map[string]string `json:"commands"`
	RequiredFiles []string          `json:"required_files"`
}

type host struct{ os, arch, distro, release string }

var noVersionCommand = map[string]bool{
	"aerospace": true, "code": true, "codex-code-mode-host": true, "git-gui": true, "gitk": true,
	"kiro-cli-chat": true, "kiro-cli-term": true, "open-whispr": true, "sh": true, "ssh": true,
	"ssh-keygen": true, "win32yank": true, "wt": true, "zen": true,
}

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("usage: helper system|ids|shared|snapshot|compare|smoke|retained|v2 [options]"))
	}
	commands := map[string]func([]string) error{
		"system": system, "ids": ids, "shared": shared, "snapshot": snapshot,
		"compare": compare, "smoke": smoke, "retained": retained, "v2": v2,
	}
	run, ok := commands[os.Args[1]]
	if !ok {
		fail(fmt.Errorf("unknown helper command %q", os.Args[1]))
	}
	if err := run(os.Args[2:]); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "e2e:", err)
	os.Exit(1)
}

func list(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func discover() host {
	h := host{os: runtime.GOOS, arch: runtime.GOARCH}
	if h.os != "linux" {
		return h
	}
	data, _ := os.ReadFile("/etc/os-release")
	for _, line := range strings.Split(string(data), "\n") {
		name, value, _ := strings.Cut(line, "=")
		value = strings.Trim(value, `"'`)
		switch name {
		case "ID":
			h.distro = value
		case "VERSION_ID":
			h.release = value
		}
	}
	return h
}

func (h host) name() string {
	switch h.os {
	case "darwin":
		return "macos-" + h.arch
	case "linux":
		return h.distro + "-" + h.release + "-" + h.arch
	}
	return h.os + "-" + h.arch
}

func system([]string) error {
	fmt.Println(discover().name())
	return nil
}

func decodeFile(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func catalog(checkout string) ([]resource, error) {
	files, err := filepath.Glob(filepath.Join(checkout, "installer", "tools", "*", "tool.json"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no tool.json under %s", checkout)
	}
	all := []resource{}
	for _, file := range files {
		var tool struct {
			Resources []resource `json:"resources"`
		}
		if err := decodeFile(file, &tool); err != nil {
			return nil, err
		}
		all = append(all, tool.Resources...)
	}
	return all, nil
}

func (r resource) available(h host) bool {
	if len(r.Platforms) > 0 && !slices.Contains(r.Platforms, h.os) {
		return false
	}
	architectures, limited := r.PlatformArchitectures[h.os]
	return !limited || slices.Contains(architectures, h.arch)
}

func ids(args []string) error {
	flags := flag.NewFlagSet("ids", flag.ContinueOnError)
	checkout := flags.String("checkout", ".", "")
	exclude := flags.String("exclude", "", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	resources, err := catalog(*checkout)
	if err != nil {
		return err
	}
	h, skip := discover(), list(*exclude)
	menu := []resource{}
	for _, r := range resources {
		if r.Capability && r.available(h) && !slices.Contains(skip, r.ID) {
			menu = append(menu, r)
		}
	}
	sort.Slice(menu, func(i, j int) bool { return menu[i].Order < menu[j].Order })
	names := []string{}
	for _, r := range menu {
		names = append(names, r.ID)
	}
	fmt.Println(strings.Join(names, ","))
	return nil
}

func shared(args []string) error {
	flags := flag.NewFlagSet("shared", flag.ContinueOnError)
	checkout := flags.String("checkout", ".", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	resources, err := catalog(*checkout)
	if err != nil {
		return err
	}
	h, names := discover(), []string{}
	for _, r := range resources {
		if r.Shared && !r.Capability && r.available(h) {
			names = append(names, r.ID)
		}
	}
	sort.Strings(names)
	fmt.Println(strings.Join(names, ","))
	return nil
}

type entry struct{ kind, digest string }

func walk(root string, exclude []string) (map[string]entry, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	skip := []string{}
	for _, path := range exclude {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		skip = append(skip, absolute)
	}
	entries := map[string]entry{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if path == root {
			return walkErr
		}
		if slices.Contains(skip, path) {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if walkErr != nil {
			entries[name] = entry{kind: "unreadable", digest: "-"}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		switch mode := d.Type(); {
		case mode.IsDir():
			entries[name] = entry{kind: "dir", digest: "-"}
		case mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				entries[name] = entry{kind: "unreadable", digest: "-"}
				return nil
			}
			entries[name] = entry{kind: "link", digest: filepath.ToSlash(target)}
		case mode.IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				entries[name] = entry{kind: "unreadable", digest: "-"}
				return nil
			}
			entries[name] = entry{kind: "file", digest: digest(data)}
		default:
			entries[name] = entry{kind: "other", digest: "-"}
		}
		return nil
	})
	return entries, err
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func snapshot(args []string) error {
	flags := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	root := flags.String("root", "", "")
	exclude := flags.String("exclude", "", "")
	out := flags.String("out", "", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *root == "" || *out == "" {
		return errors.New("snapshot needs --root and --out")
	}
	entries, err := walk(*root, list(*exclude))
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	var text strings.Builder
	for _, name := range names {
		fmt.Fprintf(&text, "%s\t%s\t%s\n", entries[name].kind, entries[name].digest, name)
	}
	return os.WriteFile(*out, []byte(text.String()), 0o600)
}

func readSnapshot(path string) (map[string]entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	entries := map[string]entry{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s: invalid snapshot line %q", path, line)
		}
		entries[fields[2]] = entry{kind: fields[0], digest: fields[1]}
	}
	return entries, nil
}

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func pattern(glob string) *regexp.Regexp {
	var expression strings.Builder
	for i := 0; i < len(glob); i++ {
		switch {
		case glob[i:] == "/**":
			expression.WriteString("(?:/.*)?")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			expression.WriteString(".*")
			i++
		case glob[i] == '*':
			expression.WriteString("[^/]*")
		default:
			expression.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		}
	}
	return regexp.MustCompile("^" + expression.String() + "$")
}

var blockStart = regexp.MustCompile(`(?:\r?\n|^)# >>> dotfiles:([a-z][a-z0-9.-]*) >>>\r?\n`)

func withoutBlocks(data []byte) []byte {
	for {
		start := blockStart.FindSubmatchIndex(data)
		if start == nil {
			return data
		}
		finish := regexp.MustCompile(`# <<< dotfiles:` + regexp.QuoteMeta(string(data[start[2]:start[3]])) + ` <<<\r?\n`)
		end := finish.FindIndex(data[start[1]:])
		if end == nil {
			return data
		}
		data = append(append([]byte{}, data[:start[0]]...), data[start[1]+end[1]:]...)
	}
}

func compare(args []string) error {
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	root := flags.String("root", "", "")
	exclude := flags.String("exclude", "", "")
	baseline := flags.String("baseline", "", "")
	remains := flags.String("remains", "", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	before, err := readSnapshot(*baseline)
	if err != nil {
		return err
	}
	after, err := walk(*root, list(*exclude))
	if err != nil {
		return err
	}
	globs, err := readLines(*remains)
	if err != nil {
		return err
	}
	allowed := []*regexp.Regexp{}
	for _, glob := range globs {
		allowed = append(allowed, pattern(glob))
	}
	names := map[string]bool{}
	for name := range before {
		names[name] = true
	}
	for name := range after {
		names[name] = true
	}
	differences, used := []string{}, map[int]bool{}
	for name := range names {
		old, had := before[name]
		current, has := after[name]
		if had && has && old == current {
			continue
		}
		if had && has && old.kind == "file" && current.kind == "file" {
			data, err := os.ReadFile(filepath.Join(*root, filepath.FromSlash(name)))
			if err == nil && digest(withoutBlocks(data)) == old.digest {
				fmt.Println("personal content is equal outside the managed blocks:", name)
				continue
			}
		}
		remain := false
		for i, expression := range allowed {
			if expression.MatchString(name) {
				remain, used[i] = true, true
			}
		}
		if remain {
			continue
		}
		switch {
		case !had:
			differences = append(differences, "added    "+current.kind+" "+name)
			if data, err := os.ReadFile(filepath.Join(*root, filepath.FromSlash(name))); err == nil && current.kind == "file" && len(data) <= 4096 && utf8.Valid(data) && name[0] == '.' && !strings.Contains(name, "/") {
				fmt.Printf("content of the added file %s:\n%s\n", name, data)
			}
		case !has:
			differences = append(differences, "removed  "+old.kind+" "+name)
		default:
			differences = append(differences, "changed  "+old.kind+" -> "+current.kind+" "+name)
		}
	}
	for i, glob := range globs {
		if !used[i] {
			fmt.Println("note: no difference matched the allowed pattern", glob)
		}
	}
	if len(differences) > 0 {
		sort.Strings(differences)
		return fmt.Errorf("the home folder differs from its baseline outside %s:\n%s", *remains, strings.Join(differences, "\n"))
	}
	fmt.Println("the home folder equals its baseline, except the allowed remains")
	return nil
}

func pins(checkout string) (map[string]map[string]pin, error) {
	files, err := filepath.Glob(filepath.Join(checkout, "installer", "tools", "*", "pins.json"))
	if err != nil {
		return nil, err
	}
	all := map[string]map[string]pin{}
	for _, file := range files {
		var document struct {
			Resources map[string]map[string]pin `json:"resources"`
		}
		if err := decodeFile(file, &document); err != nil {
			return nil, err
		}
		for family, platforms := range document.Resources {
			all[family] = platforms
		}
	}
	return all, nil
}

func execute(timeout time.Duration, environment []string, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, path, args...)
	command.Env = append(os.Environ(), environment...)
	command.WaitDelay = 5 * time.Second
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func smoke(args []string) error {
	flags := flag.NewFlagSet("smoke", flag.ContinueOnError)
	checkout := flags.String("checkout", ".", "")
	state := flags.String("state", "", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	resources, err := catalog(*checkout)
	if err != nil {
		return err
	}
	families, err := pins(*checkout)
	if err != nil {
		return err
	}
	h := discover()
	builds, err := filepath.Glob(filepath.Join(*state, "tools", "*", "current"))
	if err != nil {
		return err
	}
	if len(builds) == 0 {
		return fmt.Errorf("no private build under %s", filepath.Join(*state, "tools"))
	}
	sort.Strings(builds)
	failures := []string{}
	for _, current := range builds {
		id := filepath.Base(filepath.Dir(current))
		family := id
		for _, r := range resources {
			if r.ID == id && r.Bindings[h.os].Provider == "archive" && r.Bindings[h.os].Package != "" {
				family = r.Bindings[h.os].Package
			}
		}
		platforms, known := families[family]
		if !known {
			platforms = families[id]
		}
		selected, ok := platforms[h.os+"/"+h.arch+"/"+h.distro+"-"+h.release]
		if !ok {
			selected, ok = platforms[h.os+"/"+h.arch]
		}
		if !ok {
			fmt.Printf("%s: no archive pin for this system; its files are not examined\n", id)
			continue
		}
		for _, name := range selected.RequiredFiles {
			if _, err := os.Stat(filepath.Join(current, filepath.FromSlash(name))); err != nil {
				failures = append(failures, fmt.Sprintf("%s: required file %s: %v", id, name, err))
			}
		}
		names := make([]string, 0, len(selected.Commands))
		for name := range selected.Commands {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			path := filepath.Join(current, filepath.FromSlash(selected.Commands[name]))
			if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
				failures = append(failures, fmt.Sprintf("%s: command %s is not a file at %s", id, name, path))
				continue
			}
			if noVersionCommand[name] || h.os == "windows" && name == "gh-dash" {
				fmt.Printf("%s: %s is present (no version command)\n", id, name)
				continue
			}
			environment := []string{"PATH=" + filepath.Dir(path) + string(os.PathListSeparator) + os.Getenv("PATH")}
			if id == "tool.ghostty" && h.os == "linux" {
				environment = append(environment, "LD_BIND_NOW=1")
			}
			output, err := execute(2*time.Minute, environment, path, "--version")
			first, _, _ := strings.Cut(output, "\n")
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: %s --version: %v: %s", id, name, err, output))
				continue
			}
			fmt.Printf("%s: %s --version (pin %s): %s\n", id, name, selected.Version, first)
			if id == "tool.ghostty" && h.os == "linux" {
				if !strings.Contains(output, selected.Version) {
					failures = append(failures, fmt.Sprintf("tool.ghostty: the loaded version is not the pin %s: %s", selected.Version, output))
				}
				config := filepath.Join(*checkout, "ghostty", "config")
				if output, err := execute(2*time.Minute, environment, path, "+validate-config", "--config-file="+config); err != nil || output != "" {
					failures = append(failures, fmt.Sprintf("tool.ghostty: the managed configuration is not valid: %v: %s", err, output))
				}
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("smoke failures:\n%s", strings.Join(failures, "\n"))
	}
	return nil
}

var planOperation = regexp.MustCompile(`^(RETAIN|KEEP): .* \(([a-z][a-z0-9.-]*)\)$`)

func retained(args []string) error {
	flags := flag.NewFlagSet("retained", flag.ContinueOnError)
	expected := flags.String("expected", "", "")
	plan := flags.String("plan", "", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	want, err := readLines(*expected)
	if err != nil {
		return err
	}
	text, err := os.Open(*plan)
	if err != nil {
		return err
	}
	defer text.Close()
	got := []string{}
	scanner := bufio.NewScanner(text)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		if match := planOperation.FindStringSubmatch(strings.TrimRight(scanner.Text(), "\r")); match != nil {
			got = append(got, match[2])
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	sort.Strings(want)
	sort.Strings(got)
	if slices.Equal(want, got) {
		fmt.Println("the retained resources equal", *expected)
		return nil
	}
	lines := []string{}
	for _, id := range got {
		if !slices.Contains(want, id) {
			lines = append(lines, "unexpected "+id)
		}
	}
	for _, id := range want {
		if !slices.Contains(got, id) {
			lines = append(lines, "missing    "+id)
		}
	}
	return fmt.Errorf("the retained resources differ from %s:\n%s\nfull list:\n%s", *expected, strings.Join(lines, "\n"), strings.Join(got, "\n"))
}

type v2Request struct {
	Schema       int      `json:"schema"`
	Mode         string   `json:"mode"`
	Selected     []string `json:"selected,omitempty"`
	Adopt        []string `json:"adopt,omitempty"`
	ExpectedPlan string   `json:"expected_plan,omitempty"`
	Retry        bool     `json:"retry,omitempty"`
}

type v2Result struct {
	Status string `json:"status"`
	Plan   struct {
		ID         string `json:"id"`
		Operations []struct {
			Resource string `json:"resource"`
			Action   string `json:"action"`
			Observed struct {
				Adoptable bool `json:"adoptable"`
			} `json:"observed"`
		} `json:"operations"`
	} `json:"plan"`
	Message string `json:"message"`
}

func v2Machine(launcher []string, request v2Request) (v2Result, error) {
	var result v2Result
	input, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	command := exec.Command(launcher[0], append(launcher[1:], "machine")...)
	command.Stdin = bytes.NewReader(input)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, os.Stderr
	if err := command.Run(); err != nil {
		return result, fmt.Errorf("v2 %s request: %w: %s", request.Mode, err, output.String())
	}
	text := output.Bytes()
	if start := bytes.LastIndex(bytes.TrimRight(text, "\r\n"), []byte("\n")); start >= 0 {
		text = text[start+1:]
	}
	if err := json.Unmarshal(text, &result); err != nil {
		return result, fmt.Errorf("v2 %s result: %w: %s", request.Mode, err, output.String())
	}
	return result, nil
}

func v2(args []string) error {
	flags := flag.NewFlagSet("v2", flag.ContinueOnError)
	mode := flags.String("mode", "", "")
	selection := flags.String("select", "", "")
	state := flags.String("state", "", "")
	if err := flags.Parse(args); err != nil {
		return err
	}
	launcher := flags.Args()
	if len(launcher) == 0 || *state == "" {
		return errors.New("v2 needs --state and the launcher command after the options")
	}
	if *mode == "check" {
		result, err := v2Machine(launcher, v2Request{Schema: 1, Mode: "check"})
		if err != nil {
			return err
		}
		fmt.Println("v2 check:", result.Status, result.Message)
		if result.Status != "ready" {
			return errors.New("the v2 check did not pass")
		}
		return nil
	}
	if *mode != "apply" {
		return errors.New("v2 needs --mode apply or --mode check")
	}
	request := v2Request{Schema: 1, Mode: "apply", Selected: list(*selection)}
	for attempt := 1; attempt <= 5; attempt++ {
		request.ExpectedPlan, request.Retry = "", false
		var saved struct {
			Transaction json.RawMessage `json:"transaction"`
		}
		if err := decodeFile(filepath.Join(*state, "state.json"), &saved); err == nil {
			request.Retry = len(saved.Transaction) > 0 && string(saved.Transaction) != "null"
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		preview, err := v2Machine(launcher, request)
		if err != nil {
			return err
		}
		if preview.Status != "preview" || preview.Plan.ID == "" {
			return fmt.Errorf("the v2 preview is missing: %s %s", preview.Status, preview.Message)
		}
		adopt := []string{}
		for _, op := range preview.Plan.Operations {
			if op.Action == "pending" && op.Observed.Adoptable && !slices.Contains(request.Adopt, op.Resource) {
				adopt = append(adopt, op.Resource)
			}
		}
		if len(adopt) > 0 {
			request.Adopt = append(request.Adopt, adopt...)
			sort.Strings(request.Adopt)
			continue
		}
		request.ExpectedPlan = preview.Plan.ID
		result, err := v2Machine(launcher, request)
		if err != nil {
			return err
		}
		fmt.Printf("v2 apply, attempt %d: %s %s\n", attempt, result.Status, result.Message)
		if result.Status == "ready" {
			return nil
		}
		if result.Status != "needs-action" {
			return errors.New("the v2 apply failed")
		}
	}
	return errors.New("the v2 selection did not finish in five attempts")
}
