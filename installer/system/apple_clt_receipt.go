package system

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

const appleCLTNoSelectionText = "xcode-select: error: Unable to get active developer directory. Use `sudo xcode-select --switch path/to/Xcode.app` to set one (or see `man xcode-select`)"

var appleCLTReceiptIdentity = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*@[1-9][0-9]*$`)

func appleCLTSelectionMissing(err error, stdout, stderr []byte) bool {
	code, exited := platform.ExitCode(err)
	return exited && code == 2 && len(stdout) == 0 && string(stderr) == appleCLTNoSelectionText+"\n"
}

// ParseAppleCLTReceipt reads the output of
// `pkgutil --pkg-info=com.apple.pkg.CLTools_Executables` and returns the
// receipt identity "<version>@<install-time>". It accepts only a receipt of
// the Command Line Tools package at the root volume.
func ParseAppleCLTReceipt(data []byte) (string, error) {
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok || fields[key] != "" || value == "" || strings.ContainsAny(value, "\x00\r\n") {
			return "", errors.New("invalid Apple CLT package receipt")
		}
		fields[key] = value
	}
	identity := fields["version"] + "@" + fields["install-time"]
	if len(fields) != 5 || fields["package-id"] != "com.apple.pkg.CLTools_Executables" || fields["volume"] != "/" || fields["location"] != "/" || !ValidAppleCLTReceipt(identity) || identity == "none" {
		return "", errors.New("invalid Apple CLT package receipt identity or installation root")
	}
	return identity, nil
}

// ValidAppleCLTReceipt reports that identity is "none" or a receipt identity
// "<version>@<install-time>".
func ValidAppleCLTReceipt(identity string) bool {
	if identity == "none" {
		return true
	}
	if len(identity) > 160 || !appleCLTReceiptIdentity.MatchString(identity) {
		return false
	}
	version, timestamp, _ := strings.Cut(identity, "@")
	if len(timestamp) > 18 || len(version) > 140 {
		return false
	}
	n, err := strconv.ParseUint(timestamp, 10, 63)
	return err == nil && n > 0
}

// AppleCLTReceipt returns the receipt identity of the installed Command Line
// Tools package, or "none". A failed query is an error, not absence.
func AppleCLTReceipt(ctx context.Context, query MacQuery) (string, error) {
	data, err := query(ctx, "/usr/sbin/pkgutil", "--pkgs")
	if err != nil {
		return "", err
	}
	if !slices.Contains(strings.Split(string(data), "\n"), "com.apple.pkg.CLTools_Executables") {
		return "none", nil
	}
	data, err = query(ctx, "/usr/sbin/pkgutil", "--pkg-info=com.apple.pkg.CLTools_Executables")
	if err != nil {
		return "", err
	}
	return ParseAppleCLTReceipt(data)
}

func appleCLTSelection(ctx context.Context, query MacQuery) (string, error) {
	data, err := query(ctx, "/usr/bin/xcode-select", "--print-path")
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.Is(err, errMacNoDeveloperSelection) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return macosDeveloperPath(data)
}

func appleCLTDisclosure(baseline string) string {
	if baseline == "none" {
		return "install missing Apple CLT payload (no prior receipt); select it globally; retain installed tools"
	}
	version, timestamp, _ := strings.Cut(baseline, "@")
	return fmt.Sprintf("reinstall missing Apple CLT payload (historical receipt %s, install-time %s); Apple updates receipts; select CLT globally; retain installed tools", version, timestamp)
}

func appleCLTReceiptChanged(before, after string) bool {
	if !ValidAppleCLTReceipt(before) || !ValidAppleCLTReceipt(after) || after == "none" {
		return false
	}
	if before == "none" {
		return true
	}
	_, oldTime, _ := strings.Cut(before, "@")
	_, newTime, _ := strings.Cut(after, "@")
	return oldTime != newTime
}
