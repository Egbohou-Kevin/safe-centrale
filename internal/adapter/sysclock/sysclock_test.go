package sysclock

import (
	"runtime"
	"testing"
)

func TestSyncedAnswersOnLinuxOnly(t *testing.T) {
	_, known := Synced()
	if known != (runtime.GOOS == "linux") {
		t.Fatalf("known %v on %s", known, runtime.GOOS)
	}
}
