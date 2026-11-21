package platform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxDocumentBytes = 8 * 1024 * 1024

// Decode reads exactly one JSON document of at most 8 MiB. An unknown field
// or content after the document fails.
func Decode(data []byte, v any) error {
	if len(data) > maxDocumentBytes {
		return fmt.Errorf("installer document exceeds 8 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("unexpected content after installer document")
	}
	return nil
}

// ReadDocument reads a regular file of at most 8 MiB beneath its folder. It
// refuses a linked folder and a file that changes while it opens. On Windows
// the open shares delete access, so SaveDocument can publish during a read.
func ReadDocument(path string) (data []byte, result error) {
	directory := filepath.Dir(path)
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("installer document directory must not be a link")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, root.Close()) }()
	name := filepath.Base(path)
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > maxDocumentBytes {
		return nil, errors.New("installer document must be a regular file of at most 8 MiB")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, file.Close()) }()
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) {
		return nil, errors.New("installer document changed while opening; retry discovery")
	}
	data, err = io.ReadAll(io.LimitReader(file, maxDocumentBytes+1))
	if len(data) > maxDocumentBytes {
		return nil, errors.New("installer document exceeds 8 MiB")
	}
	return data, err
}

// SaveDocument publishes complete bytes with mode 0600: it writes a temporary
// file in the same folder, flushes it, renames it over path and flushes the
// folder. The old document stays when a step fails.
func SaveDocument(path string, data []byte) (result error) {
	if len(data) > maxDocumentBytes {
		return errors.New("installer document exceeds 8 MiB; existing state preserved")
	}
	dir := filepath.Dir(path)
	if err := PrepareDirectory(dir); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("state destination must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}()
	if err := file.Chmod(0600); err != nil {
		return errors.Join(err, file.Close())
	}
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Close(); err != nil {
		return err
	}
	return publishDocument(name, path)
}

// PrepareDirectory creates a private folder and refuses a link in its place.
// A known folder above it can be redirected.
func PrepareDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("installer state directory must not be a link")
	}
	return nil
}
