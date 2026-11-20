#!/usr/bin/env bash
set -euo pipefail

fail() { printf 'e2e: %s\n' "$*" >&2; exit 1; }
step() { printf '\n=== %s ===\n' "$*"; }

setup() {
  local attempt code
  for attempt in 1 2 3 4; do
    code=0
    bash "$checkout/setup.sh" "$@" || code=$?
    [[ $code == 3 ]] || return "$code"
    printf 'e2e: setup stopped for a new plan (exit 3), run %d\n' "$attempt"
  done
  fail "setup $1 still returned exit 3 after three repeats"
}

seed() {
  mkdir -p "$(dirname "$1")"
  printf '%s' "$2" > "$1"
}

main() {
  [[ ${DOTFILES_TEST_NATIVE:-} == 1 ]] ||
    fail 'this test installs and removes real tools; set DOTFILES_TEST_NATIVE=1 only on a disposable runner'

  local selection='' exclude='' case_name='' argument
  for argument in "$@"; do
    case $argument in
      --select=*) selection=${argument#--select=} ;;
      --exclude=*) exclude=${argument#--exclude=} ;;
      --case=*) case_name=${argument#--case=} ;;
      *) fail "unknown argument $argument; use --select=<ids>, --exclude=<ids> and --case=<name>" ;;
    esac
  done

  checkout="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
  local work helper
  work="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/dotfiles-e2e.XXXXXXXX")"
  helper=${DOTFILES_E2E_HELPER:-}
  if [[ -z $helper ]]; then
    helper="$work/helper"
    CGO_ENABLED=0 go build -o "$helper" "$checkout/tests/e2e/helper/main.go"
  fi

  local home="$work/home"
  mkdir -p "$home"
  export HOME="$home" ZDOTDIR="$home"
  export XDG_CONFIG_HOME="$home/.config" XDG_DATA_HOME="$home/.local/share"
  export XDG_STATE_HOME="$home/.local/state" XDG_CACHE_HOME="$home/.cache"
  export CODEX_HOME="$home/.codex" CLAUDE_CONFIG_DIR="$home/.claude"
  export KIRO_HOME="$home/.kiro" COPILOT_HOME="$home/.copilot"
  local state="$XDG_STATE_HOME/dotfiles"

  local system name retained remains shared
  system="$("$helper" system)"
  name="$system${case_name:+.$case_name}"
  retained="$checkout/tests/e2e/expected/$name.retained"
  remains="$checkout/tests/e2e/expected/$name.remains"
  [[ -f $retained && -f $remains ]] || fail "tests/e2e/expected has no $name.retained and $name.remains"
  if [[ -z $selection ]]; then
    selection="$("$helper" ids --checkout "$checkout" "--exclude=$exclude")"
  fi
  shared="$("$helper" shared --checkout "$checkout")"
  printf 'e2e: system %s\ne2e: selection %s\n' "$name" "$selection"

  step 'Personal files and the baseline of the home folder'
  local code_settings="$XDG_CONFIG_HOME/Code/User/settings.json"
  [[ $(uname -s) != Darwin ]] || code_settings="$home/Library/Application Support/Code/User/settings.json"
  seed "$home/.zshrc" $'# personal zsh line\nexport DOTFILES_E2E_PERSONAL=zsh\n'
  seed "$home/.bashrc" $'# personal bash line\nexport DOTFILES_E2E_PERSONAL=bash\n'
  seed "$home/.gitconfig" $'[user]\n\tname = E2E Person\n\temail = e2e@example.invalid\n[alias]\n\te2e = status --short\n'
  seed "$code_settings" $'{\n  // personal comment\n  "dotfiles.e2e.personal": true\n}\n'
  "$helper" snapshot --root "$home" --exclude "$state" --out "$work/baseline.tsv"

  step 'Install the selection'
  setup apply --yes "--select=$selection"

  step 'Smoke each private build'
  "$helper" smoke --checkout "$checkout" --state "$state"

  step 'Check, update, check'
  setup check
  setup update --yes
  setup check

  step 'Remove the selection and the shared resources'
  setup apply --yes --select= "--remove-shared=$shared"
  setup check

  step 'Retained resources'
  setup plan > "$work/plan.txt"
  cat "$work/plan.txt"
  "$helper" retained --expected "$retained" --plan "$work/plan.txt"

  step 'Home folder against its baseline'
  "$helper" compare --root "$home" --exclude "$state" --baseline "$work/baseline.tsv" --remains "$remains"

  printf '\ne2e: %s passed\n' "$name"
}

main "$@"
