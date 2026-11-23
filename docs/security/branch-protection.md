# Branch protection

The owner sets the protection of `main` in the GitHub settings. The
repository has no checked-in ruleset, no check registry and no helper script
for these settings. This document lists the policy and the required checks. A
document does not prove the live settings: read them back.

`main` has three repository rulesets and a classic required-check list.

| Layer | Bypass | Purpose |
| --- | --- | --- |
| `Protect main: integrity` | None | Strict required checks, PRs, squash-only merges, linear history, no deletion or force pushes, and CodeQL error/high-security protection. |
| `Protect main: review` | Owner, on pull requests only | One approval, CODEOWNER review, stale-review dismissal, last-push approval and resolved threads. |
| `Protect main: owner updates` | Owner, on pull requests only | Only the owner can update `main`; branch/PR automation cannot merge. |

Live state: since 2026-11-03 only `Protect main: integrity` is active. The
review and owner-updates layers are disabled on GitHub.

Keep these layers separate: review bypass must never bypass the integrity checks.
Routine automation must use a separate GitHub App or bot identity without
administration permission, not the owner's credentials. The one exception is the
owner's scheduled maintenance agent. It merges bot PRs, the PRs that replace
them and the PRs for the Renovate dashboard items, with the owner's credentials
only while the review and owner-updates
layers are disabled. It never uses an administration action, `--admin` or a
bypass. Branch protection does not prevent the personal-account owner from
deleting the repository. Release remains an owner action. See
[archive pin proposals](supply-chain.md#archive-pin-proposals).

The live integrity and review rulesets have
`require_extra_approval_for_unattributed_changes: true`. GitHub documents the
[additional approval setting for unattributed Copilot pull requests](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets#additional-approval-for-unattributed-copilot-pull-requests).
Keep this value when you write a ruleset. A missing or false live value is a
difference to correct.

## Required checks

The integrity ruleset and the classic list require the same 18 jobs of
[`ci.yml`](../../.github/workflows/ci.yml). Each check comes from GitHub
Actions app `15368`, and strict mode is on in both layers.

| Group | Check names |
| --- | --- |
| `lint` | `lint (ubuntu-26.04)`, `lint (ubuntu-26.04-arm)`, `lint (macos-26)`, `lint (windows-2025)` |
| `e2e` | `e2e (macOS arm64)`, `e2e (Windows amd64)`, `e2e (Ubuntu 24.04 amd64)`, `e2e (Ubuntu 24.04 arm64)`, `e2e (Ubuntu 26.04 arm64)`, `e2e (Debian 13 amd64)`, `e2e (Debian 13 arm64)` |
| `clean` | `clean (fresh-homebrew)`, `clean (missing-apple-clt)`, `clean (vc-runtime)`, `clean (build-tools)` |
| `migrate` | `migrate (ubuntu-26.04)`, `migrate (macos-26)`, `migrate (windows-2025)` |

CodeQL is not in this list. The integrity ruleset blocks CodeQL error-level
alerts and high or critical security alerts through its code scanning rule.

The native jobs stay required. Unit tests, cross-compilation or a job that
passed on an earlier commit cannot replace them. When a job name in `ci.yml`
changes, change this table and both live lists in the same way.

## Replace the required checks

Release v3.0.0 replaces the 25 checks of `test.yml` and
`installer-engine.yml` with the 18 checks above. The old workflows do not
exist in the v3.0.0 pull request, so the old names block its merge. The owner
does these steps before the merge. They change only the two lists of required
checks.

1. Finish the review. Make sure that each of the 18 new checks passed on the
   exact head of the pull request, from GitHub Actions. A new commit makes
   this evidence invalid.
2. Save the two live lists to files outside the checkout, for a rollback:

   ```bash
   gh api repos/lgmr-dev/dotfiles/rulesets \
     --jq '.[] | {id,name,enforcement}'
   gh api repos/lgmr-dev/dotfiles/rulesets/INTEGRITY_ID > integrity-before.json
   gh api repos/lgmr-dev/dotfiles/branches/main/protection/required_status_checks \
     > classic-before.json
   ```

3. Copy `integrity-before.json` to `integrity-after.json`. In the rule with
   the type `required_status_checks`, replace the `required_status_checks`
   list with the 18 names. Each entry has the form
   `{"context": "lint (ubuntu-26.04)", "integration_id": 15368}`. Keep
   `strict_required_status_checks_policy: true` and each other rule. Keep
   only the fields `name`, `target`, `enforcement`, `conditions`, `rules` and
   `bypass_actors` in the file. Then write the ruleset:

   ```bash
   gh api -X PUT repos/lgmr-dev/dotfiles/rulesets/INTEGRITY_ID \
     --input integrity-after.json
   ```

4. Write `classic-after.json` with `"strict": true` and a `checks` list of
   the same 18 names. Each entry has the form
   `{"context": "lint (ubuntu-26.04)", "app_id": 15368}`. Then write the
   classic list:

   ```bash
   gh api -X PATCH \
     repos/lgmr-dev/dotfiles/branches/main/protection/required_status_checks \
     --input classic-after.json
   ```

5. Read both layers back with the commands in [Readback](#readback). Compare
   the names, the app ID and strict mode in each layer with the table. Make
   sure that each other setting is the same as before.
6. Make sure that the pull request shows each of the 18 checks as required
   and passed. Then the owner decides about the merge.
7. After the merge, make sure that the `ci` workflow passes on the merged
   commit on `main`.

Do not add jobs that only keep an old check name. Do not disable the required
checks for the merge. To go back, write `integrity-before.json` and
`classic-before.json` with the same two commands.

`.github/settings.yml` deliberately has no `branches` key. Probot Settings must
not independently change the classic check set. The static guard parses YAML,
including aliases, inline forms and `null`.

## GitHub security

The repository keeps private vulnerability reporting and immutable releases
on. CodeQL default setup scans GitHub Actions, Go and Python with the default
queries. Shell, PowerShell and Lua remain covered by their focused gates. The
integrity rule blocks CodeQL error-level alerts and high/critical security
alerts, with no bypass.

## Readback

Run `make test-static` for the local policy checks. An authorized maintainer
can inspect the live settings without changing them:

```bash
gh api repos/lgmr-dev/dotfiles/rulesets \
  --jq '.[] | {id,name,enforcement}'
# Use the unique integrity ID returned above.
gh api repos/lgmr-dev/dotfiles/rulesets/INTEGRITY_ID
gh api repos/lgmr-dev/dotfiles/branches/main/protection/required_status_checks
gh api repos/lgmr-dev/dotfiles/actions/permissions
gh api repos/lgmr-dev/dotfiles/private-vulnerability-reporting
gh api repos/lgmr-dev/dotfiles/immutable-releases
gh api repos/lgmr-dev/dotfiles/code-scanning/default-setup
gh api 'repos/lgmr-dev/dotfiles/code-scanning/analyses?ref=refs/heads/main&tool_name=CodeQL'
```

Compare exact names and app IDs in both required-check layers, strict mode,
Actions SHA pinning, bypass actors, unchanged review/owner rules and exact-head
CodeQL analysis. A prior readback is historical evidence, not proof of current
live state. Unexpected drift needs an independently reviewed correction; do not
delete duplicates or overwrite unfamiliar policy to make the lists match.

Historical rollout evidence, the 23-check and 25-check cutovers and the former
Nix/chezmoi procedure remain in the
[guide at the last v2 commit](https://github.com/lgmr-dev/dotfiles/blob/fb15ce5/docs/security/branch-protection.md).
