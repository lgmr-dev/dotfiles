package hooks

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lgmr-dev/dotfiles/installer/engine"
	"github.com/lgmr-dev/dotfiles/installer/platform"
	"github.com/lgmr-dev/dotfiles/installer/state"
	"github.com/lgmr-dev/dotfiles/installer/targets"
)

func (p *Profiles) terminalUnits() ([]targets.Desired, error) {
	if p.Context.OS != "windows" {
		return nil, errors.New("Windows Terminal is available on Windows only")
	}
	pwsh, err := p.shellCommand("tool.powershell", "pwsh")
	if err != nil {
		return nil, err
	}
	for _, path := range []string{p.Folders.LocalAppData, p.Checkout, pwsh} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return nil, errors.New("Windows Terminal requires observed LocalAppData and canonical repository/managed PowerShell paths")
		}
	}
	data, err := platform.ReadDocument(filepath.Join(p.Checkout, "windows-terminal", "settings.fragment.jsonc"))
	if err != nil {
		return nil, err
	}
	projection, err := terminalExtract(data)
	if err != nil {
		return nil, err
	}
	remainder, err := terminalReplace(data, nil)
	if err != nil {
		return nil, err
	}
	properties, err := terminalProperties(remainder)
	if err != nil || len(properties) != 0 {
		return nil, errors.Join(errors.New("Windows Terminal source has settings outside its reviewed fixed recipe"), err)
	}
	recipe, err := terminalDecodeProjection(projection)
	if err != nil {
		return nil, err
	}
	recipe.Values["profile/commandline"], err = json.Marshal(`"` + pwsh + `"`)
	if err != nil {
		return nil, err
	}
	projection, err = terminalProjectionData(recipe)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(p.Folders.LocalAppData, "Microsoft", "Windows Terminal", "settings.json")
	return []targets.Desired{{Kind: "field", Path: path, Slot: TerminalSlot, Content: projection}}, nil
}

func terminalBaseline(d targets.Desired, rec engine.Records) error {
	if _, has := rec.State().Targets[d.Key]; has {
		return nil
	}
	data, err := profileRead(d.Path)
	if err != nil || len(data) == 0 {
		return err
	}
	raw, err := terminalExtract(data)
	if err != nil || len(raw) == 0 {
		return err
	}
	projection, err := terminalDecodeProjection(raw)
	if err != nil || len(projection.Values) != 0 {
		return err
	}
	return rec.Update(func(s *state.State) error {
		s.Targets[d.Key] = state.Record{Resource: d.Resource, Kind: d.Kind, Before: "value:" + base64.StdEncoding.EncodeToString(raw)}
		return nil
	})
}

type terminalSlot struct{}

func (terminalSlot) Extract(data []byte, _ targets.Desired) ([]byte, bool, error) {
	if len(data) == 0 {
		return nil, false, errors.New("existing JSON settings are empty; preserve and correct the file before continuing")
	}
	raw, err := terminalExtract(data)
	if err != nil {
		return nil, false, err
	}
	projection, err := terminalDecodeProjection(raw)
	if err != nil || len(projection.Values) == 0 {
		return nil, false, err
	}
	return raw, true, nil
}

func (terminalSlot) Render(d targets.Desired) ([]byte, error) {
	recipe, err := terminalDecodeProjection(d.Content)
	if err != nil {
		return nil, err
	}
	defaultValue, err := terminalValue(recipe.Default)
	var defaultProfile string
	if err != nil || json.Unmarshal(defaultValue, &defaultProfile) != nil || defaultProfile != terminalProfileGUID {
		return nil, errors.New("Windows Terminal recipe must declare its managed PowerShell startup profile")
	}
	for _, field := range terminalScalars {
		if _, ok := recipe.Values[field]; !ok {
			return nil, errors.New("Windows Terminal recipe is missing a fixed setting")
		}
	}
	for _, collection := range terminalCollections() {
		for _, field := range collection.Fields {
			if _, ok := recipe.Values[collection.ID+"/"+field]; !ok {
				return nil, errors.New("Windows Terminal recipe is missing a fixed profile, theme or keybinding setting")
			}
		}
	}
	return terminalProjectionData(recipe)
}

func (terminalSlot) Replace(data []byte, _ targets.Desired, value []byte, present bool) ([]byte, error) {
	if !present {
		value = nil
	}
	return terminalReplace(data, value)
}

func (terminalSlot) Empty(data []byte, d targets.Desired) (bool, error) {
	return targets.Field{JSONC: func(string) bool { return true }}.Empty(data, d)
}

func (terminalSlot) Fingerprint(value []byte) (string, error) {
	projection, err := terminalDecodeProjection(value)
	if err != nil {
		return "", err
	}
	if len(projection.Values) == 0 {
		return targets.Absent, nil
	}
	return terminalCanonical(value)
}

func terminalReplace(data, replacement []byte) ([]byte, error) {
	if _, err := terminalExtract(data); err != nil {
		return nil, err
	}
	want, err := terminalDecodeProjection(replacement)
	if err != nil {
		return nil, err
	}
	result := bytes.Clone(data)
	if len(result) == 0 {
		result = []byte("{}\n")
	}
	for _, field := range terminalScalars {
		result, err = terminalSet(result, field, want.Values[field])
		if err != nil {
			return nil, err
		}
	}
	for _, collection := range terminalCollections() {
		array, present, err := terminalGet(result, collection.Path)
		if err != nil {
			return nil, err
		}
		needed := slices.Contains(want.Containers, collection.ID)
		for _, field := range collection.Fields {
			needed = needed || want.Values[collection.ID+"/"+field] != nil
		}
		if !present {
			if !needed {
				continue
			}
			array = []byte("[]")
		}
		entry, index, err := terminalEntry(array, collection)
		if err != nil {
			return nil, err
		}
		if entry == nil {
			if !needed {
				continue
			}
			entry, err = json.Marshal(map[string]string{collection.Key: collection.Match})
			if err != nil {
				return nil, err
			}
		}
		for _, field := range collection.Fields {
			entry, err = terminalSet(entry, field, want.Values[collection.ID+"/"+field])
			if err != nil {
				return nil, err
			}
		}
		for _, path := range []string{"tab", "tabRow", "window"} {
			if collection.ID != "theme" || slices.Contains(want.Containers, collection.ID+"/"+path) {
				continue
			}
			value, present, err := terminalGet(entry, path)
			if err != nil {
				return nil, err
			}
			if present {
				empty, err := terminalEmpty(value, false)
				if err != nil {
					return nil, err
				}
				if empty {
					entry, err = terminalSet(entry, path, nil)
					if err != nil {
						return nil, err
					}
				}
			}
		}
		if !needed {
			withoutKey, err := terminalSet(entry, collection.Key, nil)
			if err != nil {
				return nil, err
			}
			empty, err := terminalEmpty(withoutKey, false)
			if err != nil {
				return nil, err
			}
			if empty {
				entry = nil
			}
		}
		array, err = terminalReplaceElement(array, index, entry)
		if err != nil {
			return nil, err
		}
		result, err = terminalSet(result, collection.Path, array)
		if err != nil {
			return nil, err
		}
	}
	current, present, err := terminalGet(result, "defaultProfile")
	if err != nil {
		return nil, err
	}
	name := ""
	if present {
		value, err := terminalValue(current)
		if err != nil || json.Unmarshal(value, &name) != nil {
			return nil, errors.New("Windows Terminal defaultProfile must be a string")
		}
	}
	var prior string
	if len(want.Default) > 0 {
		value, err := terminalValue(want.Default)
		if err != nil || json.Unmarshal(value, &prior) != nil {
			return nil, errors.New("invalid saved Windows Terminal startup preference")
		}
	}
	removedDefault := false
	if strings.EqualFold(name, terminalProfileGUID) {
		collection := terminalCollections()[0]
		array, exists, err := terminalGet(result, collection.Path)
		if err != nil {
			return nil, err
		}
		removedDefault = !exists
		if exists {
			entry, _, err := terminalEntry(array, collection)
			if err != nil {
				return nil, err
			}
			removedDefault = entry == nil
		}
	}
	if !present || name == "" || strings.EqualFold(name, terminalProfileGUID) && prior == "" || removedDefault {
		result, err = terminalSet(result, "defaultProfile", want.Default)
		if err != nil {
			return nil, err
		}
	}
	for _, path := range []string{"profiles/defaults/font", "profiles/defaults", "profiles/list", "profiles", "schemes", "themes", "actions", "keybindings"} {
		if slices.Contains(want.Containers, path) {
			continue
		}
		value, present, err := terminalGet(result, path)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		array := slices.Contains([]string{"profiles/list", "schemes", "themes", "actions", "keybindings"}, path)
		empty, err := terminalEmpty(value, array)
		if err != nil {
			return nil, err
		}
		if empty {
			result, err = terminalSet(result, path, nil)
			if err != nil {
				return nil, err
			}
		}
	}
	if _, err := terminalExtract(result); err != nil {
		return nil, err
	}
	return result, nil
}
