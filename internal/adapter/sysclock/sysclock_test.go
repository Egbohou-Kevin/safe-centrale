package sysclock

import (
	"runtime"
	"testing"
	"time"
)

func TestSyncedAnswersOnLinuxOnly(t *testing.T) {
	_, known := Synced()
	if known != (runtime.GOOS == "linux") {
		t.Fatalf("known %v on %s", known, runtime.GOOS)
	}
}

func TestUptimeAnswersOnLinuxOnly(t *testing.T) {
	d, known := Uptime()
	if known != (runtime.GOOS == "linux") || known && d <= 0 {
		t.Fatalf("uptime %v, known %v on %s", d, known, runtime.GOOS)
	}
}

// A file that does not say a positive number of seconds says nothing: the hub then
// keeps the rule without a boot time.
func TestUptimeIsReadFromProc(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"350735.47 234388.90\n", 350735470 * time.Millisecond, true},
		{"12.5 3.1", 12500 * time.Millisecond, true},
		{"", 0, false},
		{"abc 1.0", 0, false},
		{"0.00 0.00", 0, false},
		{"-5 1", 0, false},
		{"NaN 1", 0, false},
		{"+Inf 1", 0, false},
		{"1e300 1", 0, false},
	}
	for _, c := range cases {
		got, ok := parseUptime(c.in)
		if ok != c.ok || ok && (got-c.want).Abs() > time.Millisecond {
			t.Errorf("%q: %v %v, want %v %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
