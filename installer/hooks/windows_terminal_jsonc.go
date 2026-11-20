package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/targets"
)

func terminalSorted(values []string) []string {
	result := append([]string{}, values...)
	slices.Sort(result)
	return slices.Compact(result)
}

func terminalProperties(data []byte) ([]targets.JSONProperty, error) {
	clean, _, err := targets.JSONCMask(data)
	if err != nil {
		return nil, err
	}
	return targets.JSONProperties(clean)
}

func (r *terminalReader) rejectLegacyBindings(data []byte) error {
	array, present, err := r.get(data, "actions")
	if err != nil || !present {
		return err
	}
	for _, collection := range terminalCollections() {
		if collection.Path != "keybindings" {
			continue
		}
		legacy := collection
		legacy.Path = "actions"
		entry, _, err := r.entry(array, legacy)
		if err != nil || entry != nil {
			return errors.New("Windows Terminal has an overlapping legacy combined action/key binding; preserve settings, launch the supported unpackaged stable Terminal once to migrate its settings, then retry")
		}
	}
	return nil
}

func terminalGet(data []byte, path string) ([]byte, bool, error) {
	return terminalLookup(data, path, terminalProperties)
}

func terminalLookup(data []byte, path string, propertiesOf func([]byte) ([]targets.JSONProperty, error)) ([]byte, bool, error) {
	for _, part := range strings.Split(path, "/") {
		properties, err := propertiesOf(data)
		if err != nil {
			return nil, false, err
		}
		index := slices.IndexFunc(properties, func(p targets.JSONProperty) bool { return p.Name == part })
		if index < 0 {
			return nil, false, nil
		}
		p := properties[index]
		data = data[p.Value:p.End]
	}
	return data, true, nil
}

func terminalSet(data []byte, path string, value []byte) ([]byte, error) {
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 2 {
		child, present, err := terminalGet(data, parts[0])
		if err != nil {
			return nil, err
		}
		if !present {
			if value == nil {
				return data, nil
			}
			child = []byte("{}")
		}
		value, err = terminalSet(child, parts[1], value)
		if err != nil {
			return nil, err
		}
	}
	clean, _, err := targets.JSONCMask(data)
	if err != nil {
		return nil, err
	}
	properties, err := targets.JSONProperties(clean)
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(properties, func(p targets.JSONProperty) bool { return p.Name == parts[0] })
	if index >= 0 && value != nil {
		p := properties[index]
		a, err := terminalValue(data[p.Value:p.End])
		if err != nil {
			return nil, err
		}
		b, err := terminalValue(value)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(a, b) {
			return data, nil
		}
		return append(append(bytes.Clone(data[:p.Value]), value...), data[p.End:]...), nil
	}
	field := targets.Field{JSONC: func(string) bool { return true }}
	if value == nil {
		return field.Replace(data, targets.Desired{Slot: parts[0]}, nil, false)
	}
	result, err := field.Replace(data, targets.Desired{Slot: parts[0]}, []byte("null"), true)
	if err != nil {
		return nil, err
	}
	properties, err = terminalProperties(result)
	if err != nil {
		return nil, err
	}
	index = slices.IndexFunc(properties, func(p targets.JSONProperty) bool { return p.Name == parts[0] })
	if index < 0 {
		return nil, errors.New("Windows Terminal field publication lost its key")
	}
	p := properties[index]
	return append(append(bytes.Clone(result[:p.Value]), value...), result[p.End:]...), nil
}

type terminalElement struct{ start, end int }

func terminalElements(data []byte) ([]terminalElement, []byte, error) {
	clean, lexical, err := targets.JSONCMask(data)
	if err != nil {
		return nil, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(clean))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('[') {
		return nil, nil, errors.New("Windows Terminal collection must be an array")
	}
	var result []terminalElement
	for decoder.More() {
		start := int(decoder.InputOffset())
		for start < len(clean) && bytes.ContainsRune([]byte(" \t\r\n,"), rune(clean[start])) {
			start++
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, nil, err
		}
		result = append(result, terminalElement{start, int(decoder.InputOffset())})
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim(']') {
		return nil, nil, errors.New("incomplete Windows Terminal collection")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, nil, errors.New("trailing Windows Terminal collection content")
	}
	return result, lexical, nil
}

func terminalIdentityEqual(selector, actual, expected string) bool {
	if selector == "keys" {
		a, b := strings.Split(strings.ToLower(actual), "+"), strings.Split(strings.ToLower(expected), "+")
		return slices.Equal(terminalSorted(a), terminalSorted(b))
	}
	if selector == "guid" {
		return strings.EqualFold(actual, expected)
	}
	return strings.EqualFold(actual, expected)
}

func terminalEntry(array []byte, collection terminalCollection) ([]byte, int, error) {
	return terminalFindEntry(array, collection, terminalElements, terminalGet)
}

func terminalFindEntry(array []byte, collection terminalCollection,
	elementsOf func([]byte) ([]terminalElement, []byte, error),
	get func([]byte, string) ([]byte, bool, error),
) ([]byte, int, error) {
	elements, _, err := elementsOf(array)
	if err != nil {
		return nil, -1, err
	}
	index := -1
	var result []byte
	for i, element := range elements {
		entry := array[element.start:element.end]
		value, present, err := get(entry, collection.Key)
		if err != nil {
			return nil, -1, err
		}
		if !present {
			continue
		}
		clean, err := terminalValue(value)
		if err != nil {
			return nil, -1, err
		}
		var key string
		if json.Unmarshal(clean, &key) != nil {
			if collection.Key == "keys" {
				var keys []string
				if json.Unmarshal(clean, &keys) != nil {
					return nil, -1, errors.New("Windows Terminal action keys must be strings or string arrays")
				}
				if slices.ContainsFunc(keys, func(key string) bool { return terminalIdentityEqual(collection.Key, key, collection.Match) }) {
					return nil, -1, errors.New("managed Windows Terminal action key appears in a grouped binding; normalize it to an individual binding before adoption")
				}
				continue
			}
			return nil, -1, errors.New("Windows Terminal profile/scheme identity must be a string")
		}
		if collection.Key == "guid" && strings.EqualFold(strings.Trim(key, "{}"), strings.Trim(collection.Match, "{}")) && !strings.EqualFold(key, collection.Match) {
			return nil, -1, errors.New("managed Windows Terminal profile GUID must use its canonical braces before adoption")
		}
		if collection.Key != "guid" && collection.Key != "keys" && strings.EqualFold(key, collection.Match) && key != collection.Match {
			return nil, -1, errors.New("managed Windows Terminal name or action ID differs only by case; normalize the case-sensitive reference before adoption")
		}
		if !terminalIdentityEqual(collection.Key, key, collection.Match) {
			continue
		}
		if index >= 0 {
			return nil, -1, fmt.Errorf("duplicate managed Windows Terminal %s identity", collection.ID)
		}
		result, index = entry, i
	}
	return result, index, nil
}

func terminalReplaceElement(array []byte, index int, value []byte) ([]byte, error) {
	elements, lexical, err := terminalElements(array)
	if err != nil {
		return nil, err
	}
	if index < 0 {
		if value == nil {
			return array, nil
		}
		at := bytes.IndexByte(lexical, '[') + 1
		if len(elements) > 0 {
			at = elements[len(elements)-1].end
			value = append([]byte{','}, value...)
		}
		return append(append(bytes.Clone(array[:at]), value...), array[at:]...), nil
	}
	start, end := elements[index].start, elements[index].end
	if value == nil {
		right := len(lexical)
		if index+1 < len(elements) {
			right = elements[index+1].start
		}
		comma := bytes.IndexByte(lexical[end:right], ',')
		if comma >= 0 {
			comma += end
		} else if index > 0 {
			left := elements[index-1].end
			comma = left + bytes.IndexByte(lexical[left:start], ',')
		}
		if comma >= 0 {
			array = append(bytes.Clone(array[:comma]), array[comma+1:]...)
			if comma < start {
				start--
				end--
			}
		}
	}
	return append(append(bytes.Clone(array[:start]), value...), array[end:]...), nil
}

func terminalEmpty(data []byte, array bool) (bool, error) {
	_, lexical, err := targets.JSONCMask(data)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(lexical, data) {
		return false, nil
	}
	if array {
		items, _, err := terminalElements(data)
		return len(items) == 0, err
	}
	properties, err := terminalProperties(data)
	return len(properties) == 0, err
}
