//go:build !linux

package sysclock

func read() (bool, bool) { return false, false }
