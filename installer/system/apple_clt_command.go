package system

import (
	"bytes"
	"errors"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

// https://github.com/Homebrew/install/blob/35da6871c4be7d7fdab2fd505fb7fa667926a2a5/install.sh#L827-L846
const appleCLTScript = `set -eu
operation="$1"
mode="$2"
clt="$3"
placeholder="$4"
baseline="$5"
selected_before="$6"
installed_receipt="$7"
owned_placeholder=""
receipt_identity() {
	local packages info
	packages="$(/usr/sbin/pkgutil --pkgs)" || return 1
	case $'\n'"$packages"$'\n' in
		*$'\ncom.apple.pkg.CLTools_Executables\n'*) ;;
		*) printf '%s\n' none; return 0 ;;
	esac
	info="$(/usr/sbin/pkgutil --pkg-info=com.apple.pkg.CLTools_Executables)" || return 1
	printf '%s\n' "$info" | /usr/bin/awk '
		{
			if (split($0, a, ": ") != 2 || seen[a[1]]++ || a[2] == "") exit 1
			if (a[1] != "package-id" && a[1] != "version" && a[1] != "volume" && a[1] != "location" && a[1] != "install-time") exit 1
			value[a[1]]=a[2]; count++
		}
		END {
			if (count != 5 || value["package-id"] != "com.apple.pkg.CLTools_Executables" || value["volume"] != "/" || value["location"] != "/" ||
				value["version"] !~ /^[0-9]+(\.[0-9]+)*$/ || value["install-time"] !~ /^[1-9][0-9]*$/ || length(value["install-time"]) > 18 || length(value["version"]) > 140) exit 1
			print value["version"] "@" value["install-time"]
		}'
}
selection() {
	local value status
	if value="$(/usr/bin/xcode-select --print-path 2>&1)"; then printf '%s\n' "$value"; return 0; else status=$?; fi
	if [[ "$status" == 2 && "$value" == $'` + appleCLTNoSelectionText + `' ]]; then return 0; fi
	printf 'Cannot inspect Apple developer selection (exit %s): %s\n' "$status" "$value" >&2
	return 1
}
cleanup() {
	status=$?
	trap - EXIT
	if [[ -n "$owned_placeholder" ]]; then
		if [[ -L "$placeholder" || ! -f "$placeholder" ]] ||
				[[ "$(/usr/bin/stat -f '%d:%i' "$placeholder")" != "$owned_placeholder" ]]; then
			printf '%s\n' 'CLT discovery sentinel changed; preserve it and inspect manually.' >&2
			exit 1
		fi
		/bin/rm "$placeholder" || exit 1
	fi
	exit "$status"
}
trap cleanup EXIT
selected="$(selection)"
if [[ -n "$selected" && "$selected" != "$clt" ]]; then
	printf '%s\n' 'A different Apple developer directory is selected; preserve it and review a new plan.' >&2
	exit 1
fi
if [[ "$mode" == install ]]; then
	if [[ -e "$clt" || -L "$clt" ]]; then
		printf '%s\n' 'CLT appeared before installation; preserve it and review a new plan.' >&2
		exit 1
	fi
	current_receipt="$(receipt_identity)" || exit 1
	if [[ "$selected" != "$selected_before" || "$current_receipt" != "$baseline" ]]; then
		printf '%s\n' 'Apple CLT receipt or selection changed from approval; preserve it and replan.' >&2
		exit 1
	fi
	if [[ -L "$placeholder" || ( -e "$placeholder" && ! -f "$placeholder" ) ]]; then
		printf '%s\n' 'The CLT discovery sentinel is not a regular file; inspect it manually.' >&2
		exit 1
	fi
	if [[ ! -e "$placeholder" ]]; then
		(set -C; : > "$placeholder")
		owned_placeholder="$(/usr/bin/stat -f '%d:%i' "$placeholder")"
	fi
	listing="$(/usr/sbin/softwareupdate -l)"
	label="$(printf '%s\n' "$listing" |
		/usr/bin/grep -B 1 -E 'Command Line Tools' |
		/usr/bin/awk -F'*' '/^ *\*/ {print $2}' |
		/usr/bin/sed -e 's/^ *Label: //' -e 's/^ *//' |
		/usr/bin/sort -V | /usr/bin/tail -n1)"
	if [[ "$label" != Command\ Line\ Tools* || "$label" == *$'\n'* || "$label" == *$'\r'* ]]; then
		printf '%s\n' 'Apple did not advertise a compatible CLT package. Run xcode-select --install in a graphical session, complete Apple setup, then run setup again.' >&2
		exit 1
	fi
	printf 'DOTFILES_CLT_LABEL:%s\n' "$label"
	current_receipt="$(receipt_identity)" || exit 1
	selected="$(selection)" || exit 1
	if [[ -e "$clt" || -L "$clt" || "$current_receipt" != "$baseline" || "$selected" != "$selected_before" ]]; then
		printf '%s\n' 'CLT appeared during discovery; preserve it and review a new plan.' >&2
		exit 1
	fi
	/usr/sbin/softwareupdate -i "$label"
	installed_receipt="$(receipt_identity)" || exit 1
	if [[ "$installed_receipt" == none || "${installed_receipt#*@}" == "${baseline#*@}" ]]; then
		printf '%s\n' 'Apple CLT installation did not produce a changed native receipt; completion is unproved.' >&2
		exit 1
	fi
	[[ -x "$clt/usr/bin/clang" && -x "$clt/usr/bin/clang++" && -x "$clt/usr/bin/make" ]] || exit 1
	printf 'DOTFILES_CLT_RECEIPT:%s\n' "$installed_receipt"
	printf 'DOTFILES_CLT_INSTALLED:%s\n' "$operation"
elif [[ "$mode" != finish ]]; then
	exit 1
fi
selected="$(selection)"
if [[ -n "$selected" && "$selected" != "$clt" ]]; then
	printf '%s\n' 'Apple developer selection changed during installation; preserve it.' >&2
	exit 1
fi
current_receipt="$(receipt_identity)" || exit 1
[[ "$current_receipt" == "$installed_receipt" && "$current_receipt" != none && "$current_receipt" != "$baseline" ]] || exit 1
[[ -x "$clt/usr/bin/clang" && -x "$clt/usr/bin/clang++" && -x "$clt/usr/bin/make" ]] || exit 1
/usr/bin/xcode-select --switch "$clt"
printf 'DOTFILES_CLT_SELECTED:%s\n' "$operation"
`

func (d *AppleCLT) command(operation, baseline, selection string) (platform.Command, error) {
	if !ValidAppleCLTReceipt(baseline) || selection != "" && selection != d.clt || !macosCleanPath(d.clt) || !macosCleanPath(d.placeholder) {
		return platform.Command{}, errors.New("invalid Apple CLT command")
	}
	return platform.Command{Path: "/usr/bin/sudo", Args: []string{
		"-n", "/usr/bin/env", "-i", "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C",
		"/bin/bash", "--noprofile", "--norc", "-s", "--", operation, "install", d.clt, d.placeholder, baseline, selection, "",
	}, Stdin: bytes.NewReader([]byte(appleCLTScript)), Elevated: true}, nil
}

func appleCLTMarker(output []byte, kind, operation string) bool {
	for _, line := range strings.Split(string(output), "\n") {
		if line == "DOTFILES_CLT_"+kind+":"+operation {
			return true
		}
	}
	return false
}

func appleCLTInstalledReceipt(output []byte) (string, error) {
	found := ""
	for _, line := range strings.Split(string(output), "\n") {
		if value, ok := strings.CutPrefix(line, "DOTFILES_CLT_RECEIPT:"); ok {
			if found != "" || value == "none" || !ValidAppleCLTReceipt(value) {
				return "", errors.New("invalid Apple CLT installed receipt marker")
			}
			found = value
		}
	}
	if found == "" {
		return "", errors.New("missing Apple CLT installed receipt marker")
	}
	return found, nil
}
