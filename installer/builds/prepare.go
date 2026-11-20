package builds

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/luisgui1757/dotfiles/installer/catalog"
	"github.com/luisgui1757/dotfiles/installer/fetch"
	"github.com/luisgui1757/dotfiles/installer/platform"
)

const pythonResource = "tool.python"

func baseResources(pin catalog.ArchivePin) []string {
	if pin.Latex2text != nil || pin.Yamllint != nil {
		return []string{pythonResource}
	}
	return nil
}

func (p *Provider) download(ctx context.Context, pin catalog.ArchivePin, destination string) error {
	return fetch.Download(ctx, pin, destination, fetch.Options{Client: p.Client, Cache: filepath.Join(p.StateDir, "downloads"), DpkgDeb: p.DpkgDeb, Transfer: p.Transfer})
}

func (p *Provider) run(ctx context.Context, program string, environment []string, args ...string) error {
	if p.Runner == nil {
		return errors.New("the build needs a command runner")
	}
	_, err := p.Runner.Run(ctx, platform.Command{Path: program, Args: args, Env: environment})
	return err
}

func (p *Provider) prepare(ctx context.Context, pin catalog.ArchivePin, payload string, uses []string) (result error) {
	var launcher []byte
	if pin.RustTarget == "x86_64-pc-windows-msvc" {
		var err error
		launcher, err = p.readWindowsRustLauncher(pin)
		if err != nil {
			return err
		}
	}
	source := pin
	source.RustComponents, source.Latex2text = nil, nil
	source.Yamllint = nil
	source.WindowsVenvRevision = 0
	source.RustTarget = ""
	source.WindowsRustLauncher = nil
	source.PortableGit = false
	source.PortableGitRevision = 0
	source.GhosttyLibraries = false
	source.T3CodeLauncher = false
	source.ZenProfile = false
	source.ZenProfileRevision = 0
	source.AzureCLI = nil
	if pin.GhosttyLibraries {
		source.RequiredFiles = slices.DeleteFunc(slices.Clone(source.RequiredFiles), func(name string) bool { return name == "usr/bin/ghostty-bin" })
	}
	if pin.T3CodeLauncher {
		source.RequiredFiles = slices.DeleteFunc(slices.Clone(source.RequiredFiles), func(name string) bool { return name == "t3-code" })
	}
	if pin.AzureCLI != nil {
		source.Commands = nil
		source.RequiredFiles = []string{azureCLIMain}
	}
	if len(pin.RustComponents) > 0 {
		source.Commands = nil
		source.RequiredFiles = []string{"rustc/manifest.in", "rustc/" + pin.Commands["rustc"]}
	} else if pin.Latex2text != nil || pin.Yamllint != nil || pin.PortableGit {
		source.Commands = nil
		source.RequiredFiles = []string{pin.File}
	}
	if err := p.download(ctx, source, payload); err != nil {
		return err
	}
	switch {
	case len(pin.RustComponents) > 0:
		if err := p.prepareRust(ctx, pin, payload); err != nil {
			return err
		}
		if launcher != nil {
			if err := prepareWindowsRustLaunchers(payload, launcher); err != nil {
				return err
			}
		}
	case pin.Latex2text != nil:
		if err := p.prepareLatex2text(ctx, pin, payload, uses); err != nil {
			return err
		}
	case pin.Yamllint != nil:
		if err := p.prepareYamllint(ctx, pin, payload, uses); err != nil {
			return err
		}
	case pin.PortableGit:
		if err := p.preparePortableGit(ctx, pin, payload); err != nil {
			return err
		}
	case pin.ZenProfile:
		if err := prepareZen(payload); err != nil {
			return err
		}
	case pin.GhosttyLibraries:
		if err := prepareGhosttyLibraries(payload); err != nil {
			return err
		}
	case pin.T3CodeLauncher:
		if err := prepareT3CodeLauncher(payload); err != nil {
			return err
		}
	case pin.AzureCLI != nil:
		if err := p.prepareAzureCLI(ctx, pin, payload); err != nil {
			return err
		}
	default:
		if err := preparePython(ctx, pin, payload); err != nil {
			return err
		}
	}
	if err := checkPayload(payload, pin); err != nil {
		return err
	}
	return platform.SyncDirectory(payload)
}

func checkPayload(payload string, pin catalog.ArchivePin) error {
	root, err := os.OpenRoot(payload)
	if err != nil {
		return err
	}
	return errors.Join(fetch.CheckPayload(root, pin), root.Close())
}
