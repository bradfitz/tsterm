//go:build windows

package pty

import (
	"errors"
	"runtime"
)

// errUnsupported is returned by Start and Resize on platforms where the
// PTY backend is not yet implemented. tsterm's first iteration is Unix
// only; Windows support would land via creack/pty's ConPTY backend later.
var errUnsupported = errors.New("pty: not supported on " + runtime.GOOS)

func start(cfg Config) (*PTY, error) { return nil, errUnsupported }

func (p *PTY) Resize(rows, cols int) error { return errUnsupported }
