//go:build !linux

package session

// Auto-naming needs a process's foreground group, arguments, and working
// directory, which only Linux's /proc gives here. TODO: macOS, with
// libproc or sysctl.

func procPath(pid int, name string) string { return "" }

func foreground(pid int) int { return 0 }

func cmdline(pid int) []string { return nil }
