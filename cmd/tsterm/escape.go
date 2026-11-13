package main

// escapeByte begins a client command, as in Mosh. It is Ctrl-^, which many
// terminals also send for Ctrl-~. After it:
//
//	.        disconnect, leaving the session running
//
// Any other key sends Ctrl-^ followed by that key, for tstermd, which has
// commands of its own after Ctrl-^ (see package udp): " opens the session
// menu, and Ctrl-^ types a literal Ctrl-^.
const escapeByte = 0x1e

// escaper scans typed input for client commands. The zero value is ready.
type escaper struct {
	pending bool // the previous byte was escapeByte
}

// filter returns the input to send to the session, and whether the user
// asked to disconnect, in which case any input after the command is dropped.
func (e *escaper) filter(in []byte) (out []byte, disconnect bool) {
	for _, b := range in {
		if !e.pending {
			if b == escapeByte {
				e.pending = true
			} else {
				out = append(out, b)
			}
			continue
		}
		e.pending = false
		switch b {
		case '.':
			return out, true
		default:
			out = append(out, escapeByte, b)
		}
	}
	return out, false
}
