// Package sysclock tells whether the system clock is synchronised, how long the
// system has been up, and when it started on the clock as it read at boot. A
// Raspberry Pi has no clock that survives a power cut: until NTP sets it, the time
// is wrong, and so are the times in the messages. It uses the standard library only.
package sysclock

import (
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Synced says whether the kernel considers its clock synchronised (NTP keeps it so,
// through systemd-timesyncd). known is false where it cannot tell (outside Linux).
//
// The kernel stops calling the clock synchronised when NTP has not corrected it for
// some hours: after a long cut of internet, false means "not checked lately", the
// time is then probably right but no longer vouched for.
func Synced() (synced, known bool) { return read() }

// Uptime says how long the system has been up (/proc/uptime): now less it is when the
// system started, on the clock as it is now. ok is false where it cannot tell (outside
// Linux, the file unreadable or not a positive number of seconds).
func Uptime() (d time.Duration, ok bool) { return uptime() }

// maxUptime bounds what parseUptime takes: a century, far below what a Duration holds.
const maxUptime = 100 * 365 * 24 * time.Hour

// parseUptime reads the first field of /proc/uptime, the seconds since the system
// started.
func parseUptime(s string) (time.Duration, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, false
	}
	sec, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(sec) || sec <= 0 || sec > maxUptime.Seconds() {
		return 0, false
	}
	return time.Duration(sec * float64(time.Second)), true
}

// BootClockPath is where safehome-boot-clock.service (deploy/system) writes, early at
// boot and before NTP can set the clock, the time and the uptime as they read then.
const BootClockPath = "/run/safehome-boot-clock"

// StartShown says when the system started on the clock as it read early at boot,
// before NTP could set it: the time written in BootClockPath less the uptime written
// with it. NTP setting the clock forward since, it is before now less Uptime. ok is
// false where it cannot tell (the file missing, as outside the hub, or not a positive
// time and a positive uptime).
func StartShown() (time.Time, bool) {
	b, err := os.ReadFile(BootClockPath)
	if err != nil {
		return time.Time{}, false
	}
	return parseBootClock(string(b))
}

// parseBootClock reads "<seconds since 1970>.<fraction> <uptime in seconds>", what
// date +%s.%N and /proc/uptime say, and returns the first less the second.
func parseBootClock(s string) (time.Time, bool) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return time.Time{}, false
	}
	whole, frac, _ := strings.Cut(fields[0], ".")
	sec, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || sec <= 0 || len(frac) > 9 {
		return time.Time{}, false
	}
	var nsec int64
	if frac != "" {
		n, err := strconv.ParseUint(frac+strings.Repeat("0", 9-len(frac)), 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		nsec = int64(n)
	}
	up, ok := parseUptime(fields[1])
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(sec, nsec).Add(-up), true
}
