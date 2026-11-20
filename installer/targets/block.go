package targets

import (
	"bytes"
	"encoding/binary"
	"errors"
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Block is the Slot of a marked block in a text file. The slot is the
// identity in the two marker lines "# >>> dotfiles:<slot> >>>" and
// "# <<< dotfiles:<slot> <<<". The file is UTF-8, or UTF-16 with a byte
// order mark, which Windows PowerShell 5.1 profiles use. The byte order
// mark, the line endings, a missing final newline and each byte outside the
// block stay. The value is the block as UTF-8 text with LF line endings and
// its two marker lines. A new block goes to the end of the file, or with Top
// to its start, after a byte order mark.
type Block struct{ Top bool }

var blockIdentity = regexp.MustCompile(`^[a-z][a-z0-9.-]{0,79}$`)

type textEncoding struct {
	encode  func(string) []byte
	decode  func([]byte) string
	newline string
}

func encodingOf(data []byte) (textEncoding, error) {
	encoding := textEncoding{encode: func(s string) []byte { return []byte(s) }, decode: func(b []byte) string { return string(b) }}
	if bytes.HasPrefix(data, []byte{0xff, 0xfe}) || bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		var order binary.ByteOrder = binary.LittleEndian
		if data[0] == 0xfe {
			order = binary.BigEndian
		}
		if len(data)%2 != 0 {
			return encoding, errors.New("profile has incomplete UTF-16 data")
		}
		units := make([]uint16, (len(data)-2)/2)
		for i := range units {
			units[i] = order.Uint16(data[2+i*2:])
		}
		for i := 0; i < len(units); i++ {
			if utf16.IsSurrogate(rune(units[i])) {
				if i+1 == len(units) || units[i] < 0xd800 || units[i] > 0xdbff || units[i+1] < 0xdc00 || units[i+1] > 0xdfff {
					return encoding, errors.New("profile has invalid UTF-16 data")
				}
				i++
			}
		}
		encoding.encode = func(s string) []byte {
			encoded := utf16.Encode([]rune(s))
			out := make([]byte, 2*len(encoded))
			for i, value := range encoded {
				order.PutUint16(out[i*2:], value)
			}
			return out
		}
		encoding.decode = func(b []byte) string {
			decoded := make([]uint16, len(b)/2)
			for i := range decoded {
				decoded[i] = order.Uint16(b[i*2:])
			}
			return string(utf16.Decode(decoded))
		}
	} else if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return encoding, errors.New("profile must be valid UTF-8 or BOM-marked UTF-16; preserve it for explicit conversion")
	}
	encoding.newline = "\n"
	if bytes.Contains(data, encoding.encode("\r\n")) {
		encoding.newline = "\r\n"
	}
	return encoding, nil
}

func blockMarkers(id string) (begin, finish string) {
	return "# >>> dotfiles:" + id + " >>>", "# <<< dotfiles:" + id + " <<<"
}

func blockBounds(data []byte, id string) (start, end int, encoding textEncoding, err error) {
	if !blockIdentity.MatchString(id) {
		return 0, 0, encoding, errors.New("invalid profile block identity")
	}
	encoding, err = encodingOf(data)
	if err != nil {
		return 0, 0, encoding, err
	}
	begin, finish := blockMarkers(id)
	begins, finishes := bytes.Count(data, encoding.encode(begin)), bytes.Count(data, encoding.encode(finish))
	if begins == 0 && finishes == 0 {
		return -1, -1, encoding, nil
	}
	if begins != 1 || finishes != 1 {
		return 0, 0, encoding, errors.New("profile block markers are duplicated or incomplete")
	}
	opening := 0
	for _, bom := range [][]byte{{0xef, 0xbb, 0xbf}, {0xff, 0xfe}, {0xfe, 0xff}} {
		if bytes.HasPrefix(data, bom) {
			opening = len(bom)
		}
	}
	for _, nl := range []string{"\r\n", "\n"} {
		left, right := encoding.encode(nl+begin+nl), encoding.encode(finish+nl)
		start = bytes.Index(data, left)
		if start < 0 && bytes.HasPrefix(data[opening:], encoding.encode(begin+nl)) {
			start, left = opening, encoding.encode(begin+nl)
		}
		if start < 0 {
			continue
		}
		closing := bytes.Index(data[start+len(left):], right)
		if closing < 0 {
			continue
		}
		return start, start + len(left) + closing + len(right), encoding, nil
	}
	return 0, 0, encoding, errors.New("profile block markers were edited; preserve the file")
}

// Extract implements Slot.
func (b Block) Extract(data []byte, d Desired) ([]byte, bool, error) {
	start, end, encoding, err := blockBounds(data, d.Slot)
	if err != nil || start < 0 {
		return nil, false, err
	}
	text := strings.ReplaceAll(encoding.decode(data[start:end]), "\r\n", "\n")
	return []byte(strings.TrimPrefix(text, "\n")), true, nil
}

// Render implements Slot.
func (b Block) Render(d Desired) ([]byte, error) {
	script := string(d.Content)
	if !blockIdentity.MatchString(d.Slot) || !utf8.ValidString(script) || strings.ContainsAny(script, "\x00\r") || strings.Contains(script, "# >>> dotfiles:") || strings.Contains(script, "# <<< dotfiles:") {
		return nil, errors.New("invalid managed profile script")
	}
	begin, finish := blockMarkers(d.Slot)
	return []byte(begin + "\n" + strings.TrimRight(script, "\n") + "\n" + finish + "\n"), nil
}

// Replace implements Slot.
func (b Block) Replace(data []byte, d Desired, value []byte, present bool) ([]byte, error) {
	start, end, encoding, err := blockBounds(data, d.Slot)
	if err != nil {
		return nil, err
	}
	if !present {
		if start < 0 {
			return bytes.Clone(data), nil
		}
		return append(bytes.Clone(data[:start]), data[end:]...), nil
	}
	begin, finish := blockMarkers(d.Slot)
	text := string(value)
	if !strings.HasPrefix(text, begin+"\n") || !strings.HasSuffix(text, "\n"+finish+"\n") || strings.Contains(text, "\r") {
		return nil, errors.New("invalid managed profile block")
	}
	nl := encoding.newline
	if start >= 0 {
		nl = "\n"
		if bytes.Contains(data[start:end], encoding.encode("\r\n")) {
			nl = "\r\n"
		}
	}
	rendered := encoding.encode(nl + strings.ReplaceAll(text, "\n", nl))
	if start < 0 {
		start = len(data)
		if b.Top {
			start = 0
			for _, bom := range [][]byte{{0xef, 0xbb, 0xbf}, {0xff, 0xfe}, {0xfe, 0xff}} {
				if bytes.HasPrefix(data, bom) {
					start = len(bom)
				}
			}
		}
		end = start
	}
	return append(append(bytes.Clone(data[:start]), rendered...), data[end:]...), nil
}

// Empty implements Slot.
func (b Block) Empty(data []byte, _ Desired) (bool, error) {
	return len(data) == 0 || bytes.Equal(data, []byte{0xef, 0xbb, 0xbf}), nil
}
