//go:build !linux

package sysclock

import "time"

func read() (bool, bool) { return false, false }

func uptime() (time.Duration, bool) { return 0, false }
