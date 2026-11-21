#!/usr/bin/env bash
set -euo pipefail

fail() { printf 'e2e migrate: %s\n' "$*" >&2; exit 1; }
step() { printf '\n=== %s ===\n' "$*"; }

setup() {
  local attempt code
  for attempt in 1 2 3 4; do
    code=0
    bash "$checkout/setup.sh" "$@" || code=$?
    [[ $code == 3 ]] || return "$code"
    printf 'e2e migrate: setup stopped for a new plan (exit 3), run %d\n' "$attempt"
  done
  fail "setup $1 still returned exit 3 after three repeats"
}

main() {
  [[ ${DOTFILES_TEST_NATIVE:-} == 1 ]] ||
    fail 'this test installs and removes real tools; set DOTFILES_TEST_NATIVE=1 only on a disposable runner'
  [[ -n ${V2_REF:-} ]] || fail 'set V2_REF to the last v2 commit or tag'

  local selection='' argument
  for argument in "$@"; do
    case $argument in
      --select=*) selection=${argument#--select=} ;;
      *) fail "unknown argument $argument; use --select=<ids>" ;;
    esac
  done
  [[ -n $selection ]] || fail 'give the v2 selection with --select=<ids>'

  checkout="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
  [[ -z $(git -C "$checkout" status --porcelain) ]] || fail 'the checkout has changes; this test changes the checked out commit'
  local work helper
  pr_head="$(git -C "$checkout" rev-parse HEAD)"
  work="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/dotfiles-migrate.XXXXXXXX")"
  helper="$work/helper"
  CGO_ENABLED=0 go build -o "$helper" "$checkout/tests/e2e/helper/main.go"

  local home="$work/home"
  mkdir -p "$home"
  export HOME="$home" ZDOTDIR="$home"
  export XDG_CONFIG_HOME="$home/.config" XDG_DATA_HOME="$home/.local/share"
  export XDG_STATE_HOME="$home/.local/state" XDG_CACHE_HOME="$home/.cache"
  export CODEX_HOME="$home/.codex" CLAUDE_CONFIG_DIR="$home/.claude"
  export KIRO_HOME="$home/.kiro" COPILOT_HOME="$home/.copilot"
  local state="$XDG_STATE_HOME/dotfiles"

  trap 'git -C "$checkout" checkout --quiet --detach "$pr_head"' EXIT

  step "Install $selection with v2 ($V2_REF)"
  git -C "$checkout" checkout --quiet --detach "$V2_REF"
  "$helper" v2 --mode apply --select "$selection" --state "$state" -- bash "$checkout/setup.sh"
  "$helper" v2 --mode check --state "$state" -- bash "$checkout/setup.sh"

  step 'Preview the import'
  git -C "$checkout" checkout --quiet --detach "$pr_head"
  setup plan

  step 'Import'
  setup apply --yes

  step 'Check the imported installation'
  setup check

  step 'Restore v2'
  setup restore-v2 --yes

  step "Check with v2 ($V2_REF)"
  git -C "$checkout" checkout --quiet --detach "$V2_REF"
  "$helper" v2 --mode check --state "$state" -- bash "$checkout/setup.sh"

  printf '\ne2e migrate: passed\n'
}

main "$@"; exit
