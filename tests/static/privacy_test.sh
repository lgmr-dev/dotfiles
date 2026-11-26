#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
while IFS= read -r name; do unset "$name"; done < <(env | sed -n 's/^\(GIT_[A-Z_]*\)=.*/\1/p')
export HOME="$work" GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0 GIT_AUTHOR_NAME=T GIT_COMMITTER_NAME=T
at='@'
h=home
u=Users
l=users
noreply="1+a${at}users.noreply.github.com"
personal="alice${at}mail.example-isp.net"
failures=0

as() {
  GIT_AUTHOR_EMAIL="$1" GIT_COMMITTER_EMAIL="${2:-$1}" "${@:3}"
}

expect() {
  local want="$1" label="$2" rc=0
  shift 2
  "$@" >/dev/null 2>&1 || rc=$?
  if [[ "$want" == pass && "$rc" != 0 ]] || [[ "$want" == stop && "$rc" == 0 ]]; then
    echo "FAIL: privacy hooks must $want $label" >&2
    failures=$((failures + 1))
  fi
}

git init -q --bare -b main "$work/remote.git"
git clone -q "$work/remote.git" "$work/repo" 2>/dev/null
cd "$work/repo"
mkdir -p scripts
cp -R "$REPO_ROOT/scripts/privacy.sh" "$REPO_ROOT/scripts/hooks" scripts/
git add scripts
as "$personal" "" git commit -q -m "old history with a personal address"
git push -q origin main
git config core.hooksPath scripts/hooks

file_case() {
  printf '%s\n' "$3" > f.txt
  git add f.txt
  expect "$1" "$2" as "$noreply" "" git commit -q -m f
  git reset -q --hard origin/main
}
file_case stop "a home path" "/$h/alice/.config"
file_case stop "a Windows home path" "C:\\$u\\alice\\x"
file_case stop "a Git Bash home path" "/c/$u/alice/x"
file_case stop "a lower-case macOS home path" "/$l/alice"
file_case stop "a personal mailbox" "Alice${at}Gmail.com"
file_case stop "a home path after a URL in minified JSON" "{\"u\":\"https://a.example/x\",\"p\":\"/$h/alice/.ssh\"}"
file_case stop "a file URL with a host" "file://localhost/$h/alice/x"
file_case stop "a Windows home path without a drive" "\\$u\\alice\\secret"
file_case stop "a home path after a URL in a table" "|https://a.example/x|/$h/alice/k|"
file_case stop "a home folder in a WSL share" "\\\\wsl\$\\Ubuntu\\$h\\alice"
file_case pass "an upper-case web URL with a home-like path" "HTTPS://github.com/o/r/blob/main/root/README"
file_case pass "the synthetic test homes" "/home/user /Users/bob /home/runner /home/dotfiles-migration"
file_case pass "a web URL with a home-like path" "https://github.com/o/r/blob/main/portable/root/README"
file_case pass "an ASP.NET Home folder" "Views/Home/Index.cshtml"

push_case() {
  local want="$1" label="$2" author="$3" message="$4" ref="${5:-refs/heads/case-$failures-$RANDOM}"
  as "$author" "$noreply" git commit -q --allow-empty -m "$message"
  expect "$want" "$label" git push -q origin "HEAD:$ref"
  git reset -q --hard origin/main
}
push_case pass "a vendor co-author over old personal history" "$noreply" $'x\n\nCo-Authored-By: Bot <noreply@anthropic.com>'
push_case pass "a package tag in a message" "$noreply" "Update package@next and node@v20.1"
push_case stop "a personal author" "$personal" x
push_case stop "a personal Signed-off-by trailer" "$noreply" $'x\n\nSigned-off-by: A <'"$personal"'>'
push_case stop "a home path in a message" "$noreply" "see /$u/alice/notes"
push_case pass "an SSH remote in a message" "$noreply" "Push to git${at}github.com:o/r.git"
push_case pass "an upper-case noreply domain" "1+a${at}USERS.NOREPLY.GITHUB.COM" x
push_case stop "a git author on a private domain" "git${at}corp-mail.net" x
push_case stop "a bare upper-case sign-off" "$noreply" $'x\n\nSIGNED-OFF-BY: '"$personal"
push_case pass "an ssh URL in a message" "$noreply" "Clone ssh://git${at}github.com/o/r.git"
push_case stop "a personal address after an SSH remote and a comma" "$noreply" "See git${at}github.com:o/r.git,$personal"
push_case pass "a generic type in a trailer" "$noreply" $'x\n\nCaused-by: List<String> change'
push_case stop "a personal address in a ref name" "$noreply" x "refs/heads/$personal"

as "$noreply" "" git tag -a v1 -m $'notes\ntype tree'
expect pass "a tag message with a header-like line" git push -q origin v1
as "$noreply" "$personal" git tag -a v2 -m release
expect stop "a personal tagger" git push -q origin v2

if ((failures > 0)); then
  exit 1
fi
echo "OK: privacy hooks stop personal data and pass public data"
