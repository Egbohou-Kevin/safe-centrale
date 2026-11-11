package contract

import (
	"errors"
	"testing"
)

func TestReplayRequestsAreBounded(t *testing.T) {
	good := []ReplayData{{1, 1}, {5, 9}, {100, 100 + MaxReplay - 1}}
	for _, d := range good {
		if err := d.Validate(); err != nil {
			t.Errorf("%+v: %v", d, err)
		}
	}
	bad := []ReplayData{{0, 3}, {5, 4}, {1, MaxReplay + 1}}
	for _, d := range bad {
		if err := d.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v accepted: %v", d, err)
		}
	}
}

func TestReplayReportsStayInsideTheirRange(t *testing.T) {
	ok := ReplayedData{From: 10, To: 20, Last: 50, Resent: 9, Missing: []SeqRange{{10, 10}, {15, 16}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []ReplayedData{
		{From: 10, To: 20, Resent: -1},
		{From: 10, To: 20, Missing: []SeqRange{{9, 10}}},
		{From: 10, To: 20, Missing: []SeqRange{{12, 11}}},
		{From: 10, To: 20, Missing: []SeqRange{{19, 21}}},
		{From: 20, To: 10},
	} {
		if err := d.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v accepted: %v", d, err)
		}
	}
}
