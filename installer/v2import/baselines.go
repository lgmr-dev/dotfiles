package v2import

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/luisgui1757/dotfiles/installer/platform"
)

type configBaselineTarget struct {
	Destination string            `json:"destination"`
	Original    platform.Snapshot `json:"original"`
	Backup      string            `json:"backup"`
	Created     []string          `json:"created,omitempty"`
}

type configBaseline struct {
	Schema    int                    `json:"schema"`
	Resource  string                 `json:"resource"`
	Operation string                 `json:"operation"`
	Before    Observation            `json:"before"`
	Targets   []configBaselineTarget `json:"targets"`
}

type profileBaselineEntry struct {
	Path    string   `json:"path"`
	Block   []byte   `json:"block,omitempty"`
	Absent  bool     `json:"absent"`
	Created []string `json:"created,omitempty"`
}

type profileBaseline struct {
	Schema   int                    `json:"schema"`
	Resource string                 `json:"resource"`
	Before   Observation            `json:"before"`
	Entries  []profileBaselineEntry `json:"entries"`
	Fields   []string               `json:"fields,omitempty"`
}

func validRecovery(recovery string) bool {
	return filepath.IsLocal(recovery) && filepath.Clean(recovery) == recovery && filepath.ToSlash(filepath.Dir(filepath.Dir(recovery))) == "recovery"
}

func cleanAbsolute(path string) bool { return filepath.IsAbs(path) && filepath.Clean(path) == path }

func readConfigBaseline(stateDir, id string, receipt Receipt) (configBaseline, error) {
	var baseline configBaseline
	data, err := platform.ReadDocument(filepath.Join(stateDir, receipt.Recovery, "configuration.json"))
	if err != nil {
		return baseline, err
	}
	if err := platform.Decode(data, &baseline); err != nil {
		return baseline, err
	}
	if baseline.Schema != 1 || baseline.Resource != id || !operationID.MatchString(baseline.Operation) || !sameArtifact(baseline.Before, receipt.Before) || len(baseline.Targets) == 0 {
		return baseline, errors.New("configuration baseline differs from the original receipt")
	}
	for i, target := range baseline.Targets {
		workspace := filepath.Join(filepath.Dir(target.Destination), fmt.Sprintf(".dotfiles-config-%s-%d", baseline.Operation, i))
		if !cleanAbsolute(target.Destination) || target.Backup != filepath.Join(workspace, "previous") ||
			len(target.Created) != 0 && (target.Original.Kind != "absent" || !platform.ValidCreatedDirectories(target.Destination, target.Created)) {
			return baseline, errors.New("configuration baseline contains invalid adjacent recovery storage")
		}
	}
	return baseline, nil
}

func readProfileBaseline(stateDir, id string, receipt Receipt) (profileBaseline, error) {
	var baseline profileBaseline
	data, err := platform.ReadDocument(filepath.Join(stateDir, receipt.Recovery, "profile.json"))
	if err != nil {
		return baseline, err
	}
	if err := platform.Decode(data, &baseline); err != nil {
		return baseline, err
	}
	if baseline.Schema != 1 || baseline.Resource != id || !sameArtifact(baseline.Before, receipt.Before) || len(baseline.Entries) == 0 || len(baseline.Entries) > 8 {
		return baseline, errors.New("profile baseline differs from receipt")
	}
	for _, entry := range baseline.Entries {
		if !cleanAbsolute(entry.Path) || len(entry.Block) > platform.MaxProfileBytes || entry.Absent && len(entry.Block) != 0 ||
			len(entry.Created) != 0 && (!entry.Absent || !platform.ValidCreatedDirectories(entry.Path, entry.Created)) {
			return baseline, errors.New("profile baseline has invalid targets or blocks")
		}
	}
	return baseline, nil
}

func verifyProfileBaseline(id string, receipt Receipt, baseline profileBaseline) error {
	paths, blocks, present := []string{}, [][]byte{}, false
	for _, entry := range baseline.Entries {
		paths, blocks, present = append(paths, entry.Path), append(blocks, entry.Block), present || len(entry.Block) != 0
	}
	var identity any = paths
	if len(baseline.Fields) != 0 {
		identity = struct{ Paths, Fields []string }{paths, baseline.Fields}
	}
	identityDigest, identityErr := digest(identity)
	blocksDigest, blocksErr := digest(blocks)
	if err := errors.Join(identityErr, blocksErr); err != nil {
		return err
	}
	if identityDigest != baseline.Before.Identity {
		return errors.New("the paths and fields of the profile baseline differ from its receipt")
	}
	if id == terminalResource {
		if blocksDigest != baseline.Before.Inventory || blocksDigest != receipt.Before.Inventory {
			return errors.New("the saved Windows Terminal values differ from their receipt")
		}
		return nil
	}
	if blocksDigest != baseline.Before.Fingerprint || present != baseline.Before.Present {
		return errors.New("the saved profile values differ from their receipt")
	}
	return nil
}

func baselineExists(stateDir string, receipt Receipt, name string) bool {
	if !validRecovery(receipt.Recovery) {
		return false
	}
	_, err := os.Lstat(filepath.Join(stateDir, receipt.Recovery, name))
	return err == nil
}

func readProfileFile(path string) ([]byte, platform.Snapshot, error) {
	absent := platform.Snapshot{Kind: "absent"}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, absent, nil
	}
	if err != nil {
		return nil, absent, err
	}
	if !info.Mode().IsRegular() || info.Size() > platform.MaxProfileBytes {
		return nil, absent, errors.New("profile is not a regular file within its read bound")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, absent, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, absent, errors.Join(errors.New("profile changed while opening"), err, file.Close())
	}
	data, err := io.ReadAll(io.LimitReader(file, platform.MaxProfileBytes+1))
	if err = errors.Join(err, file.Close()); err != nil || len(data) > platform.MaxProfileBytes {
		return nil, absent, errors.Join(errors.New("profile exceeds its read bound"), err)
	}
	sum := sha256.Sum256(data)
	snapshot, err := platform.SnapshotEntries([]platform.Entry{{Path: ".", Kind: "file", Content: hex.EncodeToString(sum[:]), Mode: uint32(info.Mode().Perm())}})
	return data, snapshot, errors.Join(err, hit("read:"+path))
}

func readLink(path string) (string, platform.Snapshot, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", platform.Snapshot{}, err
	}
	kind := "link"
	switch {
	case info.Mode()&os.ModeIrregular != 0:
		kind = "junction"
	case info.Mode()&os.ModeSymlink == 0:
		return "", platform.Snapshot{}, errors.New("the runtime path is not a link")
	}
	target, err := os.Readlink(path)
	if err != nil {
		return "", platform.Snapshot{}, err
	}
	snapshot, err := platform.SnapshotEntries([]platform.Entry{{Path: ".", Kind: kind, Content: target}})
	return target, snapshot, errors.Join(err, hit("read:"+path))
}

func profileEncoding(data []byte) (func(string) []byte, error) {
	encode := func(s string) []byte { return []byte(s) }
	if bytes.HasPrefix(data, []byte{0xff, 0xfe}) || bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		var order binary.ByteOrder = binary.LittleEndian
		if data[0] == 0xfe {
			order = binary.BigEndian
		}
		if len(data)%2 != 0 {
			return nil, errors.New("profile has incomplete UTF-16 data")
		}
		encode = func(s string) []byte {
			units := utf16.Encode([]rune(s))
			out := make([]byte, 2*len(units))
			for i, value := range units {
				order.PutUint16(out[i*2:], value)
			}
			return out
		}
	} else if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("profile must be valid UTF-8 or BOM-marked UTF-16")
	}
	return encode, nil
}

func liveBlock(data []byte, id string) ([]byte, error) {
	encode, err := profileEncoding(data)
	if err != nil {
		return nil, err
	}
	begin, finish := "# >>> dotfiles:"+id+" >>>", "# <<< dotfiles:"+id+" <<<"
	begins, finishes := bytes.Count(data, encode(begin)), bytes.Count(data, encode(finish))
	if begins == 0 && finishes == 0 {
		return nil, nil
	}
	if begins != 1 || finishes != 1 {
		return nil, errors.New("profile block markers are duplicated or incomplete")
	}
	opening := 0
	for _, bom := range [][]byte{{0xef, 0xbb, 0xbf}, {0xff, 0xfe}, {0xfe, 0xff}} {
		if bytes.HasPrefix(data, bom) {
			opening = len(bom)
		}
	}
	for _, nl := range []string{"\r\n", "\n"} {
		left, right := encode(nl+begin+nl), encode(finish+nl)
		var missing []byte
		start := bytes.Index(data, left)
		if start < 0 && bytes.HasPrefix(data[opening:], encode(begin+nl)) {
			start, missing, left = opening, encode(nl), encode(begin+nl)
		}
		if start < 0 {
			continue
		}
		closing := bytes.Index(data[start+len(left):], right)
		if closing < 0 {
			continue
		}
		return append(bytes.Clone(missing), data[start:start+len(left)+closing+len(right)]...), nil
	}
	return nil, errors.New("profile block markers were edited")
}
