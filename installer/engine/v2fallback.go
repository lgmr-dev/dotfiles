package engine

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/lgmr-dev/dotfiles/installer/state"
)

const switchingLine = "The import from release v2 is not complete. Run setup to complete it, or run restore-v2 to go back to release v2."

// V2Folders are the folders in the state folder that release v2 wrote and
// that the import leaves in place, and the restore set of the import. Setup
// never deletes them.
var V2Folders = []string{"packages", "nvim-sync", "configuration", "profiles", "recovery", "v2-restore"}

// FallbackNotice returns the line that a preview shows for the v2_restore
// value of its plan. While the fallback is open, a confirmed apply or update
// closes it before its first step that applies or removes. It is empty for a
// check, for a state without a fallback and for an open fallback when the plan
// has no such step.
func FallbackNotice(plan Plan) string {
	if plan.Mode != "apply" && plan.Mode != "update" {
		return ""
	}
	switch plan.V2Restore {
	case "open":
		if !slices.ContainsFunc(plan.Operations, func(op Operation) bool { return mutating(op.Action) }) {
			return ""
		}
		return "This closes the v2 fallback: after you confirm this run, restore-v2 is not available. The v2 folders stay for your cleanup."
	case "switching":
		return switchingLine
	}
	return ""
}

const maxReferenceBytes = 8 << 20

func readBounded(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("it is not a regular file")
	}
	if info.Size() > maxReferenceBytes {
		return nil, errors.New("it is larger than 8 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxReferenceBytes))
	return data, errors.Join(err, file.Close())
}

var errUTF16 = errors.New("it starts with a UTF-16 byte order mark and does not decode")

func decodeText(data []byte) (string, error) {
	var order binary.ByteOrder
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}):
		order = binary.LittleEndian
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		order = binary.BigEndian
	default:
		return string(data), nil
	}
	body := data[2:]
	if len(body)%2 != 0 {
		return "", errUTF16
	}
	units := make([]uint16, len(body)/2)
	for i := range units {
		units[i] = order.Uint16(body[2*i:])
	}
	for i := 0; i < len(units); i++ {
		if !utf16.IsSurrogate(rune(units[i])) {
			continue
		}
		if i+1 == len(units) || utf16.DecodeRune(rune(units[i]), rune(units[i+1])) == unicode.ReplacementChar {
			return "", errUTF16
		}
		i++
	}
	return string(utf16.Decode(units)), nil
}

func readText(path string) (string, error) {
	data, err := readBounded(path)
	if err != nil {
		return "", err
	}
	return decodeText(data)
}

func names(text, folder string) bool {
	for _, form := range []string{folder, strings.ReplaceAll(folder, `\`, `\\`), filepath.ToSlash(folder)} {
		if strings.Contains(text, form) {
			return true
		}
	}
	return false
}

func treeTexts(root, origin string) ([]string, error) {
	texts := []string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir():
			return nil
		case entry.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0:
			target, err := os.Readlink(path)
			if err == nil && !filepath.IsAbs(target) {
				inside, _ := filepath.Rel(root, path)
				target = filepath.Join(filepath.Dir(filepath.Join(origin, inside)), target)
			}
			texts = append(texts, target)
			return err
		}
		text, err := readText(path)
		texts = append(texts, text)
		if errors.Is(err, errUTF16) {
			return errors.New(path + ": " + err.Error())
		}
		return err
	})
	return texts, err
}

func v2References(st state.State, folders []string) (users map[string]string, unreadable string) {
	users = map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(st.Targets)) {
		record := st.Targets[key]
		path := key
		if cut := strings.LastIndex(key, "#"); cut > 0 && (record.Kind == "block" || record.Kind == "field") {
			path = key[:cut]
		}
		texts := []string{}
		for _, value := range []string{record.Applied, record.Next, record.Before} {
			if encoded, inline := strings.CutPrefix(value, "value:"); inline {
				if data, err := base64.StdEncoding.DecodeString(encoded); err == nil {
					if value, err = decodeText(data); err != nil && unreadable == "" {
						unreadable = key + " (its recorded value): " + err.Error()
					}
				}
			}
			if backup, saved := strings.CutPrefix(value, "backup:"); saved {
				contents, err := treeTexts(backup, path)
				if err != nil && !errors.Is(err, os.ErrNotExist) && unreadable == "" {
					unreadable = key + " (its backup " + backup + "): " + err.Error()
				}
				texts = append(texts, contents...)
			}
			texts = append(texts, value)
		}
		var contents []string
		var err error
		switch record.Kind {
		case "link":
			contents, err = treeTexts(path, path)
		case "file", "block", "field":
			var text string
			text, err = readText(path)
			contents = []string{text}
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) && unreadable == "" {
			unreadable = key + ": " + err.Error()
		}
		texts = append(texts, contents...)
		for _, folder := range folders {
			if _, found := users[folder]; !found && slices.ContainsFunc(texts, func(text string) bool { return names(text, folder) }) {
				users[folder] = key
			}
		}
	}
	return users, unreadable
}

func (e *Engine) v2FolderReport(fallback string) string {
	present := []string{}
	for _, name := range V2Folders {
		folder := filepath.Join(e.StateDir, name)
		if _, err := os.Lstat(folder); err == nil {
			present = append(present, folder)
		}
	}
	if len(present) == 0 || fallback == "switching" {
		return ""
	}
	if fallback == "open" {
		return "\nNote: the v2 fallback is open. Keep these folders of release v2 while restore-v2 can use them: " + strings.Join(present, ", ")
	}
	st, err := state.Peek(e.StateDir, e.Target)
	if err != nil {
		return "\nNote: setup could not examine the folders of release v2: " + err.Error()
	}
	users, unreadable := v2References(st, present)
	message := ""
	for _, folder := range present {
		switch {
		case users[folder] != "":
			message += "\nNote: " + folder + " is a folder of release v2 still in use by " + users[folder] + "; keep it"
		case unreadable != "":
			message += "\nNote: setup cannot say if " + folder + " is still in use, because it could not read " + unreadable + "; keep it until check can read that target"
		default:
			message += "\nOwner cleanup: " + folder + " is a folder of release v2 that setup no longer uses; inspect it, then delete it yourself"
		}
	}
	return message + "\nNote: the reference check of the v2 folders covers the recorded link targets, the content of the recorded file, block and field targets, and the baselines. Another file can still name a path in these folders."
}
