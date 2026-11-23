package targets

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/lgmr-dev/dotfiles/installer/platform"
)

// Field is the Slot of one named property of the JSON object in a settings
// file. The slot is the property name and the value is its compact JSON
// value. Only the value and the necessary separator change: each other byte
// stays. JSONC selects, by the file path, the files that can hold comments
// and trailing commas, such as the VS Code settings; each other file is
// strict JSON. A duplicate property and a root that is not an object fail,
// because they give no clear baseline.
type Field struct{ JSONC func(path string) bool }

// JSONProperty is one property of a JSON object: its name, and the byte
// offsets of its key, of its value and of the end of its value.
type JSONProperty struct {
	Name            string
	Key, Value, End int
}

func validFieldName(field string) error {
	if field == "" || len(field) > 256 || !utf8.ValidString(field) || strings.ContainsAny(field, "\x00\r\n") {
		return errors.New("invalid or duplicate managed JSON field")
	}
	return nil
}

// JSONProperties returns the properties of the JSON object in data, in
// order. A root that is not an object, a duplicate property and content after
// the object fail.
func JSONProperties(data []byte) ([]JSONProperty, error) {
	if !utf8.Valid(data) || len(data) > platform.MaxProfileBytes {
		return nil, errors.New("JSON settings must be bounded UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("settings must contain a JSON object")
	}
	properties := []JSONProperty{}
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
		properties = append(properties, JSONProperty{name, key, value, int(decoder.InputOffset())})
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		return nil, errors.New("settings contain an incomplete JSON object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("settings contain trailing content")
	}
	return properties, nil
}

func compactJSON(value []byte) ([]byte, error) {
	var compact bytes.Buffer
	if len(bytes.TrimSpace(value)) == 0 || !json.Valid(value) {
		return nil, errors.New("a managed JSON field needs one JSON value")
	}
	if err := json.Compact(&compact, value); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}

func replaceJSONField(data []byte, field string, value []byte, keep bool) ([]byte, error) {
	if len(data) == 0 && !keep {
		return nil, nil
	}
	result := bytes.Clone(data)
	if len(result) == 0 {
		result = []byte("{}\n")
	}
	properties, err := JSONProperties(result)
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(properties, func(p JSONProperty) bool { return p.Name == field })
	start, end := 0, 0
	switch {
	case index >= 0:
		property := properties[index]
		start, end = property.Value, property.End
		if !keep {
			start = property.Key
			if index+1 < len(properties) {
				end = properties[index+1].Key
			} else if index > 0 {
				start = properties[index-1].End
			}
		}
	case keep:
		name, err := json.Marshal(field)
		if err != nil {
			return nil, err
		}
		value = append(append(name, ':'), value...)
		if len(properties) > 0 {
			start = properties[len(properties)-1].End
			value = append([]byte{','}, value...)
		} else {
			start = bytes.IndexByte(result, '{') + 1
		}
		end = start
	default:
		return result, nil
	}
	if !keep {
		value = nil
	}
	result = append(append(bytes.Clone(result[:start]), value...), result[end:]...)
	if _, err := JSONProperties(result); err != nil {
		return nil, err
	}
	return result, nil
}

// JSONCMask returns two copies of JSONC data with the same length. In
// lexical, spaces replace the comments and a byte order mark. In clean,
// spaces also replace the trailing commas, so clean is strict JSON.
func JSONCMask(data []byte) (clean, lexical []byte, err error) {
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

func replaceJSONCField(data []byte, field string, value []byte, keep bool) ([]byte, error) {
	if _, _, err := JSONCMask(data); err != nil {
		return nil, err
	}
	result := bytes.Clone(data)
	if len(result) == 0 {
		if !keep {
			return nil, nil
		}
		result = []byte("{}\n")
	}
	clean, lexical, err := JSONCMask(result)
	if err != nil {
		return nil, err
	}
	properties, err := JSONProperties(clean)
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(properties, func(p JSONProperty) bool { return p.Name == field })
	if index < 0 && !keep {
		return result, nil
	}
	start, end := 0, 0
	if index >= 0 {
		p := properties[index]
		start, end = p.Value, p.End
		if !keep {
			start, value = p.Key, nil
			right := len(lexical)
			if index+1 < len(properties) {
				right = properties[index+1].Key
			}
			comma := bytes.IndexByte(lexical[p.End:right], ',')
			if comma >= 0 {
				comma += p.End
			} else if index > 0 {
				left := properties[index-1].End
				comma = left + bytes.IndexByte(lexical[left:p.Key], ',')
			}
			if comma >= 0 {
				result = append(result[:comma], result[comma+1:]...)
				if comma < start {
					start--
					end--
				}
			}
		}
	} else {
		name, err := json.Marshal(field)
		if err != nil {
			return nil, err
		}
		value = append(append(name, ':'), value...)
		start = bytes.IndexByte(clean, '{') + 1
		if len(properties) != 0 {
			start = properties[len(properties)-1].End
			value = append([]byte{','}, value...)
		}
		end = start
	}
	result = append(append(bytes.Clone(result[:start]), value...), result[end:]...)
	clean, _, err = JSONCMask(result)
	if err == nil {
		_, err = JSONProperties(clean)
	}
	return result, err
}

func (f Field) jsonc(d Desired) bool { return f.JSONC != nil && f.JSONC(d.Path) }

// Extract implements Slot. An existing empty file fails: setup does not
// decide what an empty settings file means.
func (f Field) Extract(data []byte, d Desired) ([]byte, bool, error) {
	if err := validFieldName(d.Slot); err != nil {
		return nil, false, err
	}
	if len(data) == 0 {
		return nil, false, errors.New("existing JSON settings are empty; preserve and correct the file before continuing")
	}
	clean := data
	if f.jsonc(d) {
		var err error
		if clean, _, err = JSONCMask(data); err != nil {
			return nil, false, err
		}
	}
	properties, err := JSONProperties(clean)
	if err != nil {
		return nil, false, err
	}
	for _, property := range properties {
		if property.Name == d.Slot {
			value, err := compactJSON(clean[property.Value:property.End])
			return value, err == nil, err
		}
	}
	return nil, false, nil
}

// Render implements Slot.
func (f Field) Render(d Desired) ([]byte, error) {
	if err := validFieldName(d.Slot); err != nil {
		return nil, err
	}
	return compactJSON(d.Content)
}

// Replace implements Slot.
func (f Field) Replace(data []byte, d Desired, value []byte, present bool) ([]byte, error) {
	if err := validFieldName(d.Slot); err != nil {
		return nil, err
	}
	if present {
		var err error
		if value, err = compactJSON(value); err != nil {
			return nil, err
		}
	}
	if f.jsonc(d) {
		return replaceJSONCField(data, d.Slot, value, present)
	}
	return replaceJSONField(data, d.Slot, value, present)
}

// Empty implements Slot. A comment that the owner added to a file that setup
// created is personal content, so that file is not empty.
func (f Field) Empty(data []byte, d Desired) (bool, error) {
	if len(data) == 0 {
		return true, nil
	}
	if f.jsonc(d) {
		clean, lexical, err := JSONCMask(data)
		if err != nil {
			return false, err
		}
		properties, err := JSONProperties(clean)
		return len(properties) == 0 && bytes.Equal(lexical, data), err
	}
	properties, err := JSONProperties(data)
	return len(properties) == 0, err
}
