package shim

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// setProcName sets the process's name, as ps and top show it: a shim runs
// as /proc/PID/exe, which would otherwise make it "exe".
func setProcName(name string) {
	b := append([]byte(name), 0)
	unix.Prctl(unix.PR_SET_NAME, uintptr(unsafe.Pointer(&b[0])), 0, 0, 0)
}
