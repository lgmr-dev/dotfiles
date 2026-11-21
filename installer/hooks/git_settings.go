package hooks

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/luisgui1757/dotfiles/installer/targets"
)

const gitAttributionOffChoice = "sentinel-no-attribution"

type gitSlot struct{ targets.Block }

func gitValidEncoding(data []byte) error {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return errors.New("Git configuration must be UTF-8; preserve it for explicit conversion")
	}
	return nil
}

func (s gitSlot) Extract(data []byte, d targets.Desired) ([]byte, bool, error) {
	if err := gitValidEncoding(data); err != nil {
		return nil, false, err
	}
	return s.Block.Extract(data, d)
}

func (s gitSlot) Replace(data []byte, d targets.Desired, value []byte, present bool) ([]byte, error) {
	if err := gitValidEncoding(data); err != nil {
		return nil, err
	}
	return s.Block.Replace(data, d, value, present)
}

func gitCanonicalPath(value string) bool {
	return utf8.ValidString(value) && filepath.IsAbs(value) && filepath.Clean(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

func gitIncludeScript(source string, attributionOff bool) string {
	quoted := strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(filepath.ToSlash(source))
	script := "[include]\n\tpath = \"" + quoted + "\"\n"
	if attributionOff {
		script += "[sentinel]\n\tattribution = off\n"
	}
	return script
}

func (p *Profiles) gitConfigPath() string {
	if p.Folders.GitConfig != "" {
		return p.Folders.GitConfig
	}
	root := p.Folders.Config
	if root == "" {
		root = filepath.Join(p.Folders.Home, ".config")
	}
	return filepath.Join(root, "git", "config")
}

func gitCheckoutOverlap(checkout, target string) string {
	if relative, err := filepath.Rel(checkout, target); err == nil && (relative == "." || filepath.IsLocal(relative)) {
		return "Git's writable global configuration resolves inside the dotfiles checkout. Choose a personal Git configuration directory outside the checkout before enabling defaults."
	}
	return ""
}

func gitDefaultsIssue(source string) string {
	info, err := os.Lstat(source)
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("profile is not a regular file; preserve its redirect and use explicit migration")
	}
	if err == nil {
		var data []byte
		if data, err = profileRead(source); err == nil {
			err = gitValidEncoding(data)
		}
	}
	if err != nil {
		return "Git defaults source is unavailable; restore the matching checkout: " + err.Error()
	}
	return ""
}

func (p *Profiles) gitUnits(selected []string) ([]targets.Desired, string, error) {
	source, path := filepath.Join(p.Checkout, "git", "config"), p.gitConfigPath()
	if !gitCanonicalPath(source) || !gitCanonicalPath(path) {
		return nil, "", errors.New("Git settings require explicit canonical paths without control characters")
	}
	script := gitIncludeScript(source, slices.Contains(selected, gitAttributionOffChoice))
	issues := []string{gitDefaultsIssue(source), p.Folders.GitIssue}
	blocked := strings.TrimSpace(strings.Join(slices.DeleteFunc(issues, func(issue string) bool { return issue == "" }), " "))
	return []targets.Desired{{Kind: "block", Path: path, Slot: profileGit, Content: []byte(script)}}, blocked, nil
}
