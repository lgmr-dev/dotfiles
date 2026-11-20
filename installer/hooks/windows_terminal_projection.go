package hooks

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/luisgui1757/dotfiles/installer/platform"
	"github.com/luisgui1757/dotfiles/installer/targets"
)

// TerminalSlot is the slot of the Windows Terminal settings projection: the
// reviewed settings of the fixed recipe in the Terminal settings file.
const TerminalSlot = "windows-terminal"

const terminalProfileGUID = "{8a0e8c9b-2b4c-5842-ac1b-29cd17efc89b}"

type terminalProjection struct {
	Schema     int               `json:"dotfiles_windows_terminal"`
	Values     map[string][]byte `json:"values,omitempty"`
	Containers []string          `json:"containers,omitempty"`
	Default    []byte            `json:"default,omitempty"`
}

type terminalCollection struct {
	ID, Path, Key, Match string
	Fields               []string
}

var terminalScalars = []string{
	"$schema", "copyFormatting", "copyOnSelect", "firstWindowPreference", "initialRows", "launchMode", "theme", "useAcrylicInTabRow", "windowingBehavior",
	"profiles/defaults/colorScheme", "profiles/defaults/font/face", "profiles/defaults/font/size", "profiles/defaults/historySize", "profiles/defaults/opacity", "profiles/defaults/useAcrylic", "profiles/defaults/padding", "profiles/defaults/antialiasingMode", "profiles/defaults/scrollbarState",
}

func terminalCollections() []terminalCollection {
	result := []terminalCollection{
		{"profile", "profiles/list", "guid", terminalProfileGUID, []string{"name", "commandline", "startingDirectory"}},
		{"scheme", "schemes", "name", "rose-pine", []string{"background", "foreground", "cursorColor", "selectionBackground", "black", "red", "green", "yellow", "blue", "purple", "cyan", "white", "brightBlack", "brightRed", "brightGreen", "brightYellow", "brightBlue", "brightPurple", "brightCyan", "brightWhite"}},
		{"theme", "themes", "name", "rose-pine", []string{"tab/background", "tab/iconStyle", "tab/showCloseButton", "tab/unfocusedBackground", "tabRow/background", "tabRow/unfocusedBackground", "window/applicationTheme", "window/experimental.rainbowFrame", "window/frame", "window/unfocusedFrame", "window/useMica"}},
	}
	result = append(result, terminalCollection{"close-tab", "actions", "id", "Dotfiles.CloseTab", []string{"command"}})
	for _, key := range []string{"ctrl+c", "ctrl+v", "ctrl+shift+f", "alt+shift+d", "ctrl+shift+w", "ctrl+t", "ctrl+tab", "ctrl+right", "ctrl+shift+tab", "ctrl+left", "ctrl+shift+up", "ctrl+shift+down", "ctrl+shift+right", "ctrl+shift+left", "ctrl+alt+w"} {
		result = append(result, terminalCollection{"binding:" + key, "keybindings", "keys", key, []string{"id"}})
	}
	return result
}

func terminalProjectionData(p terminalProjection) ([]byte, error) {
	if len(p.Values) == 0 && len(p.Containers) == 0 && len(p.Default) == 0 {
		return nil, nil
	}
	p.Schema = 1
	p.Containers = terminalSorted(p.Containers)
	data, err := json.Marshal(p)
	if len(data) > platform.MaxProfileBytes {
		return nil, errors.New("Windows Terminal owned projection exceeds its saved document bound")
	}
	return data, err
}

func terminalDecodeProjection(data []byte) (terminalProjection, error) {
	p := terminalProjection{Schema: 1, Values: map[string][]byte{}}
	if len(data) == 0 {
		return p, nil
	}
	if err := platform.Decode(data, &p); err != nil {
		return p, err
	}
	if p.Schema != 1 {
		return p, errors.New("invalid Windows Terminal projection schema")
	}
	allowed := map[string]bool{}
	containers := map[string]bool{"profiles": true, "profiles/defaults": true, "profiles/defaults/font": true}
	for _, field := range terminalScalars {
		allowed[field] = true
	}
	for _, collection := range terminalCollections() {
		containers[collection.Path], containers[collection.ID] = true, true
		for _, field := range collection.Fields {
			allowed[collection.ID+"/"+field] = true
			parts := strings.Split(field, "/")
			for i := 1; i < len(parts); i++ {
				containers[collection.ID+"/"+strings.Join(parts[:i], "/")] = true
			}
		}
	}
	for field, value := range p.Values {
		if !allowed[field] {
			return p, errors.New("Windows Terminal projection contains an unowned field")
		}
		if _, err := terminalValue(value); err != nil {
			return p, err
		}
	}
	seen := map[string]bool{}
	for _, container := range p.Containers {
		if !containers[container] || seen[container] {
			return p, errors.New("Windows Terminal projection has invalid or duplicate containers")
		}
		seen[container] = true
	}
	if len(p.Default) > 0 {
		value, err := terminalValue(p.Default)
		var name string
		if err != nil || json.Unmarshal(value, &name) != nil {
			return p, errors.New("Windows Terminal defaultProfile must be a string")
		}
	}
	return p, nil
}

func terminalValue(raw []byte) ([]byte, error) {
	clean, _, err := targets.JSONCMask(raw)
	if err != nil {
		return nil, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, clean); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}

type terminalCanonicalValue struct {
	JSON   json.RawMessage
	Number string
}

func terminalOwned(data []byte) (map[string]terminalCanonicalValue, error) {
	p, err := terminalDecodeProjection(data)
	if err != nil {
		return nil, err
	}
	values := map[string]terminalCanonicalValue{}
	for field, value := range p.Values {
		clean, err := terminalValue(value)
		if err != nil {
			return nil, err
		}
		value := terminalCanonicalValue{JSON: clean}
		if field == "close-tab/command" && bytes.Equal(clean, []byte(`{"action":"closeTab"}`)) {
			value.JSON = json.RawMessage(`"closeTab"`)
		}
		if len(clean) > 0 && (clean[0] == '-' || clean[0] >= '0' && clean[0] <= '9') {
			numberText := string(clean)
			bounded := len(numberText) <= 128
			if _, exponent, found := strings.Cut(strings.ToLower(numberText), "e"); found {
				n, err := strconv.Atoi(exponent)
				bounded = bounded && err == nil && n >= -1000 && n <= 1000
			}
			if bounded {
				if number, ok := new(big.Rat).SetString(numberText); ok {
					value = terminalCanonicalValue{Number: number.RatString()}
				}
			}
		}
		if strings.HasPrefix(field, "scheme/") || strings.HasPrefix(field, "theme/") {
			var color string
			if json.Unmarshal(clean, &color) == nil && strings.HasPrefix(color, "#") {
				color = strings.ToLower(color)
				if len(color) == 9 && strings.HasSuffix(color, "ff") {
					color = color[:7]
				}
				value.JSON, err = json.Marshal(color)
				if err != nil {
					return nil, err
				}
			}
		}
		values[field] = value
	}
	return values, nil
}

func terminalCanonical(raw []byte) (string, error) {
	owned, err := terminalOwned(raw)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(owned)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "field:" + hex.EncodeToString(sum[:]), nil
}

func terminalExtract(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	reader := terminalReader{}
	if _, err := reader.propertiesOf(data); err != nil {
		return nil, err
	}
	if err := reader.rejectLegacyBindings(data); err != nil {
		return nil, err
	}
	p := terminalProjection{Schema: 1, Values: map[string][]byte{}}
	for _, path := range []string{"profiles", "profiles/defaults", "profiles/defaults/font"} {
		value, present, err := reader.get(data, path)
		if err != nil {
			return nil, err
		}
		if present {
			if _, err := reader.propertiesOf(value); err != nil {
				return nil, fmt.Errorf("Windows Terminal %s must be an object: %w", path, err)
			}
			p.Containers = append(p.Containers, path)
		}
	}
	for _, field := range terminalScalars {
		value, present, err := reader.get(data, field)
		if err != nil {
			return nil, err
		}
		if present {
			p.Values[field] = bytes.Clone(value)
		}
	}
	if value, present, err := reader.get(data, "defaultProfile"); err != nil {
		return nil, err
	} else if present {
		p.Default = bytes.Clone(value)
	}
	for _, collection := range terminalCollections() {
		array, present, err := reader.get(data, collection.Path)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		p.Containers = append(p.Containers, collection.Path)
		entry, _, err := reader.entry(array, collection)
		if err != nil {
			return nil, err
		}
		if entry == nil {
			continue
		}
		p.Containers = append(p.Containers, collection.ID)
		for _, field := range collection.Fields {
			parts := strings.Split(field, "/")
			for i := 1; i < len(parts); i++ {
				path := strings.Join(parts[:i], "/")
				value, present, err := reader.get(entry, path)
				if err != nil {
					return nil, err
				}
				if present {
					if _, err := reader.propertiesOf(value); err != nil {
						return nil, err
					}
					p.Containers = append(p.Containers, collection.ID+"/"+path)
				}
			}
			value, present, err := reader.get(entry, field)
			if err != nil {
				return nil, err
			}
			if present {
				p.Values[collection.ID+"/"+field] = bytes.Clone(value)
			}
		}
	}
	encoded, err := terminalProjectionData(p)
	if err != nil {
		return nil, err
	}
	_, err = terminalDecodeProjection(encoded)
	return encoded, err
}
