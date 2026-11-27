//go:build darwin

package audit

import "golang.org/x/sys/unix"

// ancestry names the process pid and up to n-1 of its parents, the
// closest first, with one sysctl each.
func ancestry(pid, n int) []string {
	var names []string
	for len(names) < n && pid > 0 {
		kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			break
		}
		names = append(names, cString(kp.Proc.P_comm[:]))
		if pid == 1 {
			break
		}
		pid = int(kp.Eproc.Ppid)
	}
	return names
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
