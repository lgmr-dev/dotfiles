# Installer

The setup entrypoints start `cmd/dotfiles` through the verified Go bootstrap.
The [operator guide](../README.md) lists the tools, the platforms and the
commands. [CLAUDE.md](../CLAUDE.md) holds the rules for a change. This file
is the map of the packages.

Release v3.0.0 imports the state of v2.3.0 one time. See
[From v2 to v3.0.0](../docs/UPGRADING.md#from-v2-to-v300).

## Packages

`featuremap.json` at the repository root holds the allowed imports. `make
deps-check` checks them.

| Package | Content |
|---|---|
| `platform` | Host discovery, known folders, the state folder, the lock, strict JSON, the atomic save and the file system primitives |
| `tools` | The embedded tool folders. `tools/<name>/` holds `tool.json`, the pin files and the shell fragments in `shell/`. |
| `catalog` | The catalog and pin types, load, validation, the dependency closure, the menu entries and the target resolution |
| `state` | State schema 2, the store, the record operations, the pending marks and the system records |
| `fetch` | Download with `Range` requests after a broken connection, the SHA-256 check and the archive extraction |
| `engine` | The planner, retention, the run loop, Check, the provider and UI contracts, progress and the apply log |
| `targets` | The target kinds `file`, `tree`, `link`, `block`, `field` and `font`, and the write order |
| `builds` | Private builds, their manifests, the `current` link, the sweep and the runtime recipes |
| `system` | APT, the pinned DEBs, Homebrew formulae and casks, the Homebrew bootstrap, Apple CLT, the Windows vendor installers and the keyboard rule |
| `hooks` | Compiled integrations: shell initialization, settings, desktop entries, fonts, Neovim sync and Sentinel |
| `terminal` | The menu, the review screen and the progress display |
| `v2import` | The schema-1 readers, the import and `restore-v2`. Release v3.1.0 deletes this package. |
| `cmd/dotfiles` | The argument parser and the wiring of the providers |

`engine` does not import a provider package. `state` does not import
`catalog`. Only `cmd/dotfiles` imports `v2import`.

## Contracts

- **Catalog.** `tools/<name>/tool.json` is the only dependency list. A menu
  entry is a resource with `capability`. A resource reaches its provider
  through its action, its ID or its platform binding. `cmd/dotfiles/wire.go`
  holds the routes.
- **State.** `<state>/state.json` is strict schema-2 JSON. It holds the
  selection, the kept resources, the system records, the pending marks and one
  record for each target. `<state>/mutation.lock` is the installer lock. It
  has the same name as in v2, so the two installers exclude each other.
- **Provider.** A provider has `Inspect`, `Apply` and `Remove`. `Inspect` only
  reads and returns a finding with a token. Before each step, the engine
  inspects again and compares the token. After the step, it inspects again to
  verify.
- **Run loop.** The engine computes the closure of the selected, kept and
  retained resources and shows the plan. A failed step blocks its dependents,
  and independent steps continue. A step that makes a provider available ends
  the pass. The engine saves no plan.
- **Targets.** The write order is: save the first baseline, publish `next`,
  check the destination again, write, verify, promote `next` to `applied`. A
  removal sets `release` and restores the baseline.
- **Private builds.** A build has the path
  `<state>/tools/<id>/<version>-<digest>-<serial>`. Setup checks it from that
  path and writes its manifest. Then `<state>/tools/<id>/current` moves to it.
  After a pass with no failure, the sweep removes unused builds that match
  their manifest.
- **System packages.** Install and update run in the foreground after the
  approval. A provider writes a pending mark before a native command that can
  continue after setup stops. Setup never removes a system package or a
  vendor application: `Inspect` returns a command block for the operator.
- **Commands.** `plan`, `apply --yes`, `update --yes`, `check` and
  `restore-v2 --yes`. Exit code 0 is done, 1 is a failure or a difference, and
  3 tells the caller to run the same command again.

## Tests

Use the Go version in `go.mod`, then run `make test-installer`. It runs
`gofmt`, `go vet` and the race suite of all packages. Tests use fakes and
temporary folders. Git fixtures remove inherited `GIT_*` variables. Terminal
tests use real PTY and ConPTY sessions.

The native proof is in `tests/e2e/`, on disposable GitHub runners.
`DOTFILES_TEST_NATIVE=1` enables it. Never set this variable on an everyday
workstation. `run.sh` and `run.ps1` install each tool of a system, run it,
update it and remove it, then compare the home folder with the lists in
`tests/e2e/expected/`. `migrate.sh` and `migrate.ps1` install with v2.3.0,
import, run `check`, restore v2 and run the v2.3.0 Check.
