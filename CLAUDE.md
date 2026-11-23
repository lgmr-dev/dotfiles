# Guide for coding agents

This file is the canonical guide for coding agents in this repository.
`AGENTS.md` only points to this file. Do not copy this guide into another file.
`README.md` is the operator guide. Keep its first sections short: platforms,
install, update and removal, then the cheat sheets.

## The repository

This repository holds personal dotfiles and a native Go installer. The
installer shows a menu of tools. It resolves their dependencies, applies the
shared Rose Pine configuration and records what it owns. Update, Check, repair
and removal use these records.

Release v1.0.1 replaced the earlier Nix and chezmoi setup. Only the v1.0.1
checkout has `migrate.sh` and `migrate.ps1`. They move a v0.4.x installation to
the new installer.

Release v3.0.0 replaced the v2 installer. It imports only the state of v2.3.0
(commit `fb15ce5`), one time. An older machine runs Update tools with the
v2.3.0 checkout first. The `v2_restore` state field, the `restore-v2` command
and `installer/v2import` are temporary. Release v3.1.0 deletes them. It reads
only a schema-2 state with a closed fallback, and its error for an older state
names the v3.0.0 tag.

## Supported platforms

| Target | Support |
|---|---|
| macOS | Apple Silicon (arm64) only. The installer stops Intel and Rosetta before provider discovery. |
| Linux | Ubuntu and Debian on amd64 and arm64. Only `ID=ubuntu` or `ID=debian` in os-release passes. `ID_LIKE` does not add a distribution. |
| Windows | Native Windows amd64. The minimum is Windows 10 or Windows Server 2016. |
| WSL | WSL uses the Linux graph. There is no WSL detector and no setup of the Windows host. |

APT installs system packages on Ubuntu and Debian. Homebrew installs them on
macOS. Signed Microsoft installers supply the Windows runtimes and Build Tools.
Portable tools use private archives with SHA-256 pins on all three systems.
Other distributions, Linuxbrew, musl and Intel macOS are out of scope.

## Layout

| Path | Content |
|---|---|
| `installer/` | The Go installer. Its packages are `platform`, `tools`, `catalog`, `state`, `fetch`, `engine`, `targets`, `builds`, `system`, `hooks`, `terminal`, `v2import` and `cmd/dotfiles`. |
| `installer/tools/<name>/` | One folder for each tool. `tool.json` holds its catalog entries and its configuration targets. The pin files and the shell fragments in `shell/` are in the same folder. |
| `featuremap.json`, `featuremap.baseline.json`, `scripts/featuremap/` | The package boundaries, their baseline and their check |
| `setup.sh`, `setup.ps1` | Entrypoints for the menu and the commands. They run only from a local clone. |
| `scripts/installer-bootstrap.*` | The verified private Go bootstrap |
| `nvim/`, `tmux/`, `herdr/`, `ghostty/`, `windows-terminal/`, `aerospace/`, `xremap/`, `xcompose/`, `git/`, `lazygit/`, `lazydocker/`, `gh-dash/`, `starship/`, `lsd/`, `atuin/`, `superfile/` | Tool configuration |
| `tests/` | Tests by area. `tests/static/` holds the repository checks. `tests/e2e/` holds the end-to-end tests for the disposable runners. |
| `.github/` | Workflows and the repository settings |
| `docs/` | Upgrade, release and security guides, and release notes |

## Commands

| Command | Use |
|---|---|
| `make ci` | The full local gate. Run it before a pull request. |
| `.\test.ps1` | The local gate on Windows. CI also uses it. |
| `make test` | Static checks, lint, and the Neovim, shell, Starship, tmux, Ghostty, AeroSpace and XCompose tests |
| `make test-static` | `tests/static/run_all.sh` |
| `make test-installer` | `gofmt`, `go vet` and `go test -race -timeout 30m ./...` in `installer/` |
| `make test-bootstrap` | Python tests for the launchers and the pin scripts |
| `make validate-renovate` | The Renovate configuration and its extraction inventory |
| `make deps-check` | Checks the Go imports against the package boundaries in `featuremap.json`, for Linux, macOS and Windows |
| `make deps-baseline` | Writes `featuremap.baseline.json` |

`make ci` runs `test`, `validate-renovate`, `test-bootstrap`, `test-installer`
and `deps-check`. It needs the Go version in `installer/go.mod`, Ruby and
Node 22 or later.
Linux process tests need `cc` with pthreads. The C++ isolation test needs
`clang++`. A sub-target skips with a message when its tool is missing.

CI has two workflows:

- `ci.yml` has four groups of jobs on disposable GitHub runners:
  - `lint` runs the static checks, the configuration tests, the Go race suite
    and the package boundary check.
  - `e2e` installs, runs, updates and removes each tool of a system. Then it
    compares the home folder with its baseline.
  - `clean` proves a fresh Homebrew, missing Apple developer tools and both
    Microsoft installers.
  - `migrate` installs with v2.3.0, imports, runs `check`, restores v2 and runs
    the v2.3.0 Check.
- `agent-cli-pins.yml` proposes verified archive pin updates each day.

`docs/security/branch-protection.md` lists the checks that must pass. CodeQL
adds its own checks. A native job can continue after a failed step. Read the
result of each step. All of these checks must pass on the final head.

## Safety rules

- Do not run native package, font or GUI tests on an everyday workstation.
  Only opted-in disposable runners run them. One variable,
  `DOTFILES_TEST_NATIVE=1`, enables them.
- Do not delete state, locks or pending marks to get a clean install.
- Ownership comes only from a target record with an `applied` value that the
  live content still matches. Equal bytes, a package name, a `before` value
  without `applied` or an inventory difference never give ownership.
- A `system` record holds the name of a package that setup installed. It does
  not prove who installed the package that is there now.
- Setup deletes or replaces only recorded content that still matches. For
  changed content, setup asks first and saves the content before the
  replacement. Personal data and packages that were there before stay.
- After a failure or an interruption, three guarantees hold. Setup loses no
  personal data. Check reports each difference from the records, each
  unfinished change and each item that it cannot inspect. A later run reaches
  the selected state or names the decision or the failed step that blocks it.
- Setup never removes a system package or a vendor application: no APT
  package, Homebrew formula or cask, or Windows vendor installer. For a
  recorded item that no selected, kept or retained resource needs, the review
  shows a copy-ready command, and the operator runs it.
- Never run a global autoremove. Never use Homebrew `--ignore-dependencies` or
  cask `--force`. Never purge an APT package.
- Never change kernel, sysctl, AppArmor, TCC, Gatekeeper or quarantine
  settings. Never sign vendor bytes again. Never change the account login shell.
- Never write personal agent settings, credentials, models or sessions.
- Setup runs only from a local clone. Do not document a remote script that
  goes through a pipe into a shell.
- Never disable Node TLS certificate checks.
- Each mutation needs a full preview and an explicit confirmation. End of
  input and keys typed early never approve.
- Keep unrelated changes in a dirty worktree.
- Merge and release publication are owner actions, unless the owner gives them
  to you. The owner gives all Renovate work to the scheduled maintenance agent:
  the merge of bot PRs (the pins App and Renovate), of the PRs that replace
  them, and of the PRs that update the items of the Renovate dashboard.

## Installer invariants

- `installer/tools/` is the only dependency list: the `tool.json` of each
  tool folder holds its catalog entries. The planner is pure. Providers do the
  platform IO. The menu has no second list. Categories are display data only.
- A resource reaches a provider only through its platform binding.
- Before each step, setup inspects the resource again. When the result differs
  from the approved one, the review shows again. Never apply a stale plan.
- The write order of a target: save the first baseline and each approved
  backup, publish `next` in the state, check the destination again, write,
  verify, then promote `next` to `applied`. A removal sets `release`, restores
  the baseline or the absence, removes the empty folders in `made` and drops
  the record.
- Keep the first baseline through each update. Removal restores it.
- A provider writes a pending mark in the state before it starts a native
  command that can continue after setup stops. It clears the mark after the
  verified result. A mutating run that finds a mark waits until the operator
  confirms that the command exited. Without the menu, `apply` and `update` stop
  at a mark. Setup removes a `system` record only when it verifies the absence.
- The engine saves no plan to run later. After an interruption, the operator
  runs setup again, and setup inspects the system again.
- An unknown observation never proves absence. A failed step blocks its
  dependents, and independent steps continue.
- State is strict schema-2 JSON. An unknown field or schema fails and does not
  change the file. Setup stops when `target` differs from the active system,
  architecture or home folder.
- Constructors are pure. Find the platform, the known folders and the
  environment, such as `ZDOTDIR` and `CODEX_HOME`, at the process boundary.
- Get Windows known folders from the system. Never guess them from `HOME`.
- Retained resources keep their dependencies. Do not disable the retained
  closure to stop a configuration leak.
- System package commands and vendor installers run in the foreground. Setup
  asks for the password one time, in the foreground. The installer lock ends
  with the setup process and does not pass through `sudo` or elevation.
- Verify each archive checksum before extraction. A private build has the
  path `<state>/tools/<id>/<version>-<digest>-<serial>`. The recipe digest
  covers the effective pin, the recipe revision and the exact builds that the
  build uses. Setup checks a build from its final path and writes its
  manifest. Only then does `<state>/tools/<id>/current` move to it. Entry
  points use `current`. Never weaken a payload fingerprint to accept a vendor
  update.
- Archive downloads resume with `Range` and `If-Range` after a broken or slow
  connection, and accept only the exact remaining `Content-Range`. One 15-minute
  limit covers the whole transfer. The SHA-256 check is the only integrity
  check.
- Configuration targets are live links on POSIX. On Windows they are copies,
  with a junction for Neovim. A `block` target owns one marked block, and a
  `field` target owns one named JSON field. Outside edits, BOMs and line
  endings stay.
- The bootstrap verifies the pinned Go archive before each extraction. An
  unchanged checkout reuses its verified build. A fingerprint of the pin, the
  launcher and all files under `installer/` identifies that build. The launcher
  checks the hash of the cached binary before each launch.
- Update applies the reviewed pins of the checkout. It never gets the latest
  release, except for Sentinel: Update installs the latest Sentinel release.
- The system channels are APT, the pinned DEBs of Code and Tailscale, Homebrew
  formulae and casks, and the signed Windows installers. Update changes a
  present system item only when it has a record. The pinned routes refuse a
  downgrade. A cask application updates itself.
- Check only reads. It never installs or repairs.
- The apply log is a record, not evidence.

## Sentinel

[Sentinel](https://github.com/lgmr-dev/sentinel) holds the shared rules
and skills for coding agents.

- The `sentinel` capability needs the hidden `integration.sentinel-installer`
  resource. That resource needs `tool.git`.
- Install and update take the latest Sentinel release from the GitHub API. It
  must be published, immutable and named `vX.Y.Z`. `installer/hooks/sentinel.go`
  holds the floor release (v0.4.0) and its commit. Update refuses an older
  release and a tag that names another commit. A newer release or a failed
  lookup is a note, not a health issue.
- Apply fetches the commit of that release into `<state>/sentinel/checkout`.
  Git runs without system or global configuration. `HEAD` must equal the
  commit, and each file must equal the tree of the commit as raw bytes. Git
  index flags, local filters and replacement objects do not count.
- Then dotfiles runs the Sentinel installer of that checkout. Dotfiles does not
  copy or edit Sentinel files. Check, repair, removal and a rerun run Sentinel
  only from a verified checkout of the recorded commit. Repair, removal and a
  rerun fetch it again when necessary. An install older than the floor is a
  health issue: check does not run its installer, repair installs the target
  release, and removal uses the floor.
- The checkout is a recorded `tree` target, with its `.git` folder. Setup
  replaces or deletes a checkout that matches its record. It keeps each other
  checkout: it moves that checkout aside, and Check reports it. A checkout that
  does not match its record never keeps Sentinel on the machine.
- `<state>/sentinel/record.json` saves the intent before a change and the
  completion after it. A pending mark covers the call to the Sentinel
  installer. After an interruption, the next run runs the installer again. The
  record also keeps the release tag and commit, the hosts and the five folder
  variables of the install for check and removal.
- The hosts are Codex, Claude Code, Copilot, Kiro and OpenCode. A host counts
  when its command or folder exists, or when the same selection installs it.
- Check runs `--check`. Exit 1 means drift. Any other failure is an error.

## Product configuration

- Neovim sets the Space leader before it loads Lazy. Only Rose Pine loads
  early, with priority 1000.
- Conform is the only format-on-save handler. `:WNF` saves one time without a
  format.
- Use `vim.uv`, `client:supports_method(...)` and `vim.lsp.log.set_level(...)`.
- render-markdown owns Markdown rendering. The Obsidian UI stays disabled. The
  Obsidian plugin starts only with an existing vault. Never create a vault.
- Do not install copies of the bundled Neovim parsers: `c`, `lua`, `markdown`,
  `markdown_inline`, `query`, `vim` and `vimdoc`.
- Neovim runs from private runtime generations. Mason uses the snapshot in
  `nvim/mason-registry/`. Never resolve the latest version.
- zsh: Tab opens fzf-tab over compinit, then autosuggestions. Keep Ctrl-R,
  Ctrl-T, Alt-C and the prefix history search on Up and Down. Ctrl-R opens fzf,
  or Atuin when Atuin is selected; Atuin never takes Up, Down or `?`. Enable vi
  mode before the key bindings. Never bind a bare Escape to `kill-whole-line`.
- No global binding uses a bare Alt-h, Alt-j, Alt-k, Alt-l or Alt-c. These
  chords belong to Neovim and fzf. AeroSpace uses Ctrl-Alt chords.
  `tests/aerospace/keymap_test.sh` checks this rule.
- PowerShell enables PSReadLine vi mode before the key bindings. Interactive
  setup stops for batch, encoded, redirected and CI hosts. Profiles never use
  `Invoke-Expression`.
- tmux uses Ctrl-B, windows and panes from 1, and the four pinned plugins. There
  is no TPM. Never call the continuum auto-start handler.
- Herdr runs on all three systems. tmux runs on macOS and Linux. Do not set the
  Herdr `default_shell` on Windows.
- LazyGit keeps the upstream keybindings. Starship git-status styles use
  `($style)`.
- The shell profile puts managed folders before system and personal `PATH`
  entries. Local hooks run after it.
- The `zsh-handoff` capability starts zsh from an interactive bash terminal. It
  never calls `chsh`.
- Claude Code gets `DISABLE_UPDATES=1`. Copilot gets `COPILOT_AUTO_UPDATE=false`.
  `integration.kiro` owns only `app.disableAutoupdates`.
- Kiro CLI is not on Windows, because Kiro ships it there only as an MSI.
- AeroSpace keeps its default layout. Do not pin applications to workspaces.
- The Mac keyboard shortcuts on Linux use xremap. In a terminal, Cmd never
  sends a control character. The only privilege is one udev rule.

## Conventions

- Go uses `gofmt` with tabs. Raw Go strings also pass EditorConfig. Lua uses
  StyLua with two spaces. Other files follow `.editorconfig`.
- `.gitattributes` keeps all text LF, also Windows scripts.
- Never edit a generated file. Change its source and run its generator in
  `scripts/`.
- A new editor plugin changes its spec and `nvim/lazy-lock.json` together.
- A new LSP, formatter or parser changes `tests/nvim/language_matrix.lua`, the
  plugin configuration and the strict smoke fixture.
- A new hidden native dependency goes into the catalog with its platform
  bindings.
- PowerShell helpers use singular nouns. Do not suppress `PSUseSingularNouns`.
- Add an abstraction only for duplication that exists now.
- When a key, command, configuration path or platform changes, change the
  README cheat sheet in the same change.

## Tests

- A bug fix needs a regression test that fails before the fix.
- Prefer process-boundary tests. A native lifecycle on a disposable runner is
  the highest proof. Cross-compilation is not native proof.
- The Neovim spec runners isolate the XDG data, state and cache folders.
- Shell tests use the fixtures that `TestWriteActiveShellFixtures` writes.
- Terminal tests use real PTY and ConPTY sessions.
- Git fixtures remove inherited `GIT_*` variables.
- The Go race suite has a 30-minute limit. Do not increase it to hide a hang.
- `featuremap.json` holds the package boundaries. `make deps-check` fails on an
  import that crosses a boundary. Correct the import. Do not add it to
  `featuremap.baseline.json`.
- A new package goes into `featuremap.json` with its `mayUse` and
  `mustNotReach` rules.

## Security and releases

- The repository policy keeps CodeQL, private vulnerability reporting and
  immutable releases.
- The owner sets branch protection in the GitHub settings.
  `docs/security/branch-protection.md` lists the required checks. A document
  does not prove the live settings.
- Releases follow `docs/RELEASING.md`. The owner makes the immutable `vX.Y.Z`
  tag and publishes the notes from `docs/releases/` in one release action.
- When a release has a fault, a newer version replaces it. Never move the tag.
- Archive and Linux Code DEB pins change only with review and native runtime
  proof. The `e2e` jobs give that proof. Renovate covers exactly its extraction
  inventory.
- The archive pin workflow never merges. Its pull request must pass the
  native checks.
