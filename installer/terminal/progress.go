package terminal

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/term"
)

const progressIndent = "    "

type progressView struct {
	mutex   sync.Mutex
	output  *os.File
	restore func() error
	pending []byte
	open    bool
	err     error
	plain   bool
	held    string
	clixml  bool
	skip    bool
}

func (v *progressView) Step(text string) {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	v.finish()
	v.clixml, v.skip = false, false
	if v.plain {
		v.write("==> " + plain(text) + "\n")
		return
	}
	v.write("\x1b[1m==> " + plain(text) + "\x1b[0m\n")
}

func (v *progressView) Write(data []byte) (int, error) {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	for _, b := range data {
		switch b {
		case '\n':
			v.emit(true)
		case '\r':
			v.emit(false)
		default:
			v.pending = append(v.pending, b)
			if len(v.pending) >= 4096 {
				cut := len(v.pending) - 1
				for cut > 0 && !utf8.RuneStart(v.pending[cut]) {
					cut--
				}
				rest := append([]byte(nil), v.pending[cut:]...)
				v.pending = v.pending[:cut]
				v.emit(true)
				v.pending = append(v.pending, rest...)
			}
		}
	}
	return len(data), nil
}

func (v *progressView) Close() error {
	v.mutex.Lock()
	defer v.mutex.Unlock()
	v.finish()
	var err error
	if v.restore != nil {
		err = v.restore()
	}
	return errors.Join(v.err, err)
}

func (v *progressView) finish() {
	if len(v.pending) > 0 || v.open || v.held != "" {
		v.emit(true)
	}
}

func (v *progressView) emit(final bool) {
	text := cleanOutput(string(v.pending))
	v.pending = v.pending[:0]
	if v.hidden(text) {
		if final && v.open {
			v.write("\n")
			v.open = false
		}
		v.skip = !final
		return
	}
	skip := v.skip
	v.skip = false
	if skip && final && text == "" {
		return
	}
	if v.plain {
		if text == "" {
			text = v.held
		}
		v.held = ""
		if final {
			v.write(strings.TrimRight(progressIndent+text, " ") + "\n")
		} else {
			v.held = text
		}
		return
	}
	rewind := ""
	if v.open {
		rewind = "\r\x1b[K"
	}
	if !final {
		if text != "" {
			if width, _, err := term.GetSize(int(v.output.Fd())); err == nil {
				text = fit(text, width-1-len(progressIndent))
			}
			v.write(rewind + progressIndent + text)
			v.open = true
		}
		return
	}
	if text != "" {
		v.write(rewind + progressIndent + text + "\n")
	} else {
		v.write("\n")
	}
	v.open = false
}

func (v *progressView) hidden(text string) bool {
	trimmed := strings.TrimSpace(text)
	switch {
	case v.clixml:
	case trimmed == "#< CLIXML":
		return true
	case strings.HasPrefix(trimmed, "<Objs Version="):
	default:
		return false
	}
	v.clixml = !strings.HasSuffix(trimmed, "</Objs>")
	return true
}

func (v *progressView) write(text string) {
	if v.err == nil {
		_, v.err = io.WriteString(v.output, text)
	}
}

func cleanOutput(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		i += size
		switch {
		case r == 0x1b:
			i = skipEscape(text, i)
		case r == '\t' || r >= 32 && r != 127 && (r < 0x80 || r >= 0xa0):
			b.WriteRune(r)
		}
	}
	return b.String()
}

func skipEscape(text string, i int) int {
	if i >= len(text) {
		return i
	}
	switch text[i] {
	case '[':
		for i++; i < len(text); i++ {
			if text[i] >= 0x40 && text[i] <= 0x7e {
				return i + 1
			}
		}
	case ']', 'P', 'X', '^', '_':
		for i++; i < len(text); i++ {
			if text[i] == 0x07 {
				return i + 1
			}
			if text[i] == 0x1b && i+1 < len(text) && text[i+1] == '\\' {
				return i + 2
			}
		}
	default:
		for i < len(text) && text[i] >= 0x20 && text[i] <= 0x2f {
			i++
		}
		if i < len(text) {
			i++
		}
	}
	return i
}
