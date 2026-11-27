//go:build linux

package audit

import (
	"os"
	"strconv"
	"strings"
)

// ancestry names the process pid and up to n-1 of its parents, the
// closest first, from /proc.
func ancestry(pid, n int) []string {
	var names []string
	for len(names) < n && pid > 0 {
		comm, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
		if err != nil {
			break
		}
		names = append(names, strings.TrimSpace(string(comm)))
		if pid == 1 {
			break
		}
		pid = linuxPPID(pid)
	}
	return names
}

// linuxPPID reads the parent of pid from /proc/<pid>/stat; 0 if unknown.
func linuxPPID(pid int) int {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// "pid (comm) state ppid …": comm may hold spaces and parentheses.
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(fields[1])
	return ppid
}
