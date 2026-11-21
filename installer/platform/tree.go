package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// MaxTreeBytes and MaxTreeEntries bound a configuration snapshot.
const (
	MaxTreeBytes   = 128 * 1024 * 1024
	MaxTreeEntries = 4096
)

// Snapshot is the fingerprint of a path. Kind is absent, file, directory,
// link or junction. Hash covers each entry: its relative path, kind, mode and
// content hash or link target.
type Snapshot struct {
	Kind string `json:"kind"`
	Hash string `json:"hash,omitempty"`
}

// Entry is one member of an inspected tree. Content is the SHA-256 of a file
// or the target of a link. Mode is zero for a link, because the permissions
// of a native link change with the system and the umask.
type Entry struct {
	Path, Kind, Content string
	Mode                uint32
}

// ReadPlainDirectory lists a real folder and refuses a link or junction in
// its place. On Windows, ReadDir can report ErrNotExist for a regular file, so
// the entry itself is examined first.
func ReadPlainDirectory(path string) ([]os.DirEntry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return nil, fmt.Errorf("expected an unredirected directory at %s", path)
	}
	return os.ReadDir(path)
}

// SnapshotTree fingerprints path within the configuration bounds. It records
// a link itself and never reads what the link points at.
func SnapshotTree(path string) (Snapshot, error) {
	return SnapshotTreeBounded(path, MaxTreeBytes, MaxTreeEntries)
}

// SnapshotTreeBounded fingerprints path within the given bounds.
func SnapshotTreeBounded(path string, byteLimit int64, entryLimit int) (Snapshot, error) {
	entries, err := InspectTree(path, byteLimit, entryLimit)
	if err != nil {
		return Snapshot{}, err
	}
	return SnapshotEntries(entries)
}

// SnapshotEntries fingerprints inspected entries. No entry means absent.
func SnapshotEntries(entries []Entry) (Snapshot, error) {
	if len(entries) == 0 {
		return Snapshot{Kind: "absent"}, nil
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return Snapshot{}, err
	}
	hash := sha256.Sum256(encoded)
	return Snapshot{Kind: entries[0].Kind, Hash: hex.EncodeToString(hash[:])}, nil
}

// InspectTree lists path and each entry below it, within the bounds. A missing
// path gives no entry. It refuses a special file and a tree that changes
// during the inspection.
func InspectTree(path string, byteLimit int64, entryLimit int) ([]Entry, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries := []Entry{}
	remaining := byteLimit
	var walk func(string, string, os.FileInfo) error
	walk = func(name, relative string, before os.FileInfo) error {
		if len(entries) >= entryLimit {
			return errors.New("configuration exceeds the bounded entry count; preserve it for explicit migration")
		}
		entry := Entry{Path: relative, Mode: uint32(before.Mode().Perm())}
		switch {
		case before.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0:
			entry.Kind = "link"
			if before.Mode()&os.ModeIrregular != 0 {
				entry.Kind = "junction"
			}
			entry.Mode = 0
			entry.Content, err = os.Readlink(name)
			if err != nil {
				return err
			}
			if len(entry.Content) > 32768 {
				return errors.New("configuration link exceeds its size bound")
			}
		case before.IsDir():
			entry.Kind = "directory"
		case before.Mode().IsRegular():
			entry.Kind = "file"
			if before.Size() > remaining {
				return fmt.Errorf("tree exceeds %d bytes; preserve it for explicit migration", byteLimit)
			}
			file, err := os.Open(name)
			if err != nil {
				return err
			}
			opened, err := file.Stat()
			if err != nil || !os.SameFile(before, opened) {
				return errors.Join(errors.New("configuration changed while opening"), err, file.Close())
			}
			hash := sha256.New()
			count, copyErr := io.Copy(hash, io.LimitReader(file, remaining+1))
			if err := errors.Join(copyErr, file.Close()); err != nil {
				return err
			}
			remaining -= count
			if remaining < 0 {
				return errors.New("configuration grew beyond its size bound")
			}
			entry.Content = hex.EncodeToString(hash.Sum(nil))
		default:
			return errors.New("configuration contains a special file; preserve it for explicit migration")
		}
		entries = append(entries, entry)
		if before.IsDir() {
			children, err := os.ReadDir(name)
			if err != nil {
				return err
			}
			for _, child := range children {
				childPath := filepath.Join(name, child.Name())
				childInfo, err := os.Lstat(childPath)
				if err != nil {
					return err
				}
				if err := walk(childPath, filepath.Join(relative, child.Name()), childInfo); err != nil {
					return err
				}
			}
		}
		after, err := os.Lstat(name)
		same := err == nil && os.SameFile(before, after)
		if err != nil || !same || metadataChanged(before, after) {
			return inspectionFailure(relative, entry.Kind, before, after, same, err)
		}
		return nil
	}
	if err := walk(path, ".", info); err != nil {
		return nil, err
	}
	return entries, nil
}

// https://learn.microsoft.com/windows/win32/api/fileapi/ns-fileapi-win32_file_attribute_data
func metadataChanged(before, after os.FileInfo) bool {
	sizeDefined := runtime.GOOS != "windows" || !before.IsDir()
	return before.Mode() != after.Mode() || sizeDefined && before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime())
}

func inspectionFailure(relative, kind string, before, after os.FileInfo, same bool, statErr error) error {
	if len(relative) > 512 {
		relative = relative[:512] + "[truncated]"
	}
	metadata := func(info os.FileInfo) string {
		if info == nil {
			return "unavailable"
		}
		return fmt.Sprintf("mode=%s size=%d mtime_ns=%d", info.Mode(), info.Size(), info.ModTime().UnixNano())
	}
	return errors.Join(fmt.Errorf("configuration changed during inspection; review a fresh plan: entry=%q kind=%s same_file=%v before={%s} after={%s}", relative, kind, same, metadata(before), metadata(after)), statErr)
}

// CopyTree copies a regular file or a folder of regular files to a new
// destination and verifies the copy against the source snapshot. It refuses
// an existing destination and a link or special file in the source.
func CopyTree(source, destination string) error {
	return copyTree(source, destination, copyFileData, func(_, to string, info os.FileInfo, children func() error) error {
		if err := children(); err != nil {
			return err
		}
		return os.Chmod(to, info.Mode().Perm())
	})
}

// CopyTreeWithMetadata copies like CopyTree, and it also keeps the metadata
// that CopyWithMetadata keeps for each file and folder, whatever the size of
// a file. It verifies the extended attributes that the system can read, and
// it fails when the copy cannot keep them.
func CopyTreeWithMetadata(ctx context.Context, source, destination string) error {
	return copyTree(source, destination, func(from, to string, _ os.FileInfo, limit int64) (int64, error) {
		if err := copyFileWithMetadata(ctx, from, to, limit); err != nil {
			return 0, err
		}
		copied, err := os.Lstat(to)
		if err != nil {
			return 0, err
		}
		return copied.Size(), nil
	}, func(from, to string, info os.FileInfo, children func() error) error {
		return copyFolderMetadata(ctx, from, to, info, children)
	})
}

func copyTree(source, destination string, file func(from, to string, info os.FileInfo, limit int64) (int64, error), folder func(from, to string, info os.FileInfo, children func() error) error) error {
	before, err := SnapshotTree(source)
	if err != nil {
		return err
	}
	if before.Kind != "file" && before.Kind != "directory" {
		return errors.New("configuration payload must be a regular file or directory")
	}
	remainingBytes, remainingEntries := int64(MaxTreeBytes), MaxTreeEntries
	var copyOne func(string, string) error
	copyOne = func(from, to string) error {
		remainingEntries--
		if remainingEntries < 0 {
			return errors.New("configuration payload grew beyond its entry bound")
		}
		info, err := os.Lstat(from)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := os.Mkdir(to, 0700); err != nil {
				return err
			}
			err := folder(from, to, info, func() error {
				children, err := os.ReadDir(from)
				if err != nil {
					return err
				}
				for _, child := range children {
					if err := copyOne(filepath.Join(from, child.Name()), filepath.Join(to, child.Name())); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			return SyncDirectory(to)
		}
		if !info.Mode().IsRegular() {
			return errors.New("configuration payload contains a link or special file")
		}
		count, err := file(from, to, info, remainingBytes)
		remainingBytes -= count
		if remainingBytes < 0 {
			err = errors.Join(err, errors.New("configuration payload grew beyond its size bound"))
		}
		return err
	}
	if err := copyOne(source, destination); err != nil {
		return err
	}
	after, err := SnapshotTree(destination)
	if err != nil || before != after {
		return errors.Join(fmt.Errorf("staged configuration differs from its inspected source"), err)
	}
	return nil
}

func copyFileData(from, to string, info os.FileInfo, limit int64) (int64, error) {
	input, err := os.Open(from)
	if err != nil {
		return 0, err
	}
	opened, err := input.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return 0, errors.Join(errors.New("payload changed while opening"), err, input.Close())
	}
	output, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return 0, errors.Join(err, input.Close())
	}
	count, copyErr := io.Copy(output, io.LimitReader(input, limit+1))
	return count, errors.Join(copyErr, output.Chmod(info.Mode().Perm()), output.Sync(), output.Close(), input.Close())
}
