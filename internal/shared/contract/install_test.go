package contract

import (
	"errors"
	"testing"
	"time"
)

func TestInstallModeValidation(t *testing.T) {
	until := TSPtr(time.Date(2026, 10, 20, 15, 30, 0, 0, time.UTC))
	good := []InstallModeData{
		{Status: InstallOpen, Until: until},
		{Status: InstallClosed, Reason: InstallTimeout},
		{Status: InstallClosed, Reason: InstallArmed},
		{Status: InstallClosed, Reason: InstallStopped},
		{Status: InstallClosed, Reason: InstallRestart},
	}
	for _, d := range good {
		if err := d.Validate(); err != nil {
			t.Errorf("%+v: %v", d, err)
		}
	}
	bad := []InstallModeData{
		{Status: "ajar"},
		{Status: InstallOpen},
		{Status: InstallOpen, Until: until, Reason: InstallTimeout},
		{Status: InstallClosed},
		{Status: InstallClosed, Reason: "bored"},
		{Status: InstallClosed, Reason: InstallTimeout, Until: until},
	}
	for _, d := range bad {
		if err := d.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: got %v, want ErrInvalid", d, err)
		}
	}
}
