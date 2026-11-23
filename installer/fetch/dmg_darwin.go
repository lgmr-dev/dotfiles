package fetch

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

func readDMGArchive(ctx context.Context, file *os.File, bundle string, readTar func(io.Reader) error) (result error) {
	mount, err := os.MkdirTemp(filepath.Dir(file.Name()), ".dmg-mount-")
	if err != nil {
		return err
	}
	attach, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(attach, "/usr/bin/hdiutil", "attach", "-readonly", "-nobrowse", "-noautoopen", "-mountpoint", mount, file.Name())
	var output boundedOutput
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return errors.Join(err, errors.New("disk-image attachment incomplete; inspect "+mount+platform.Diagnostic(output.data.Bytes())))
	}
	defer func() {
		detach, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(detach, "/usr/bin/hdiutil", "detach", mount)
		var output boundedOutput
		command.Stdout, command.Stderr = &output, &output
		if err := command.Run(); err != nil {
			result = errors.Join(result, err, errors.New("cannot detach private image at "+mount+platform.Diagnostic(output.data.Bytes())))
			return
		}
		result = errors.Join(result, os.Remove(mount))
	}()
	info, err := os.Lstat(filepath.Join(mount, bundle))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.Join(errors.New("disk image lacks its declared physical application bundle"), err)
	}
	return readNativeTar(ctx, file, "/usr/bin/tar", []string{"-cf", "-", "--no-mac-metadata", "-C", mount, bundle}, readTar)
}
