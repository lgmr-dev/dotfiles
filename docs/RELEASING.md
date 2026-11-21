# Releasing dotfiles

The owner publishes a version with one GitHub release action. That action
makes the immutable `vX.Y.Z` tag and publishes the notes from
[`docs/releases/`](releases/). There is no release manifest, no release
workflow and no release command.

Releases up to v2.3.0 used a release manifest and a release workflow. Their
tags keep those files. Their notes stay in `docs/releases/`.

## Prepare a version

Open one pull request with these changes:

1. Add the reviewed notes as `docs/releases/<version>.md`. The version has the
   form `vMAJOR.MINOR.PATCH`.
2. Add the code changes of the release, if there are some.

The notes have this shape:

```markdown
# v3.0.0 — Reviewed release title

## Highlights

- Reviewed user-facing changes.

## Compatibility and upgrade

State the supported platforms, migration boundary and local-checkout entrypoint.
```

The H1 text is the title of the GitHub release. The release body is the notes
from `## Highlights` to the end. Do not claim that unexecuted native,
owner-host or visual checks passed.

Each required check must pass on the final head of the pull request.
[Branch protection](security/branch-protection.md) lists the checks. Merge is
an owner action, unless the owner gives it to an agent.

## Release a version

Release publication is an owner action.

1. Wait until the `ci` workflow passes on the merged commit on `main`.
2. Make sure that immutable releases are on. See
   [Readback](security/branch-protection.md#readback).
3. Publish the release. This command makes the tag and the release in one
   action:

   ```bash
   version=v3.0.0
   commit=MERGED_COMMIT_SHA
   notes="docs/releases/$version.md"
   gh release create "$version" --repo luisgui1757/dotfiles --target "$commit" \
     --title "$(sed -n '1s/^# //p' "$notes")" \
     --notes "$(sed -n '/^## Highlights/,$p' "$notes")"
   ```

   The GitHub release page gives the same result: type the new tag, select the
   merged commit, paste the title and the body, then publish.
4. Read the release back. GitHub must report it as immutable, not a draft and
   not a prerelease:

   ```bash
   gh api "repos/luisgui1757/dotfiles/releases/tags/$version" \
     --jq '{tag_name, target_commitish, immutable, draft, prerelease}'
   ```

Do not make the tag with `git tag` and `git push` before the release. One
release action keeps the tag and the notes together.

## Order of the v3 releases

1. **v2.3.0** is the tag of commit `fb15ce5`, the last v2 release. Publish it
   before v3.0.0. Run its Update tools on each machine.
2. **v3.0.0** is the cutover. Before the merge, the owner replaces the
   required checks. See
   [Replace the required checks](security/branch-protection.md#replace-the-required-checks).
   After the merged commit passes CI, publish the release. Then migrate the
   machines with [From v2 to v3.0.0](UPGRADING.md#from-v2-to-v300).
3. **v3.1.0** deletes the import and `restore-v2`. Publish it only after each
   machine finished the import, the three Mac applications moved and each
   fallback closed.

## Pins

Archive, component and wheel checksums, the bootstrap toolchain and the
[Mason registry snapshot](../nvim/mason-registry/README.md) are manually
reviewed pins. Update them with upstream provenance and runtime evidence.
The daily pin bot proposes verified updates for its explicit archive recipes.
It groups them into one PR and holds major releases for manual review.
Renovate lists the new releases of all pins on its dashboard as notices.
See [archive pin proposals](security/supply-chain.md#archive-pin-proposals) for
coverage and exceptions. Renovate covers the other declared workflow, Go and
validator dependencies. It does not calculate archive checksums.

URLs alone are not evidence. The `e2e` jobs install each pinned artifact on
its real system and run its version command, its required-file check or its
headless check. Cross-compilation does not replace them.

## Faults

- Never move or delete a release tag.
- When a release has a fault, merge the fix and publish a newer version. The
  old version keeps its tag and its notes. v1.0.0 was replaced this way by
  v1.0.1 (2026-10-28).
- If immutable releases are off, do not publish. Turn them on first.

Unexecuted redirected-Windows, divergent Windows Terminal, physical Linux,
Apple-Silicon owner-host and visual checks remain explicit residual evidence.
The release notes in [releases](releases/) record the history of each version.
[Known issues](KNOWN-ISSUES.md) lists the problems of each release.
