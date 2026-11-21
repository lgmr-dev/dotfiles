package builds

import (
	"errors"
	"os"
)

const ghosttyLibraryLauncher = `#!/bin/sh
set -eu
ghostty_bin_dir=$(CDPATH= cd -- "$(/usr/bin/dirname -- "$0")" && pwd -P)
LD_LIBRARY_PATH="$ghostty_bin_dir/../lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export LD_LIBRARY_PATH
exec "$ghostty_bin_dir/ghostty-bin" "$@"
`

func prepareGhosttyLibraries(payload string) (result error) {
	root, err := os.OpenRoot(payload)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	if _, err := root.Lstat("usr/bin/ghostty-bin"); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(errors.New("Ghostty prepared binary already exists"), err)
	}
	if err := root.Rename("usr/bin/ghostty", "usr/bin/ghostty-bin"); err != nil {
		return err
	}
	file, err := root.OpenFile("usr/bin/ghostty", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
	if err != nil {
		return err
	}
	_, err = file.WriteString(ghosttyLibraryLauncher)
	return errors.Join(err, file.Sync(), file.Close())
}
