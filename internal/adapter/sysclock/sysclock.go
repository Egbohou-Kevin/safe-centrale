// Package sysclock tells whether the system clock is synchronised, and how long the
// system has been up. A Raspberry Pi has no clock that survives a power cut: until NTP
// sets it, the time is wrong, and so are the times in the messages. It uses the
// standard library only.
package sysclock

import (
	"math"
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
