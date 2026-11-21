# Supply-chain identities

The current installer has two explicit package boundaries: reviewed private
archives and native package managers. Each tool has one folder in
[`installer/tools/`](../../installer/tools/). Its `tool.json` holds the catalog
entries, and its pin files hold the reviewed identities:

| Pin file in `installer/tools/` | Content |
|---|---|
| `<name>/pins.json` | The archive pins of the tool |
| `vscode/linux-code-pins.json` | The Linux Code DEB |
| `tailscale/linux-package-pins.json` | The Linux Tailscale DEB |
| `compiler/buildtools-pin.json` | The Visual Studio Build Tools bootstrapper |
| `vcredist/vcredist-pin.json` | The Microsoft VC++ runtime |
| `t3-code-app/windows-app-pins.json` | The T3 Code installer for Windows |

Neither a package name nor a version print proves downloaded bytes.

| Surface | Identity and enforcement |
|---|---|
| Checkout bootstrap | Pinned Go archive and SHA-256, verified before extraction; build identity includes the runtime and embedded source inputs. Personal Go settings, workspaces and external cache programs cannot alter the build. |
| Private tools and desktop apps | Exact upstream URL/version/SHA-256, bounded safe archive extraction, declared required files, and a private build with a manifest of its digest and its tree. Update reconciles reviewed pins in the checkout. |
| Rust and Python tools | Rust's six upstream components are independently verified and combined without upstream install scripts. LaTeX conversion and Yamllint use private venvs with pinned offline sources/wheels; complete Python identity is part of the desired state. |
| Claude Code and Codex CLI | Independent checksum-pinned vendor native platform archives, consumed without npm or Node. Claude binary hashes match its signed manifest; Codex retains package metadata and native helpers. Reviewed checkout pins own updates; personal accounts, settings and sessions remain unowned. |
| GitHub Copilot CLI | Checksum-pinned standalone single-executable assets from the official github/copilot-cli stable release, verified against its published SHA256SUMS.txt; no npm, Node, install script, MSI, winget or Homebrew. The managed shell sets `COPILOT_AUTO_UPDATE=false` so the reviewed pin owns updates; a self-update outside that environment is detected by Check and reverted by Update/Repair. Accounts, settings and sessions remain unowned. |
| Kiro CLI | Checksum-pinned fixed-version stable downloads from `prod.download.cli.kiro.dev`: the universal macOS DMG read through the private disk-image reader (bundle unmodified, desktop app never launched) and the Linux glibc ZIP; Windows is not offered (Kiro ships it there only as an MSI installer, which setup neither runs nor unpacks); the vendor `install.sh`/`curl` installer never runs. Pins come from Kiro's release manifest and were re-verified by SHA-256. One owned settings field disables its self-update so the reviewed pin owns updates; accounts, other settings and sessions remain unowned. |
| Neovim | Locked plugin sources, reviewed parser/tool recipes and a private generated runtime. Check loads the existing runtime without triggering package repair. |
| Windows Neovim formatters | Gersemi's official PyInstaller executable and clang-format's native Windows wheel binary have exact SHA-256 pins matching upstream GitHub/PyPI digests. Both are unsigned PE binaries, not Microsoft Authenticode packages. The complete clang-format wheel retains its licenses/metadata and requires the native VC++ runtime. Gersemi bundles Python and runtime libraries; its per-invocation temporary extraction is upstream behavior. Versions match the reviewed POSIX Mason registry, while native formatting proves behavior. |
| Shell/tmux plugins | Pinned source archives, source corrections restricted to reviewed fresh payloads, and retained upstream license/provenance. No runtime remote-script pipeline. |
| Sentinel | The latest published and immutable Sentinel release, never older than the floor in `installer/hooks/sentinel.go`. Git fetches exactly the commit of its tag. `HEAD` must equal that commit, and each file must equal its tree, before the Sentinel installer runs. The commit hash is the integrity check. See [Sentinel release](#sentinel-release). |
| Code on Linux | Exact official DEB version/architecture/URL/SHA-256. APT installs it in the foreground. Setup never removes it, and Update refuses a downgrade. Canonical `/usr/share/code/code` matches the distro sandbox profile. A fixed filtered debconf input preserves personal Microsoft repository/key/answer state and declines the `/usr/bin/copilot` link. Check accepts the setuid `chrome-sandbox` helper and the root-only compile cache of the DEB only with their declared owner and mode. Each pin needs its `modes` entry. Keep the entry of a replaced pin, because machines keep that version until Update runs. |
| Ubuntu/Debian native packages | APT runs in the foreground with `--no-remove`. Setup records the name of each package that it installs. A name is not proof of ownership, and setup never removes a package. Native repositories resolve system packages under their own trust model. |
| macOS native packages | Homebrew formulae with native dependency/linkage checks. Setup records the name of each formula that it installs and never removes one. Existing infrastructure remains unowned; verified fresh bootstrap is retained infrastructure. Apple CLT uses Apple's advertised signed packages. |
| macOS application casks | Zen, T3 Code, VS Code, Ghostty and OpenWhispr come from the `homebrew/cask` tap. See [Application casks on macOS](#application-casks-on-macos). |
| Windows vendor tools | Pinned bootstrapper bytes plus valid Microsoft Authenticode and native registration checks. Setup never removes them. Build Tools resolves Microsoft's signed servicing packages; its bootstrap hash is not a claim that those changing components are individually pinned. |
| Windows Terminal | Pinned official unpackaged stable ZIP; scoped settings at the observed LocalAppData path. Store/Preview/Canary are independent. |
| Ghostty on Linux | One reviewed Debian-family archive per architecture, extracted privately; native APT supplies declared system libraries. No upstream repository-add script or DEB maintainer script executes. Ubuntu 24.04 has its own archive of Ghostty 1.3.1, because Ghostty 1.4 needs GTK 4.18 and libadwaita 1.7. |
| T3 Code app | macOS: the official Homebrew `t3-code` cask. Setup accepts only its reviewed app artifact and a macOS version requirement, and `codesign` must prove the bundle ID `com.t3tools.t3code` and the Developer ID team `ARK85ZXQ4Z`. T3 updates itself. Ubuntu/Debian: the official `.deb` of each architecture with an exact SHA-256, extracted privately. Its maintainer scripts never run, so setup installs no AppArmor profile and no system link. Setup adds a start script to the payload, and the launcher and T3's own link handler use it. The script turns off T3's updater and Chromium's sandbox. Windows: the official per-user NSIS installer with an exact SHA-256, size and product code. Before it runs, and before Check calls an installation healthy, Authenticode must prove the publisher `T3 Tools Inc` on the installer, the app and its uninstaller. It runs in the foreground without elevation and must leave the per-user registration under `%LOCALAPPDATA%\Programs`. T3 updates itself. Setup never removes the app: the review names the Windows uninstall route. An installation by another tool stays unowned. |
| Hosted actions and containers | Full action commit SHAs and immutable container digests; native jobs verify their actual OS/architecture. |

Existing packages, foreign files and personal data never acquire deletion
authority merely by matching a package name. Setup never removes a system
package or a vendor application, and it never runs a global autoremove. For a
recorded item that nothing needs, the review shows the removal command for the
operator. A native command that did not report its end leaves a pending mark.
The operator confirms that the command exited before the next mutation.

## Application casks on macOS

VS Code, Ghostty and OpenWhispr moved from pinned archives to Homebrew casks
in v3.0.0. Zen and T3 Code used casks before. Each of these applications
replaces its own bundle when it updates, so a pin on the bundle competes with
the updater.

| Application | Cask | Bundle identifier | Developer ID team |
|---|---|---|---|
| VS Code | `visual-studio-code` | `com.microsoft.VSCode` | `UBF8T346G9` |
| Ghostty | `ghostty` | `com.mitchellh.ghostty` | `24VZTF6M5V` |
| OpenWhispr | `openwhispr` | `com.gizmolabs.openwhispr` | `T832773L2J` |
| Zen | `zen` | `app.zen-browser.zen` | `9V5K9TP787` |
| T3 Code | `t3-code` | `com.t3tools.t3code` | `ARK85ZXQ4Z` |

The trust model has two parts:

- Homebrew supplies the download URL and its SHA-256. Setup accepts only the
  `homebrew/cask` tap and installs with `--require-sha`. The repository pins
  no version and no bytes of these applications.
- The repository reviews the recipe in `installer/system/cask_specs.go`: the
  application path, the artifact list, the uninstall entry and the signing
  requirement. Setup checks the cask metadata before the install and the
  installed recipe after it. `codesign` must prove the bundle identifier and
  the Developer ID team.

Setup installs with `--appdir=/Applications` and `--no-binaries`. It makes the
`code` and `ghostty` command links itself and makes no `code-tunnel` link. A
real signed bundle in `/Applications` or `~/Applications` satisfies the
selection. Setup never runs `zap` and never removes a cask. Check verifies the
identity and the health of the application and compares no version.

A change of an artifact list or of a signing requirement in a cask needs a
reviewed change of `cask_specs.go`. Until then setup refuses the cask.

Archive/bootstrap and Linux Code DEB pin updates remain reviewed maintenance in
this major release. Renovate extracts these pins only to list their new
releases. It does not change them or their hashes.
Update hashes together with extraction/layout and native-consumption evidence.

## Sentinel release

Install and update ask the GitHub API for the latest release of the
[Sentinel](https://github.com/luisgui1757/sentinel) repository. GitHub must
report it as published and immutable, and its tag must be `vX.Y.Z`. A second
request gives the commit of that tag. When `GITHUB_TOKEN` is set and not empty,
setup sends it as a bearer token only to `https://api.github.com`. A redirect
to another host does not get the token. Setup does not write the token. Setup
refuses a release older than the floor in `installer/hooks/sentinel.go` (v0.4.0), an
older release than the recorded one, and a tag that names another commit than
the recorded one. The approved plan binds the commit. Apply runs these Git
commands in `<state>/sentinel/checkout`:

```sh
git init
git fetch --depth 1 https://github.com/luisgui1757/sentinel.git <commit>
git checkout --detach FETCH_HEAD
```

`<state>` is `${XDG_STATE_HOME:-~/.local/state}/dotfiles` on macOS and Linux,
and `%LOCALAPPDATA%\dotfiles` on Windows. Git runs without system or global
configuration, so a personal setting cannot change the fetch. Git uses the
`HTTPS_PROXY` and `https_proxy` variables.

Then setup checks that `git rev-parse HEAD` equals the commit. It compares each
file with the tree of the commit as raw bytes and refuses an extra file. Git index
flags, local filters and replacement objects cannot hide a change from this
comparison. Check, repair, removal and a rerun use the recorded commit, and run
Sentinel only from a checkout that passes it. An install older than the floor
is the exception: repair installs the latest release or the floor, and removal
uses the floor. The commit hash is the integrity
check.

A failed fetch stops the apply.
The report shows the Git output and tells you to check the network or the proxy.

Next, setup runs the Sentinel installer of that checkout. It uses
`tools/install` with Bash on macOS and Linux, and `tools\install.ps1` with
Windows PowerShell on Windows. Dotfiles does not change the Sentinel files.

Trust moves from a reviewed pin to the Sentinel release process. Immutable
releases keep a published tag on its commit, and a tag ruleset on Sentinel lets
only its owner create, move or delete a `v*` tag. A new release reaches a
machine only through **Update tools**. Check shows it as a note. When the
lookup fails, setup keeps the installed release and shows a note, and a first
install stops. A build with the `sentinelfixedrelease` tag installs the floor
release.

## Archive pin proposals

The daily [pin workflow](../../.github/workflows/agent-cli-pins.yml) opens one
grouped PR. It updates the five agent CLIs and the GitHub tools listed in
[the update policy](../../scripts/archive-pin-updates.json), including T3.
Each GitHub recipe names the repository, release tag and asset for every target.
The script verifies each download against the published SHA-256 and checks the
archive paths that the pin lists before it changes a pin. A missing asset, missing hash,
checksum mismatch, download above 1 GiB or changed layout fails that tool
and the workflow.
Other verified updates can still reach the grouped PR.

The five agent CLIs keep their vendor checks:

- Claude: the signed release manifest and the npm SHA-512
- Codex: the npm SHA-512
- Copilot: the release digest and `SHA256SUMS.txt`
- Kiro: the release manifest and the Linux ZIP layout
- Muse Code: the `muse-stable` channel and the size and SHA-256 in its release
  manifest

The Claude release key fingerprint is `31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE`.

For a GitHub recipe, the bot starts from the latest GitHub release. It uses that
release when the release is in the pinned major version and not older than the
pin. Otherwise it proposes the highest stable release in the pinned major version
from the last 100 releases. It reports a newer major version and keeps the pin in
its major version. It never downgrades a pin and refuses prereleases. A recipe
can list release tags that the bot ignores, such as the stray xremap `v0.16.6`.
A GitHub recipe release and a Copilot release must be at least three days old.
Renovate waits three days for each release that has a publication date.

The PR must pass the native checks that protect `main`. The bot never merges.

A scheduled maintenance agent merges the pin and Renovate PRs for the owner. It
merges a bot PR without a change only when the bot verified each tool and the
required checks pass on a head that is current with `main`. When a bot PR fails,
the agent replaces it with its own PR. That PR verifies each new SHA-256 against
the digest of the publisher, moves coupled pins together and must pass the same
native checks. For a file pinned by commit, the commit is the reference. This
verification is the review of a pin change. The agent never pushes to a bot
branch. It marks a PR that it cannot fix with the `needs-owner` label.

The agent also clears the Renovate dashboard. For each item under Pending
Approval or PR Closed (Blocked) that no bot PR covers, it opens its own PR with
the same checks. A major update also needs a model review, and the agent
changes the code that the major needs. The agent never approves a dashboard
item. When `main` has the update, Renovate removes the item. For an item that
needs an owner decision or still fails after one fix, the agent opens an issue
with the `needs-owner` label.

The policy lists every archive resource as automatic or manual. CI rejects a
resource without an entry, a stale entry, and a GitHub recipe with missing
targets. Manual entries include nested components and wheels. The job summary
lists these exceptions with their reasons.

Renovate tracks the releases of the pins. Its dashboard lists each new release
under Pending Approval as a notice:

- every GitHub release download, Ghostty, Python wheels, Node, Rust, VS Code,
  Rose Pine, PSFzf, GNU Make and the plugin source commits in the archive pins
- Linux Code, Tailscale, the bootstrap Go toolchain, the Homebrew install script
  and the Mason snapshot

Do not approve these entries: Renovate cannot verify their checksums. The bot
updates its recipes. The maintenance agent updates the other pins. The agent
CLIs follow their vendor channels in the bot. Renovate has no source for Build
Tools, the VC runtime and the Neovim plugin lock, so review them manually.

The bot does not overwrite the proposal branch when another person authored or
committed a commit on it. Merge that PR, or save your changes and delete the
branch. The bot pushes only over the branch state that it checked.

The workflow uses the existing GitHub App with Contents and Pull requests write
permissions. Its settings are `AGENT_PINS_APP_CLIENT_ID` and
`AGENT_PINS_APP_PRIVATE_KEY`. The App's pushes start the checks that protect `main`.
When verified updates exist but the App cannot open their PR, the job fails.
The job uploads the proposed patch and summary even when PR creation fails.
The workflow path and automation branch stay the same to preserve the existing
schedule and open proposal.

Release publication follows [RELEASING.md](../RELEASING.md): the owner makes
the immutable tag and publishes the notes in one release action. The `e2e`
jobs of the pull request give the native proof of a pin change. The required
checks and their live settings are in
[branch protection](branch-protection.md).

The [previous supply-chain guide](https://github.com/luisgui1757/dotfiles/blob/ae9a6446eb0a837a144d77d2a6345db967c61e4e/docs/security/supply-chain.md)
preserves Nix/chezmoi-era identities and enforcement for historical releases.
Those installers and their fallback package-manager chains are retired.
