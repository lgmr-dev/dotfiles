#!/usr/bin/env bash
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$REPO_ROOT"

fail=0
check_absent() {
    local desc="$1" pattern="$2"; shift 2
    if grep -rE --binary-files=without-match "$pattern" "$@" >/dev/null 2>&1; then
        echo "FAIL: $desc — pattern '$pattern' still appears:"
        grep -rnE --binary-files=without-match "$pattern" "$@" | head -5
        fail=1
    else
        echo "ok  : $desc"
    fi
}

check_absent "NODE_TLS_REJECT_UNAUTHORIZED gone (security)" \
    "NODE_TLS_REJECT_UNAUTHORIZED" \
    --exclude-dir=.git --exclude-dir=.claude --exclude-dir=home \
    --exclude-dir=tests \
    --exclude="CLAUDE.md" --exclude="README.md" \
    .

check_absent "vim.loop replaced by vim.uv (deprecation)" \
    "vim\\.loop\\." \
    --exclude-dir=.git --exclude-dir=tests \
    nvim/

check_absent "client.supports_method dot-call replaced (0.11)" \
    "client\\.supports_method" \
    --exclude-dir=.git --exclude-dir=tests \
    nvim/

check_absent "vim.lsp.set_log_level replaced by vim.lsp.log.set_level (0.11)" \
    "vim\\.lsp\\.set_log_level" \
    --exclude-dir=.git --exclude-dir=tests \
    nvim/

check_absent "starship literal '(style)' typo gone" \
    "\\]\\(style\\)" \
    --exclude-dir=.git --exclude-dir=tests \
    starship/

check_absent "bare-Esc kill-whole-line bindkey gone (Meta prefix shadow)" \
    "bindkey[[:space:]]+'\\\\e'[[:space:]]+kill-whole-line" \
    --exclude-dir=.git --exclude-dir=tests \
    installer/tools/

check_absent "no Invoke-Expression/iex in PowerShell profiles (cached dot-source only)" \
    "Invoke-Expression|(^|[^[:alnum:]_])iex([^[:alnum:]_]|\$)" \
    installer/tools/*/shell/*.ps1

if ! grep -Fx "set -as terminal-features ',*:extkeys'" tmux/tmux.conf >/dev/null; then
    echo "FAIL: tmux/tmux.conf must keep extended-key terminal-features on macOS/Linux"
    fail=1
fi

if find tmux/themes -type f -name '*.conf' 2>/dev/null | grep -q .; then
    echo "FAIL: local tmux theme snippets must stay deleted; use upstream Rose Pine variants"
    find tmux/themes -type f -name '*.conf'
    fail=1
else
    echo "ok  : no local tmux theme snippets"
fi

if grep -F '%a %d %b %H:%M' tmux/rose-pine.main.conf >/dev/null; then
    echo "FAIL: generated Rose Pine main config must leave date/time to Starship"
    fail=1
elif grep -Eq '#\{(user|host_short)\}' tmux/rose-pine.main.conf; then
    echo "FAIL: generated Rose Pine main config must not duplicate Starship user/host context"
    fail=1
elif ! grep -F '#{b:pane_current_path} ' tmux/rose-pine.main.conf >/dev/null; then
    echo "FAIL: generated Rose Pine main config must keep directory context with a trailing safety cell"
    fail=1
elif grep -F 'rose-pine/tmux' tmux/tmux.conf >/dev/null; then
    echo "FAIL: POSIX tmux must not use the rose-pine/tmux plugin (bar is a repo-owned generated config)"
    fail=1
else
    echo "ok  : tmux own session/windows/directory only; Starship owns user/host/time"
fi

nonlazy=$(grep -lE "lazy[[:space:]]*=[[:space:]]*false" nvim/lua/plugins/*.lua 2>/dev/null | grep -v rose-pine.lua || true)
if [[ -n "$nonlazy" ]]; then
    echo "FAIL: only rose-pine should be lazy=false. Offenders:"
    echo "$nonlazy"
    fail=1
else
    echo "ok  : lazy-load discipline (only rose-pine is lazy=false)"
fi

if ! grep -E "priority[[:space:]]*=[[:space:]]*1000" nvim/lua/plugins/rose-pine.lua >/dev/null; then
    echo "FAIL: rose-pine.lua must keep priority = 1000"
    fail=1
else
    echo "ok  : rose-pine.lua has priority = 1000"
fi

for stale in mason-lspconfig.nvim none-ls.nvim CopilotChat.nvim copilot.vim nui.nvim; do
    if grep -q "\"$stale\"" nvim/lazy-lock.json 2>/dev/null; then
        echo "FAIL: lazy-lock.json still references removed plugin '$stale'"
        fail=1
    fi
done

if [[ ! -f nvim/lua/util/pinned_git_checkout.lua \
    || ! -f tests/nvim/spec/pinned_git_checkout_spec.lua ]] \
    || ! grep -q 'pinned_checkout.locked_identity.*"lazy.nvim"' nvim/init.lua \
    || ! grep -q 'pinned_checkout.ensure' nvim/init.lua \
    || ! grep -q 'branch = lazy_branch' nvim/init.lua \
    || ! grep -q 'required_file = "lua/lazy/init.lua"' nvim/init.lua \
    || grep -q 'git.*clone' nvim/init.lua; then
    echo "FAIL: lazy.nvim bootstrap must use the behavioral fail-closed pinned checkout helper"
    fail=1
else
    echo "ok  : lazy.nvim bootstrap uses behavioral lock/branch/origin/HEAD/cleanliness proof"
fi

if lazy_sync_hits=$(grep -rnF '+Lazy! sync' \
    installer/hooks/nvim_sync.go .github/workflows/ci.yml \
    tests/nvim/spec/startup_spec.lua); then
    echo "FAIL: setup and validation paths must use Lazy! restore, not Lazy! sync"
    echo "$lazy_sync_hits"
    fail=1
else
    echo "ok  : setup and validation paths restore lazy-lock.json instead of syncing upstream"
fi

if ! grep -q 'pinned_checkout.locked_commit.*"plenary.nvim"' tests/nvim/minimal_init.lua \
    || ! grep -q 'pinned_checkout.ensure' tests/nvim/minimal_init.lua \
    || ! grep -q 'required_file = "lua/plenary/init.lua"' tests/nvim/minimal_init.lua \
    || grep -q 'git.*clone' tests/nvim/minimal_init.lua; then
    echo "FAIL: plenary test harness must use the behavioral fail-closed pinned checkout helper"
    fail=1
else
    echo "ok  : plenary test harness uses the shared pinned checkout proof"
fi

if ! grep -Fq -- "-path './tests/.cache'" tests/static/editorconfig_check.sh \
    || ! grep -Fq -- "! -name '.DS_Store'" tests/static/editorconfig_check.sh \
    || ! grep -Fq "editorconfig-checker \"\$file\"" tests/static/editorconfig_check.sh; then
    echo "FAIL: editorconfig_check.sh must feed editorconfig-checker a pruned file list excluding generated tests/.cache and OS metadata content"
    fail=1
elif find . \
    \( -path './.git' -o -path './.claude' -o -path './tests/.cache' -o -path './home' \) -prune -o \
    -type f \
    ! -name '.DS_Store' \
    ! -path './nvim/lazy-lock.json' \
    -print |
    grep -Fqx './tests/.cache/plenary.nvim/README.md'; then
    echo "FAIL: editorconfig_check.sh file-list pruning still includes generated tests/.cache content"
    fail=1
else
    echo "ok  : editorconfig_check.sh excludes generated tests/.cache and OS metadata content"
fi

if grep -Eq '^[[:space:]]*end_of_line[[:space:]]*=[[:space:]]*crlf' .editorconfig; then
    echo "FAIL: .editorconfig must not request CRLF; .gitattributes intentionally keeps repo text LF-only"
    fail=1
elif awk '$0 !~ /^[[:space:]]*#/ && $0 ~ /eol=crlf/ { found = 1 } END { exit(found ? 0 : 1) }' .gitattributes; then
    echo "FAIL: .gitattributes must not add CRLF overrides; repo text stays LF-only across platforms"
    fail=1
elif ! git check-attr eol text -- tests/nvim/fixtures/sample.bat tests/.cache/attr-probe.cmd |
    grep -Fqx 'tests/nvim/fixtures/sample.bat: eol: lf' ||
    ! git check-attr eol text -- tests/nvim/fixtures/sample.bat tests/.cache/attr-probe.cmd |
        grep -Fqx 'tests/nvim/fixtures/sample.bat: text: set' ||
    ! git check-attr eol text -- tests/nvim/fixtures/sample.bat tests/.cache/attr-probe.cmd |
        grep -Fqx 'tests/.cache/attr-probe.cmd: eol: lf' ||
    ! git check-attr eol text -- tests/nvim/fixtures/sample.bat tests/.cache/attr-probe.cmd |
        grep -Fqx 'tests/.cache/attr-probe.cmd: text: set'; then
    echo "FAIL: .gitattributes must explicitly enforce LF text checkout for .bat and .cmd files"
    fail=1
else
    echo "ok  : repo text line endings stay LF-only across EditorConfig and Git attributes"
fi

if ! grep -Fq '.\test.ps1' .github/workflows/ci.yml; then
    echo "FAIL: Windows CI must use the repo-local test.ps1 entry point"
    fail=1
else
    echo "ok  : Windows CI uses test.ps1"
fi

if [[ ! -f AGENTS.md ]] || ! grep -q 'CLAUDE.md' AGENTS.md; then
    echo "FAIL: AGENTS.md must stay a thin pointer to CLAUDE.md"
    fail=1
elif [[ "$(wc -l < AGENTS.md | tr -d ' ')" -gt 25 ]]; then
    echo "FAIL: AGENTS.md must stay thin; keep the canonical guide in CLAUDE.md"
    fail=1
else
    echo "ok  : AGENTS.md points to the canonical CLAUDE.md guide"
fi

if grep -q 'sequential[[:space:]]*=[[:space:]]*true' tests/nvim/run.ps1 .github/workflows/ci.yml 2>/dev/null; then
    echo "FAIL: Windows nvim test path must not use Plenary sequential mode"
    fail=1
else
    echo "ok  : Windows nvim test path avoids Plenary sequential mode"
fi

if grep -q 'PlenaryBustedDirectory' tests/nvim/run.ps1 2>/dev/null \
    || ! grep -q "plenary.busted" tests/nvim/run.ps1 2>/dev/null; then
    echo "FAIL: Windows nvim test path must run specs directly through plenary.busted"
    fail=1
else
    echo "ok  : Windows nvim test path uses direct plenary.busted specs"
fi

markdown_renderers=$(grep -nE "headlines\\.nvim|markview\\.nvim" nvim/lua/plugins/*.lua 2>/dev/null || true)
if [[ -n "$markdown_renderers" ]]; then
    echo "FAIL: markdown rendering must stay owned by render-markdown.nvim:"
    echo "$markdown_renderers"
    fail=1
else
    echo "ok  : markdown rendering excludes headlines.nvim and markview.nvim"
fi

ps1_files=()
while IFS= read -r f; do ps1_files+=("$f"); done < <(
    find . -type f -name '*.ps1' -not -path './.git/*' -not -path './tests/.cache/*'
)
find_non_ascii_ps1() {
    [[ "$#" -gt 0 ]] || return 0
    LC_ALL=C awk '/[\200-\377]/{print FILENAME; nextfile}' "$@" 2>/dev/null
}

ps1_non_ascii=""
if ! ps1_non_ascii="$(find_non_ascii_ps1 "${ps1_files[@]}")"; then
    echo "FAIL: .ps1 ASCII scan errored"
    fail=1
elif [[ -n "$ps1_non_ascii" ]]; then
    echo "FAIL: non-ASCII chars in .ps1 file(s) (PS 5.1 will mis-parse):"
    # shellcheck disable=SC2001  # sed is clearer than ${//} for line-prefixing
    echo "$ps1_non_ascii" | sed 's/^/  /'
    fail=1
else
    echo "ok  : all .ps1 files are pure ASCII (PS 5.1 safe)"
fi

ps1_ascii_regression_dir="$REPO_ROOT/tests/.cache/ps1-ascii-invariant"
rm -rf "$ps1_ascii_regression_dir"
mkdir -p "$ps1_ascii_regression_dir"
trap 'rm -rf "$ps1_ascii_regression_dir"' EXIT
ps1_ascii_regression_file="$ps1_ascii_regression_dir/high-bit.ps1"
printf 'Write-Host ok\n# high byte: \303\251\n' > "$ps1_ascii_regression_file"
ps1_ascii_regression_hit=""
if ! ps1_ascii_regression_hit="$(find_non_ascii_ps1 "$ps1_ascii_regression_file")"; then
    echo "FAIL: .ps1 ASCII regression scan errored"
    fail=1
elif [[ "$ps1_ascii_regression_hit" != "$ps1_ascii_regression_file" ]]; then
    echo "FAIL: .ps1 ASCII check missed a high-bit byte in tests/.cache"
    fail=1
else
    echo "ok  : .ps1 ASCII check catches high-bit bytes"
fi
rm -rf "$ps1_ascii_regression_dir"

ps1_comment_apos=$(grep -nE "^\s*#.*'" "${ps1_files[@]}" 2>/dev/null || true)
if [[ -n "$ps1_comment_apos" ]]; then
    echo "FAIL: apostrophe in a .ps1 comment (PS 5.1 may mis-tokenize):"
    # shellcheck disable=SC2001  # sed is clearer than ${//} for line-prefixing
    echo "$ps1_comment_apos" | sed 's/^/  /'
    fail=1
else
    echo "ok  : no apostrophes in .ps1 comments"
fi

# shellcheck disable=SC2016  # literal $home in the grep regex, not a shell expansion
ps1_home_assign=$(grep -nEi '\$home[[:space:]]*=|\]\$home\b' "${ps1_files[@]}" 2>/dev/null || true)
if [[ -n "$ps1_home_assign" ]]; then
    echo "FAIL: assignment to read-only \$Home crashes PowerShell at runtime:"
    # shellcheck disable=SC2001  # sed is clearer than ${//} for line-prefixing
    echo "$ps1_home_assign" | sed 's/^/  /'
    fail=1
else
    echo "ok  : no assignment to read-only \$Home in .ps1"
fi

tab_pat="$(printf '^\t')"
tab_indented_lua=$(find nvim tests/nvim linux -name '*.lua' -type f \
    -exec grep -lE "$tab_pat" {} + 2>/dev/null || true)
if [[ -n "$tab_indented_lua" ]]; then
    echo "FAIL: .lua files have tab indentation (should be spaces):"
    # shellcheck disable=SC2001
    echo "$tab_indented_lua" | sed 's/^/  /'
    fail=1
else
    echo "ok  : no tab-indented .lua"
fi

for dead in nvim/lua/plugins.lua nvim/lua/plugins/ai.lua nvim/lua/plugins/avante.lua nvim/lua/plugins/none-ls.lua; do
    if [[ -e "$dead" ]]; then
        echo "FAIL: $dead should be deleted"
        fail=1
    fi
done
[[ "$fail" -eq 0 ]] && echo "ok  : dead-code files gone"

[[ "$fail" -eq 0 ]] || exit 1
echo "all invariants OK"
