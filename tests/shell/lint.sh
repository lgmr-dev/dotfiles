#!/usr/bin/env bash
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$REPO_ROOT"

if ! command -v shellcheck >/dev/null 2>&1; then
    echo "skipped: shellcheck not installed (brew install shellcheck)"
    exit 0
fi

sh_files=()
while IFS= read -r f; do sh_files+=("$f"); done < <(
    find . -type f -name "*.sh" -not -path "./.git/*" -not -path "./tests/.cache/*"
)

fail=0
for f in "${sh_files[@]}"; do
    shellcheck -x --shell=bash "$f" || fail=1
done
[[ "$fail" -eq 0 ]] || exit 1

command -v zsh >/dev/null || { echo 'FAIL: zsh is required for active shell syntax checks'; exit 1; }
for script in installer/tools/*/shell/*.zsh; do zsh -n "$script"; done
# shellcheck source=tests/shell/active_fixture.sh
source "$REPO_ROOT/tests/shell/active_fixture.sh"
zsh -n "$ACTIVE_ZSH"
zsh -n "$ACTIVE_ZSH_CORE"
zsh -n "$ACTIVE_ZSH_ATUIN"
bash -n "$ACTIVE_ZSH"
bash -n "$ACTIVE_ZSH_CORE"
bash -n "$ACTIVE_ZSH_ATUIN"
shellcheck --shell=sh "$DOTFILES_SHELL_FIXTURES/posix/state/shells/env.sh"
echo "lint OK"
