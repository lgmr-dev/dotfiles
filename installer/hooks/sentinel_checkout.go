package hooks

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/state"
	"github.com/lgmr-dev/dotfiles/installer/targets"
)

// SentinelCheckoutSlot is the slot of the target record of the Sentinel
// checkout. The record has the kind tree.
const SentinelCheckoutSlot = "git"

const (
	sentinelAbsent    = targets.Absent
	sentinelPreserved = "checkout.preserved-"
	sentinelStage     = ".fetch-"
)

// SentinelCheckoutKey returns the state key of the target record of the
// Sentinel checkout in a state folder.
func SentinelCheckoutKey(stateDir string) string {
	return targets.Key(filepath.Join(stateDir, "sentinel", "checkout"), SentinelCheckoutSlot)
}

// SentinelKinds returns the kind that the Sentinel checkout adds to a
// targets.Writer.
func SentinelKinds() map[string]targets.Kind {
	return map[string]targets.Kind{"tree#" + SentinelCheckoutSlot: sentinelCheckoutKind{}}
}

func sentinelCommit(value string) bool {
	return len(value) == 40 && strings.Trim(value, "0123456789abcdef") == ""
}

func sentinelGitWork(relative string) bool {
	switch relative {
	case ".git/index", ".git/logs", ".git/FETCH_HEAD", ".git/ORIG_HEAD":
		return true
	}
	return strings.HasPrefix(relative, ".git/") && strings.HasSuffix(relative, ".lock")
}

// SentinelCheckoutFingerprint returns the fingerprint of a Sentinel checkout,
// or targets.Absent when path does not exist. It covers the raw bytes, the
// executable bit and the entry kind of each entry of the worktree and of the
// .git folder, such as config, hooks, info, refs and objects. It excludes
// only the entries of .git that Git rewrites in normal use: index, logs,
// FETCH_HEAD, ORIG_HEAD and each lock file.
func SentinelCheckoutFingerprint(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return sentinelAbsent, nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "git-checkout:not-a-folder", nil
	}
	sum := sha256.New()
	err = filepath.WalkDir(path, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || name == path {
			return err
		}
		relative, err := filepath.Rel(path, name)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if sentinelGitWork(relative) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case entry.IsDir():
			fmt.Fprintf(sum, "d %q\n", relative)
		case entry.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(name)
			if err != nil {
				return err
			}
			fmt.Fprintf(sum, "l %q %q\n", relative, link)
		case entry.Type().IsRegular():
			file, err := os.Open(name)
			if err != nil {
				return err
			}
			content := sha256.New()
			_, err = io.Copy(content, file)
			if err = errors.Join(err, file.Close()); err != nil {
				return err
			}
			executable := false
			if runtime.GOOS != "windows" {
				info, err := entry.Info()
				if err != nil {
					return err
				}
				executable = info.Mode().Perm()&0111 != 0
			}
			fmt.Fprintf(sum, "f %q %t %x\n", relative, executable, content.Sum(nil))
		default:
			fmt.Fprintf(sum, "o %q\n", relative)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return "git-checkout:" + hex.EncodeToString(sum.Sum(nil)), nil
}

func sentinelRemoveCheckout(path, fingerprint string) error {
	current, err := SentinelCheckoutFingerprint(path)
	if err != nil || current != fingerprint {
		return errors.Join(fmt.Errorf("%s changed; setup keeps it: examine it, move it away, then run setup again", path), err)
	}
	return os.RemoveAll(path)
}

type sentinelCheckoutKind struct{}

func (sentinelCheckoutKind) Slotted() bool { return true }

func (sentinelCheckoutKind) Observe(d targets.Desired) (string, error) {
	return SentinelCheckoutFingerprint(d.Path)
}

func (sentinelCheckoutKind) Fingerprint(d targets.Desired) (string, error) {
	if len(d.Content) == 0 {
		return "", errors.New("the Sentinel provider records the checkout after it fetched and verified it")
	}
	return string(d.Content), nil
}

func (sentinelCheckoutKind) Baseline(before string) (string, error) {
	if before == sentinelAbsent {
		return sentinelAbsent, nil
	}
	if fingerprint, ok := strings.CutPrefix(before, "fp:"); ok {
		return fingerprint, nil
	}
	return "", errors.New("the baseline of the Sentinel checkout must be absence or a fingerprint")
}

func (sentinelCheckoutKind) Recover(context.Context, targets.Desired, state.Record) error { return nil }

func (sentinelCheckoutKind) CannotCopy() bool { return true }

func (sentinelCheckoutKind) Save(_ context.Context, d targets.Desired, _ string) (string, error) {
	return "", fmt.Errorf("setup makes no copy of %s; move it away yourself", d.Path)
}

func (sentinelCheckoutKind) Write(context.Context, targets.Desired, string) error {
	return errors.New("only the Sentinel provider fetches the checkout")
}

func (sentinelCheckoutKind) Restore(_ context.Context, d targets.Desired, record state.Record, from string) error {
	if record.Before != "" && record.Before != sentinelAbsent {
		return fmt.Errorf("setup has no copy of the earlier content of %s", d.Path)
	}
	if from == sentinelAbsent {
		return nil
	}
	return sentinelRemoveCheckout(d.Path, from)
}

func (sentinelCheckoutKind) Discard(string, string) error { return nil }

func (p *Sentinel) writer() *targets.Writer {
	return &targets.Writer{StateDir: p.StateDir, Kinds: SentinelKinds()}
}

func (p *Sentinel) ownedCheckout(rec engine.Records) (owned bool, live string, err error) {
	live, err = SentinelCheckoutFingerprint(p.checkout())
	if err != nil {
		return false, "", err
	}
	record, has := rec.State().Targets[SentinelCheckoutKey(p.StateDir)]
	return has && live != sentinelAbsent && (live == record.Applied || live == record.Next), live, nil
}

func (p *Sentinel) releaseCheckout(ctx context.Context, rec engine.Records) error {
	key := SentinelCheckoutKey(p.StateDir)
	record, has := rec.State().Targets[key]
	if !has {
		return nil
	}
	owned, live, err := p.ownedCheckout(rec)
	if err != nil {
		return err
	}
	if live != sentinelAbsent && !owned {
		platform.Detail(ctx, "Kept the Sentinel checkout %s, which has content that setup did not record", p.checkout())
		return rec.Update(func(s *state.State) error { s.Drop(key); return nil })
	}
	if owned && live != record.Applied {
		if err := rec.Update(func(s *state.State) error { return s.Promote(key) }); err != nil {
			return err
		}
	}
	if owned {
		return p.writer().Release(ctx, key, rec, targets.Preserve)
	}
	return rec.Update(func(s *state.State) error { s.Drop(key); return nil })
}

func (p *Sentinel) git(directory string, arguments ...string) platform.Command {
	values := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "credential.helper=", "-c", "init.defaultBranch=main",
		"-c", "advice.detachedHead=false", "-c", "core.autocrlf=false", "-c", "core.eol=lf", "-C", directory}
	return platform.Command{Path: p.Git, Args: append(values, arguments...), Env: []string{
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "LC_ALL=C",
		"GIT_DIR=" + filepath.Join(directory, ".git"), "GIT_WORK_TREE=" + directory, "GIT_INDEX_FILE=" + filepath.Join(directory, ".git", "index")}}
}

func (p *Sentinel) verify(ctx context.Context, directory string) (string, error) {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is not a plain directory", directory)
	}
	head, err := p.query(ctx, p.git(directory, "rev-parse", "--verify", "HEAD^{commit}"))
	commit := strings.TrimSpace(string(head))
	if err != nil || !sentinelCommit(commit) {
		return "", errors.Join(fmt.Errorf("cannot read the commit of %s", directory), err)
	}
	changed := func(err error) (string, error) {
		return commit, errors.Join(fmt.Errorf("%s differs from commit %s; setup fetches the commit again", directory, commit), err)
	}
	if _, err := p.query(ctx, p.git(directory, "fsck", "--full", "--no-dangling", "--no-progress")); err != nil {
		return changed(err)
	}
	listing, err := p.query(ctx, p.git(directory, "ls-tree", "-r", "-z", "--full-tree", commit))
	if err != nil {
		return changed(err)
	}
	type entry struct{ mode, object string }
	expected := map[string]entry{}
	for _, line := range strings.Split(strings.TrimSuffix(string(listing), "\x00"), "\x00") {
		meta, path, ok := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || fields[1] != "blob" || fields[0] != "100644" && fields[0] != "100755" || strings.Contains(path, "\n") {
			return changed(fmt.Errorf("unexpected tree entry %q", line))
		}
		expected[path] = entry{fields[0], fields[2]}
	}
	paths := []string{}
	err = filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil || path == directory {
			return err
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		want, ok := expected[relative]
		if !ok || !entry.Type().IsRegular() {
			return fmt.Errorf("%s is not a file of the commit", relative)
		}
		if runtime.GOOS != "windows" {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if (info.Mode().Perm()&0111 != 0) != (want.mode == "100755") {
				return fmt.Errorf("%s has another executable bit", relative)
			}
		}
		paths = append(paths, relative)
		return nil
	})
	if err != nil {
		return changed(err)
	}
	if len(paths) != len(expected) {
		return changed(errors.New("a file of the commit is missing"))
	}
	command := p.git(directory, "hash-object", "--no-filters", "--stdin-paths")
	command.Stdin = bytes.NewReader([]byte(strings.Join(paths, "\n") + "\n"))
	hashes, err := p.query(ctx, command)
	objects := strings.Fields(string(hashes))
	if err != nil || len(objects) != len(paths) {
		return changed(err)
	}
	for i, path := range paths {
		if objects[i] != expected[path].object {
			return changed(fmt.Errorf("%s has other content", path))
		}
	}
	return commit, nil
}

func (p *Sentinel) prepare(ctx context.Context, want string, rec engine.Records) (_ string, err error) {
	if commit, err := p.verify(ctx, p.checkout()); err == nil && commit == want {
		key := SentinelCheckoutKey(p.StateDir)
		if record, has := rec.State().Targets[key]; has && record.Next != "" && !record.Release {
			if live, err := SentinelCheckoutFingerprint(p.checkout()); err == nil && live == record.Next {
				if err := rec.Update(func(s *state.State) error { return s.Promote(key) }); err != nil {
					return "", err
				}
			}
		}
		return p.checkout(), nil
	}
	if p.Runner == nil {
		return "", errors.New("the Sentinel provider has no command runner")
	}
	if err := platform.PrepareDirectory(p.directory()); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(p.directory(), sentinelStage)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(stage))
		}
	}()
	for _, arguments := range [][]string{
		{"init", "--quiet"},
		{"fetch", "--quiet", "--depth", "1", "--no-tags", p.Repository, want},
		{"checkout", "--quiet", "--detach", "FETCH_HEAD"},
	} {
		if _, err := p.Runner.Run(ctx, p.git(stage, arguments...)); err != nil {
			return "", fmt.Errorf("cannot fetch Sentinel %s from %s. Check the network and the proxy settings (git uses HTTPS_PROXY), then try again: %w", want, p.Repository, err)
		}
	}
	commit, err := p.verify(ctx, stage)
	if err != nil {
		return "", err
	}
	if commit != want {
		return "", fmt.Errorf("the fetched Sentinel commit %s is not the expected commit %s; setup refuses it", commit, want)
	}
	fetched, err := SentinelCheckoutFingerprint(stage)
	if err != nil {
		return "", err
	}
	owned, old, err := p.ownedCheckout(rec)
	if err != nil {
		return "", err
	}
	key := SentinelCheckoutKey(p.StateDir)
	if err := rec.Update(func(s *state.State) error {
		next := state.Record{Resource: SentinelResource, Kind: "tree", Next: fetched, Before: sentinelAbsent}
		if owned {
			next.Applied = old
		}
		s.Targets[key] = next
		return nil
	}); err != nil {
		return "", err
	}
	switch {
	case old == sentinelAbsent:
	case owned:
		if err := sentinelRemoveCheckout(p.checkout(), old); err != nil {
			return "", err
		}
	default:
		aside := filepath.Join(p.directory(), sentinelPreserved+strings.ToLower(rand.Text()))
		if err := platform.MoveNoReplace(p.checkout(), aside); err != nil {
			return "", err
		}
		platform.Detail(ctx, "Kept the earlier Sentinel checkout, which has content that setup did not record, at %s", aside)
	}
	if err := platform.MoveNoReplace(stage, p.checkout()); err != nil {
		return "", err
	}
	if published, err := SentinelCheckoutFingerprint(p.checkout()); err != nil || published != fetched {
		return "", errors.Join(fmt.Errorf("%s changed during its publication; run setup again", p.checkout()), err)
	}
	if err := rec.Update(func(s *state.State) error { return s.Promote(key) }); err != nil {
		return "", err
	}
	return p.checkout(), nil
}

// SentinelLeftovers returns the Sentinel checkouts in a state folder that
// setup keeps for the owner and no longer uses: each checkout that setup
// moved aside because its content was not recorded, an unfinished fetch, and
// the checkout that a removal left. Check lists them. The owner removes them.
func SentinelLeftovers(stateDir string) []string {
	directory := filepath.Join(stateDir, "sentinel")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	_, err = os.Lstat(filepath.Join(directory, "record.json"))
	removed := errors.Is(err, os.ErrNotExist)
	left := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, sentinelPreserved) || strings.HasPrefix(name, sentinelStage) || name == "checkout" && removed {
			left = append(left, filepath.Join(directory, name))
		}
	}
	slices.Sort(left)
	return left
}
