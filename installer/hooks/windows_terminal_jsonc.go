package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

type terminalProperty struct {
	name            string
	key, value, end int
}

func terminalJSONProperties(data []byte) ([]terminalProperty, error) {
	if !utf8.Valid(data) || len(data) > platform.MaxProfileBytes {
		return nil, errors.New("JSON settings must be bounded UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("settings must contain a JSON object")
	}
	properties := []terminalProperty{}
	seen := map[string]bool{}
	for decoder.More() {
		key := int(decoder.InputOffset())
		for key < len(data) && strings.ContainsRune(" \t\r\n,", rune(data[key])) {
			key++
		}
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return nil, errors.New("settings contain an invalid or duplicate JSON property")
		}
		seen[name] = true
		value := int(decoder.InputOffset())
		for value < len(data) && strings.ContainsRune(" \t\r\n:", rune(data[value])) {
			value++
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		properties = append(properties, terminalProperty{name, key, value, int(decoder.InputOffset())})
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return nil, errors.New("settings contain an incomplete JSON object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("settings contain trailing content")
	}
	return properties, nil
}

func terminalMask(data []byte) (clean, lexical []byte, err error) {
	if len(data) > platform.MaxProfileBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, nil, errors.New("JSONC settings must be bounded UTF-8")
	}
	lexical = bytes.Clone(data)
	if bytes.HasPrefix(lexical, []byte{0xef, 0xbb, 0xbf}) {
		copy(lexical, "   ")
	}
	for i := 0; i < len(lexical); i++ {
		switch lexical[i] {
		case '"':
			for i++; i < len(lexical); i++ {
				if lexical[i] == '\\' {
					i++
				} else if lexical[i] == '"' {
					break
				}
			}
		case '/':
			if i+1 == len(lexical) {
				continue
			}
			start := i
			if lexical[i+1] == '/' {
				for i += 2; i < len(lexical) && lexical[i] != '\n' && lexical[i] != '\r'; i++ {
				}
			} else if lexical[i+1] == '*' {
				end := bytes.Index(lexical[i+2:], []byte("*/"))
				if end < 0 {
					return nil, nil, errors.New("unterminated JSONC comment")
				}
				i += end + 4
			} else {
				continue
			}
			for j := start; j < i; j++ {
				if lexical[j] != '\r' && lexical[j] != '\n' {
					lexical[j] = ' '
				}
			}
			i--
		}
	}
	clean = bytes.Clone(lexical)
	for i := 0; i < len(clean); i++ {
		if clean[i] == '"' {
			for i++; i < len(clean); i++ {
				if clean[i] == '\\' {
					i++
				} else if clean[i] == '"' {
					break
				}
			}
		} else if clean[i] == ',' {
			previous := i - 1
			for previous >= 0 && bytes.ContainsRune([]byte(" \t\r\n"), rune(clean[previous])) {
				previous--
			}
			if previous < 0 || bytes.ContainsRune([]byte("{[:,"), rune(clean[previous])) {
				continue
			}
			j := i + 1
			for j < len(clean) && bytes.ContainsRune([]byte(" \t\r\n"), rune(clean[j])) {
				j++
			}
			if j < len(clean) && (clean[j] == '}' || clean[j] == ']') {
				clean[i] = ' '
			}
		}
	}
	return clean, lexical, nil
}

func terminalSorted(values []string) []string {
	result := append([]string{}, values...)
	slices.Sort(result)
	return slices.Compact(result)
}

func terminalProperties(data []byte) ([]terminalProperty, error) {
	clean, _, err := terminalMask(data)
	if err != nil {
		return nil, err
	}
	return terminalJSONProperties(clean)
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

func terminalLookup(data []byte, path string, propertiesOf func([]byte) ([]terminalProperty, error)) ([]byte, bool, error) {
	for _, part := range strings.Split(path, "/") {
		properties, err := propertiesOf(data)
		if err != nil {
			return nil, false, err
		}
		index := slices.IndexFunc(properties, func(p terminalProperty) bool { return p.name == part })
		if index < 0 {
			return nil, false, nil
		}
		p := properties[index]
		data = data[p.value:p.end]
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
	clean, _, err := terminalMask(data)
	if err != nil {
		return nil, err
	}
	properties, err := terminalJSONProperties(clean)
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(properties, func(p terminalProperty) bool { return p.name == parts[0] })
	if index >= 0 && value != nil {
		p := properties[index]
		a, err := terminalValue(data[p.value:p.end])
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
		return append(append(bytes.Clone(data[:p.value]), value...), data[p.end:]...), nil
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
	index = slices.IndexFunc(properties, func(p terminalProperty) bool { return p.name == parts[0] })
	if index < 0 {
		return nil, errors.New("Windows Terminal field publication lost its key")
	}
	p := properties[index]
	return append(append(bytes.Clone(result[:p.value]), value...), result[p.end:]...), nil
}

type terminalElement struct{ start, end int }

func terminalElements(data []byte) ([]terminalElement, []byte, error) {
	clean, lexical, err := terminalMask(data)
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
	_, lexical, err := terminalMask(data)
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
