package session

import (
	"os"
	"strconv"
	"strings"
)

func procPath(pid int, name string) string {
	return "/proc/" + strconv.Itoa(pid) + "/" + name
}

// foreground returns the leader of the foreground process group of the
// terminal that pid's process controls, the program in front, or pid if
// there's none, or 0 if pid is gone.
func foreground(pid int) int {
	b, err := os.ReadFile(procPath(pid, "stat"))
	if err != nil {
		return 0
	}
	// The command, in parentheses, may hold anything; the fields after
	// it are state, ppid, pgrp, session, tty_nr, and tpgid.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 6 {
		return 0
	}
	if tpgid, err := strconv.Atoi(f[5]); err == nil && tpgid > 0 {
		if _, err := os.Stat(procPath(tpgid, "stat")); err == nil {
			return tpgid
		}
	}
	return pid
}

// cmdline returns process pid's arguments.
func cmdline(pid int) []string {
	b, err := os.ReadFile(procPath(pid, "cmdline"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
}
