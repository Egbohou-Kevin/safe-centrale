package contract

import "fmt"

// Installation mode (see docs/security-install.md). The installer opens it on site,
// with the installer code at the keypad, while the alarm is disarmed. The hub then
// says so on install/mode, retained; the local console turns on its own Wi-Fi and its
// web page for as long as the mode lasts, 30 minutes at most. The mode ends when the
// time is up, when the alarm leaves the disarmed state, when the console closes it
// (install.close on install/cmd, never from the cloud), or when the hub restarts.

// InstallLength is how long the installation mode lasts.
const InstallLength = 30 * 60 // seconds

// Statuses of install.mode.
const (
	InstallOpen   = "open"
	InstallClosed = "closed"
)

// Why the installation mode closed.
const (
	InstallTimeout = "timeout" // the 30 minutes are over
	InstallArmed   = "armed"   // the alarm left the disarmed state
	InstallStopped = "stopped" // the console closed it
	InstallRestart = "restart" // the hub started: a mode never survives a restart
)

// InstallModeData: install.mode, from the hub on install/mode, retained.
type InstallModeData struct {
	Status string     `json:"status"`
	Until  *Timestamp `json:"until,omitempty"`  // when open: when it closes at the latest
	Reason string     `json:"reason,omitempty"` // when closed
}

// Validate checks the message.
func (d InstallModeData) Validate() error {
	switch d.Status {
	case InstallOpen:
		if d.Until == nil || d.Reason != "" {
			return fmt.Errorf("%w: an open installation mode has an end and no reason", ErrInvalid)
		}
	case InstallClosed:
		switch d.Reason {
		case InstallTimeout, InstallArmed, InstallStopped, InstallRestart:
		default:
			return fmt.Errorf("%w: unknown reason %q", ErrInvalid, d.Reason)
		}
		if d.Until != nil {
			return fmt.Errorf("%w: a closed installation mode has no end", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown status %q", ErrInvalid, d.Status)
	}
	return nil
}

// InstallCloseData: install.close, from the local console on install/cmd: the
// installer is done. It carries nothing.
type InstallCloseData struct{}
