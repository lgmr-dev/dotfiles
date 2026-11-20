package builds

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/luisgui1757/dotfiles/installer/catalog"
)

const azureCLIPackages = "libexec/lib/python3.14/site-packages"
const azureCLIPython = "bin/python3.14"
const azureCLICommand = "commands/az"
const azureCLIMain = azureCLIPackages + "/azure/cli/__main__.py"

const azureCLILauncher = `#!/bin/sh
set -eu
az_root=$(CDPATH= cd -- "$(/usr/bin/dirname -- "$0")/.." && pwd -P)
PYTHONPATH=$az_root/` + azureCLIPackages + `
AZ_INSTALLER=TARBALL
export PYTHONPATH AZ_INSTALLER
exec "$az_root/python/` + azureCLIPython + `" -B -s -P -m azure.cli "$@"
`

func (p *Provider) prepareAzureCLI(ctx context.Context, pin catalog.ArchivePin, payload string) (result error) {
	if err := p.download(ctx, pin.AzureCLI.Python, filepath.Join(payload, "python")); err != nil {
		return err
	}
	root, err := os.OpenRoot(payload)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	if err := root.Mkdir(filepath.Dir(azureCLICommand), 0755); err != nil {
		return err
	}
	file, err := root.OpenFile(azureCLICommand, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
	if err != nil {
		return err
	}
	_, err = file.WriteString(azureCLILauncher)
	if err := errors.Join(err, file.Sync(), file.Close()); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	for _, args := range [][]string{
		{"-b", "--invalidation-mode", "unchecked-hash", azureCLIPackages},
		{"--invalidation-mode", "checked-hash", "python/lib/python3.14"},
	} {
		cmd := exec.CommandContext(ctx, filepath.Join(payload, "python", azureCLIPython), append([]string{"-I", "-B", "-W", "ignore", "-m", "compileall", "-q", "-f", "-j", "1", "-o", "0"}, args...)...)
		cmd.Dir = payload
		cmd.WaitDelay = 2 * time.Second
		var output boundedOutput
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("prepare verified Azure CLI bytecode: %w\n%s", err, output.data.String())
		}
	}
	return fs.WalkDir(root.FS(), azureCLIPackages, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() || filepath.Ext(name) != ".py" {
			return err
		}
		if info, err := root.Lstat(name + "c"); err != nil || !info.Mode().IsRegular() {
			return errors.Join(fmt.Errorf("Azure CLI module %s has no bytecode", name), err)
		}
		return root.Remove(name)
	})
}
