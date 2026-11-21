package builds

import (
	"errors"
	"os"
)

const zenLauncher = `#!/bin/sh
set -eu
zen_dir=$(CDPATH= cd -- "$(/usr/bin/dirname -- "$0")" && pwd -P)
export MOZ_LEGACY_PROFILES=1
exec "$zen_dir/zen-bin" "$@"
`

func prepareZen(payload string) (result error) {
	root, err := os.OpenRoot(payload)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	if err := root.Remove("zen"); err != nil {
		return err
	}
	file, err := root.OpenFile("zen", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
	if err != nil {
		return err
	}
	_, err = file.WriteString(zenLauncher)
	if err := errors.Join(err, file.Sync(), file.Close()); err != nil {
		return err
	}
	lock, err := root.OpenFile(".parentlock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := errors.Join(lock.Sync(), lock.Close()); err != nil {
		return err
	}
	if err := root.Mkdir("distribution", 0755); err != nil {
		return err
	}
	return root.WriteFile("distribution/policies.json", []byte("{\"policies\":{\"DisableAppUpdate\":true}}\n"), 0644)
}
