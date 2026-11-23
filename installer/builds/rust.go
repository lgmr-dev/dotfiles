package builds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/platform"
)

func (p *Provider) prepareRust(ctx context.Context, pin catalog.ArchivePin, payload string) error {
	components := filepath.Join(payload, ".rust-components")
	if err := os.Mkdir(components, 0700); err != nil {
		return err
	}
	if err := mergeRustComponent(ctx, filepath.Join(payload, "rustc"), payload); err != nil {
		return err
	}
	names := catalog.RustComponentNames(pin.RustTarget)
	for index, component := range pin.RustComponents {
		source := filepath.Join(components, names[index])
		if err := p.download(ctx, component, source); err != nil {
			return err
		}
		if err := mergeRustComponent(ctx, filepath.Join(source, names[index]), payload); err != nil {
			return err
		}
	}
	for _, pattern := range []string{"libstd-*.rlib", "libcore-*.rlib"} {
		standard, err := filepath.Glob(filepath.Join(payload, "lib", "rustlib", pin.RustTarget, "lib", pattern))
		if err != nil || len(standard) != 1 {
			return errors.Join(errors.New("Rust standard library is incomplete"), err)
		}
	}
	return nil
}

func mergeRustComponent(ctx context.Context, source, payload string) error {
	for _, name := range []string{"bin", "lib", "libexec", "share", "etc"} {
		from := filepath.Join(source, name)
		if _, err := os.Lstat(from); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := mergeRustDirectory(ctx, from, filepath.Join(payload, name)); err != nil {
			return err
		}
	}
	return nil
}

func mergeRustDirectory(ctx context.Context, source, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := platform.ReadPlainDirectory(source)
	if err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0755); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if _, err := platform.ReadPlainDirectory(destination); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		from, to := filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())
		if entry.IsDir() {
			if err := mergeRustDirectory(ctx, from, to); err != nil {
				return err
			}
		} else {
			if err := platform.MoveNoReplace(from, to); err != nil {
				return fmt.Errorf("Rust component collision at %s: %w", to, err)
			}
		}
	}
	return os.Remove(source)
}

func (p *Provider) readWindowsRustLauncher(pin catalog.ArchivePin) ([]byte, error) {
	if pin.WindowsRustLauncher == nil {
		return nil, errors.New("the Windows Rust pin has no launcher recipe; use a checkout with the current pins")
	}
	if !sha256Text.MatchString(p.LauncherSHA256) || !filepath.IsAbs(p.Launcher) || filepath.Clean(p.Launcher) != p.Launcher {
		return nil, errors.New("Windows Rust preparation requires the observed installer executable and its SHA-256")
	}
	f, err := os.Open(p.Launcher)
	if err != nil {
		return nil, err
	}
	info, statErr := f.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return nil, errors.Join(errors.New("invalid Rust launcher executable"), statErr, f.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(f, 64<<20+1))
	if err := errors.Join(readErr, f.Close()); err != nil {
		return nil, err
	}
	hash := sha256.Sum256(data)
	if len(data) > 64<<20 || hex.EncodeToString(hash[:]) != p.LauncherSHA256 {
		return nil, errors.New("installer executable changed after Windows Rust launcher approval; run setup again")
	}
	return data, nil
}

func prepareWindowsRustLaunchers(payload string, launcher []byte) error {
	for _, name := range []string{"rustc.exe", "rustdoc.exe", "clippy-driver.exe"} {
		command := filepath.Join(payload, "bin", name)
		info, err := os.Lstat(command)
		if err != nil || !info.Mode().IsRegular() {
			return errors.Join(errors.New("Windows Rust lacks a required compiler executable"), err)
		}
		if err := platform.MoveNoReplace(command, filepath.Join(payload, "bin", rustOriginalCommand(name))); err != nil {
			return err
		}
		file, err := os.OpenFile(command, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(launcher)
		if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
			return err
		}
	}
	return nil
}
