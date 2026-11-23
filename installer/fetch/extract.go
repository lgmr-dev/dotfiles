package fetch

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/catalog"
	"github.com/lgmr-dev/dotfiles/installer/platform"
)

// Extract writes the verified archive in file to destination, which must not
// exist. It bounds the entries and the expanded size, refuses unsafe paths,
// hard links and special files, publishes symbolic links last, applies the
// reviewed replacements and checks the payload with CheckPayload. The caller
// verifies the SHA-256 of file first.
func Extract(ctx context.Context, file *os.File, size int64, pin catalog.ArchivePin, destination, dpkgDeb string) (result error) {
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	caseSensitive, err := caseSensitivity(root)
	if err != nil {
		return err
	}
	remaining, entries := MaxPackageBytes, 0
	seen := map[string]bool{}
	links := map[string]string{}
	excluded := map[string]bool{}
	write := func(raw string, mode os.FileMode, data io.Reader, link string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > MaxPackageEntries {
			return errors.New("archive exceeds entry limit")
		}
		name := strings.TrimSuffix(strings.TrimPrefix(raw, "./"), "/")
		if name == "." || name == "" {
			if mode.IsDir() {
				return nil
			}
			return errors.New("archive file has no name")
		}
		if !catalog.PortablePath(name) {
			return fmt.Errorf("unsafe archive path %q", raw)
		}
		parts := strings.Split(name, "/")
		if len(parts) <= pin.StripComponents {
			if mode.IsDir() {
				return nil
			}
			return errors.New("archive stripping would discard a file")
		}
		name = strings.Join(parts[pin.StripComponents:], "/")
		key := name
		if !caseSensitive {
			key = strings.ToLower(name)
		}
		if seen[key] {
			return fmt.Errorf("duplicate archive path %q", name)
		}
		seen[key] = true
		if mode&os.ModeSymlink != 0 && (strings.ContainsAny(link, "\\:\x00\r\n") || path.IsAbs(link) || !catalog.PortablePath(path.Clean(path.Join(path.Dir(name), link)))) {
			return errors.New("archive link escapes its payload")
		}
		if slices.Contains(pin.ExcludedFiles, name) {
			if !mode.IsRegular() && mode&os.ModeSymlink == 0 {
				return errors.New("archive exclusion must identify an exact regular file or symbolic link")
			}
			n, err := io.Copy(io.Discard, io.LimitReader(data, remaining+1))
			remaining -= n
			if remaining < 0 {
				return errors.Join(err, errors.New("archive exceeds expanded size limit"))
			}
			excluded[name] = true
			return err
		}
		if err := root.MkdirAll(filepath.FromSlash(path.Dir(name)), 0755); err != nil {
			return err
		}
		if mode.IsDir() {
			return root.MkdirAll(filepath.FromSlash(name), 0755)
		}
		if mode&os.ModeSymlink != 0 {
			links[name] = link
			return nil
		}
		if !mode.IsRegular() {
			return errors.New("archive contains unsupported special files")
		}
		output, err := root.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644|mode.Perm()&0111)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(output, io.LimitReader(data, remaining+1))
		err = errors.Join(copyErr, output.Sync(), output.Close())
		remaining -= n
		if remaining < 0 {
			return errors.Join(err, errors.New("archive exceeds expanded size limit"))
		}
		return err
	}
	readTar := func(stream io.Reader) error {
		reader := tar.NewReader(stream)
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if header.Typeflag == tar.TypeXGlobalHeader {
				entries++
				if entries > MaxPackageEntries {
					return errors.New("archive exceeds entry limit")
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				for key := range header.PAXRecords {
					if key != "comment" {
						return fmt.Errorf("unsupported global archive attribute %s", key)
					}
				}
				continue
			}
			if header.Typeflag == tar.TypeLink {
				return errors.New("archive hard links require an explicit supported layout")
			}
			if err := write(header.Name, header.FileInfo().Mode(), reader, header.Linkname); err != nil {
				return err
			}
		}
		trailing, err := io.Copy(io.Discard, io.LimitReader(stream, remaining+1))
		if err != nil {
			return err
		}
		if trailing > remaining {
			return errors.New("archive trailer exceeds expanded size limit")
		}
		return nil
	}
	switch pin.Format {
	case "tar.gz", "gzip":
		gz, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, gz.Close()) }()
		if pin.Format == "gzip" {
			if err := write(pin.File, 0755, gz, ""); err != nil {
				return err
			}
		} else {
			if err := readTar(gz); err != nil {
				return err
			}
		}
	case "deb":
		if err := readNativeTar(ctx, file, dpkgDeb, []string{"--fsys-tarfile", "-"}, readTar); err != nil {
			return err
		}
	case "tar.xz":
		if err := readNativeTar(ctx, file, "/usr/bin/xz", []string{"--decompress", "--stdout"}, readTar); err != nil {
			return err
		}
	case "dmg":
		if err := readDMGArchive(ctx, file, pin.AppBundle, readTar); err != nil {
			return err
		}
	case "zip":
		reader, err := zip.NewReader(file, size)
		if err != nil {
			return err
		}
		for _, member := range reader.File {
			input, err := member.Open()
			if err != nil {
				return err
			}
			link := ""
			if member.Mode()&os.ModeSymlink != 0 {
				data, readErr := io.ReadAll(io.LimitReader(input, 4097))
				remaining -= int64(len(data))
				if len(data) > 4096 || remaining < 0 {
					readErr = errors.Join(readErr, errors.New("ZIP symbolic link exceeds its size bound"))
				}
				if readErr != nil {
					return errors.Join(readErr, input.Close())
				}
				link = string(data)
			}
			err = errors.Join(write(member.Name, member.Mode(), input, link), input.Close())
			if err != nil {
				return err
			}
		}
	case "file":
		if err := write(pin.File, 0755, file, ""); err != nil {
			return err
		}
	default:
		return errors.New("unsupported archive format")
	}
	for _, name := range pin.ExcludedFiles {
		if !excluded[name] {
			return fmt.Errorf("reviewed archive exclusion is absent: %s", name)
		}
	}
	for name, link := range links {
		if err := root.Symlink(filepath.FromSlash(link), filepath.FromSlash(name)); err != nil {
			return err
		}
	}
	for name := range links {
		resolved, err := filepath.EvalSymlinks(filepath.Join(destination, filepath.FromSlash(name)))
		relative, relErr := filepath.Rel(destination, resolved)
		if err != nil || relErr != nil || !filepath.IsLocal(relative) {
			return errors.Join(errors.New("archive contains a dangling or escaping link"), err, relErr)
		}
	}
	for _, replacement := range pin.Replacements {
		if err := replaceText(root, replacement); err != nil {
			return err
		}
	}
	if err := CheckPayload(root, pin); err != nil {
		return err
	}
	return platform.SyncDirectory(destination)
}

// CheckPayload verifies that each required file of pin is a regular file and
// that each command of pin is a regular file that can run.
func CheckPayload(root *os.Root, pin catalog.ArchivePin) error {
	for _, name := range pin.RequiredFiles {
		info, err := root.Stat(filepath.FromSlash(name))
		if err != nil || !info.Mode().IsRegular() {
			return errors.Join(fmt.Errorf("archive lacks required file %s", name), err)
		}
	}
	for _, name := range pin.Commands {
		info, err := root.Stat(filepath.FromSlash(name))
		if err != nil || !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0 {
			return errors.Join(fmt.Errorf("archive lacks executable %s", name), err)
		}
	}
	return nil
}

func caseSensitivity(root *os.Root) (sensitive bool, result error) {
	const first = ".dotfiles-case-probe-A"
	const second = ".dotfiles-case-probe-a"
	file, err := root.OpenFile(first, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return false, err
	}
	defer func() { result = errors.Join(result, root.Remove(first)) }()
	if err := file.Close(); err != nil {
		return false, err
	}
	file, err = root.OpenFile(second, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, errors.Join(file.Close(), root.Remove(second))
}
