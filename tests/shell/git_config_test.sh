#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
CONFIG="$REPO_ROOT/git/config"

for key in fetch.prune pull.rebase; do
    if [[ "$(git config --file "$CONFIG" --bool --get "$key")" != "true" ]]; then
        echo "FAIL: managed Git defaults must enable $key" >&2
        exit 1
    fi
done
if git config --file "$CONFIG" --get-regexp '^(user\.|pull\.ff$)' >/dev/null; then
    echo "FAIL: Git defaults must not set personal identity or an ff-only pull policy" >&2
    exit 1
fi

echo "Git rebase and fetch-pruning defaults OK"
