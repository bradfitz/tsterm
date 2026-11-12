package wsclient

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// A Key is a key pressed, to encode for the program in a session.
type Key struct {
	// Name is a special key's name, as the web's KeyboardEvent.key names
	// it ("Enter", "ArrowUp", "F5", and so on), or empty for a character.
	Name string

	// Rune is the character typed, if Name is empty.
	Rune rune

	Shift, Alt, Ctrl bool
}

// KeyBytes returns what the key k sends to a program, as xterm sends it,
// or "" for keys that send nothing. appCursor is the program's application
// cursor keys mode (vt.Modes.ApplicationCursor).
func KeyBytes(k Key, appCursor bool) string {
	mod := 0
	if k.Shift {
		mod |= 1
	}
	if k.Alt {
		mod |= 2
	}
	if k.Ctrl {
		mod |= 4
	}
	cursor := func(final byte) string {
		switch {
		case mod != 0:
			return fmt.Sprintf("\x1b[1;%d%c", mod+1, final)
		case appCursor:
			return "\x1bO" + string(final)
		}
		return "\x1b[" + string(final)
	}
	tilde := func(n int) string {
		if mod != 0 {
			return fmt.Sprintf("\x1b[%d;%d~", n, mod+1)
		}
		return fmt.Sprintf("\x1b[%d~", n)
	}
	fkey := func(final byte) string {
		if mod != 0 {
			return fmt.Sprintf("\x1b[1;%d%c", mod+1, final)
		}
		return "\x1bO" + string(final)
	}
	switch k.Name {
	case "":
	case "Enter":
		if k.Alt {
			return "\x1b\r"
		}
		return "\r"
	case "Backspace":
		switch {
		case k.Ctrl:
			return "\x08"
		case k.Alt:
			return "\x1b\x7f"
		}
		return "\x7f"
	case "Tab":
		if k.Shift {
			return "\x1b[Z"
		}
		return "\t"
	case "Escape":
		return "\x1b"
	case "ArrowUp":
		return cursor('A')
	case "ArrowDown":
		return cursor('B')
	case "ArrowRight":
		return cursor('C')
	case "ArrowLeft":
		return cursor('D')
	case "Home":
		return cursor('H')
	case "End":
		return cursor('F')
	case "Insert":
		return tilde(2)
	case "Delete":
		return tilde(3)
	case "PageUp":
		return tilde(5)
	case "PageDown":
		return tilde(6)
	case "F1":
		return fkey('P')
	case "F2":
		return fkey('Q')
	case "F3":
		return fkey('R')
	case "F4":
		return fkey('S')
	default:
		if n, ok := map[string]int{"F5": 15, "F6": 17, "F7": 18, "F8": 19, "F9": 20, "F10": 21, "F11": 23, "F12": 24}[k.Name]; ok {
			return tilde(n)
		}
		return ""
	}
	r := k.Rune
	if r == 0 || r == utf8.RuneError {
		return ""
	}
	if k.Ctrl && !k.Alt {
		c := r
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c >= 0x40 && c <= 0x5f {
			return string(c - 0x40) // Ctrl-A is 1, and so on
		}
		special := map[rune]string{' ': "\x00", '2': "\x00", '3': "\x1b", '4': "\x1c", '5': "\x1d", '6': "\x1e", '7': "\x1f", '8': "\x7f", '/': "\x1f", '?': "\x7f"}
		return special[r]
	}
	if k.Alt && r < 0x80 {
		// Alt plus an ASCII key is ESC-prefixed.
		return "\x1b" + string(r)
	}
	return string(r)
}

// Paste returns what pasting text sends to a program: with Enter for each
// line break, and marked, if the program asked (vt.Modes.BracketedPaste).
func Paste(text string, bracketed bool) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\r"), "\n", "\r")
	if bracketed {
		// The program could be tricked by a paste that ends the paste.
		text = strings.ReplaceAll(text, "\x1b[201~", "")
		return "\x1b[200~" + text + "\x1b[201~"
	}
	return text
}

// Mouse buttons, as mouse reports number them.
const (
	MouseLeft      = 0
	MouseMiddle    = 1
	MouseRight     = 2
	MouseWheelUp   = 64
	MouseWheelDown = 65
	MouseMotion    = 32 // added to a button for motion
)

// MouseReport encodes a mouse event for a program that asked for them
// (vt.Modes.Mouse): button, as above, at the 0-based cell x, y, pressed or
// released, with modifiers, in SGR encoding if sgr (vt.Modes.MouseSGR),
// else X10's.
func MouseReport(button, x, y int, release, shift, alt, ctrl, sgr bool) string {
	b := button
	if shift {
		b |= 4
	}
	if alt {
		b |= 8
	}
	if ctrl {
		b |= 16
	}
	if sgr {
		final := 'M'
		if release {
			final = 'm'
		}
		return fmt.Sprintf("\x1b[<%d;%d;%d%c", b, x+1, y+1, final)
	}
	// The X10 encoding can't say which button was released, and can't
	// address past column or row 223.
	if release {
		b = 3 | b&^3
	}
	return "\x1b[M" + string([]byte{byte(32 + b), byte(32 + min(x+1, 223)), byte(32 + min(y+1, 223))})
}
