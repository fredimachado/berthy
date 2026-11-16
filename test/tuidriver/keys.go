//go:build unix

package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// named holds the bytes a terminal sends for the named keys.
var named = map[string]string{
	"enter":     "\r",
	"return":    "\r",
	"esc":       "\x1b",
	"escape":    "\x1b",
	"tab":       "\t",
	"shift+tab": "\x1b[Z",
	"backtab":   "\x1b[Z",
	"up":        "\x1b[A",
	"down":      "\x1b[B",
	"right":     "\x1b[C",
	"left":      "\x1b[D",
	"home":      "\x1b[H",
	"end":       "\x1b[F",
	"pgup":      "\x1b[5~",
	"pageup":    "\x1b[5~",
	"pgdown":    "\x1b[6~",
	"pagedown":  "\x1b[6~",
	"delete":    "\x1b[3~",
	"insert":    "\x1b[2~",
	"backspace": "\x7f",
	"space":     " ",
	"f1":        "\x1bOP",
	"f2":        "\x1bOQ",
	"f3":        "\x1bOR",
	"f4":        "\x1bOS",
	"f5":        "\x1b[15~",
	"f6":        "\x1b[17~",
	"f7":        "\x1b[18~",
	"f8":        "\x1b[19~",
	"f9":        "\x1b[20~",
	"f10":       "\x1b[21~",
}

// keyBytes is what a terminal sends for a key: a name such as "enter" or "ctrl+c", "alt+x", or one character.
func keyBytes(name string) ([]byte, error) {
	if s, ok := named[strings.ToLower(name)]; ok {
		return []byte(s), nil
	}
	lower := strings.ToLower(name)
	switch {
	case strings.HasPrefix(lower, "ctrl+") && len(lower) == len("ctrl+")+1:
		c := lower[len("ctrl+")]
		switch {
		case c >= 'a' && c <= 'z':
			return []byte{c - 'a' + 1}, nil
		case c == '[':
			return []byte{0x1b}, nil
		case c == '\\':
			return []byte{0x1c}, nil
		case c == ']':
			return []byte{0x1d}, nil
		case c == ' ' || c == '@':
			return []byte{0}, nil
		}
	case strings.HasPrefix(lower, "alt+") && utf8.RuneCountInString(name) == len("alt+")+1:
		return append([]byte{0x1b}, name[len("alt+"):]...), nil
	}
	if utf8.RuneCountInString(name) == 1 {
		return []byte(name), nil
	}
	return nil, fmt.Errorf("unknown key %q", name)
}
