//go:build !darwin && !linux

package audit

// ancestry is not known cheaply on this OS.
func ancestry(int, int) []string { return nil }
