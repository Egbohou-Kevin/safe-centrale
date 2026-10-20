package sysclock

import "testing"

func TestTheKernelsAnswerIsRead(t *testing.T) {
	cases := []struct {
		state  int
		status int64
		want   bool
	}{
		{0, 0, true}, // TIME_OK
		{1, 0, true}, // a leap second is announced
		{timeError, staUnsync, false},
		{0, staUnsync, false}, // flagged even if the state looks fine
		{timeError, 0, false},
	}
	for _, c := range cases {
		if got := fromTimex(c.state, c.status); got != c.want {
			t.Errorf("state %d status %#x: %v", c.state, c.status, got)
		}
	}
}
