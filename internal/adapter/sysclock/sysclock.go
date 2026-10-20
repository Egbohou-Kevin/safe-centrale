// Package sysclock tells whether the system clock is synchronised. A Raspberry Pi
// has no clock that survives a power cut: until NTP sets it, the time is wrong, and
// so are the times in the messages. It uses the standard library only.
package sysclock

// Synced says whether the kernel considers its clock synchronised (NTP keeps it so,
// through systemd-timesyncd). known is false where it cannot tell (outside Linux).
//
// The kernel stops calling the clock synchronised when NTP has not corrected it for
// some hours: after a long cut of internet, false means "not checked lately", the
// time is then probably right but no longer vouched for.
func Synced() (synced, known bool) { return read() }
