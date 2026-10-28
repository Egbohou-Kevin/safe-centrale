package sysclock

import (
	"os"
	"runtime"
	"strings"
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

// What safehome-boot-clock.service writes: the time, less the uptime, is when the
// system started on the clock as it read then. Anything else says nothing: the hub
// then keeps the rule of the boot time alone.
func TestTheClockAtBootIsReadFromWhatTheUnitWrote(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"1761644280.123456789 12.50\n", time.Unix(1761644267, 623456789), true},
		{"1761644280 2", time.Unix(1761644278, 0), true},
		{"1761644280.5 0.5", time.Unix(1761644280, 0), true},
		{"", time.Time{}, false},
		{"1761644280.1", time.Time{}, false},
		{"1761644280.1 12.5 3", time.Time{}, false},
		{"abc 12.5", time.Time{}, false},
		{"0 12.5", time.Time{}, false},
		{"-5 12.5", time.Time{}, false},
		{"1761644280.1234567890 12.5", time.Time{}, false},
		{"1761644280.-5 12.5", time.Time{}, false},
		{"1761644280.1 0", time.Time{}, false},
		{"1761644280.1 NaN", time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := parseBootClock(c.in)
		if ok != c.ok || !got.Equal(c.want) {
			t.Errorf("%q: %v %v, want %v %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestTheClockAtBootIsUnknownWithoutTheFile(t *testing.T) {
	if _, err := os.Stat(BootClockPath); err == nil {
		t.Skip(BootClockPath + " exists here")
	}
	if at, ok := StartShown(); ok {
		t.Fatalf("no file, yet %v", at)
	}
}

// The unit that writes the clock at boot writes where StartShown reads.
func TestTheUnitWritesWhereTheHubReads(t *testing.T) {
	b, err := os.ReadFile("../../../deploy/system/safehome-boot-clock.service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), ">"+BootClockPath+".new && mv "+BootClockPath+".new "+BootClockPath+"'") {
		t.Fatalf("safehome-boot-clock.service does not write %s", BootClockPath)
	}
}
