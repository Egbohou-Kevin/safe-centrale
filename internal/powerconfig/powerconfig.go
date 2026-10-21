// Package powerconfig reads the battery module service's configuration file (JSON),
// strictly: a misspelt key is an error. See config/power.example.json and
// docs/alimentation.md.
package powerconfig

import (
	"cmp"
	"errors"
	"fmt"
	"time"

	"safehome/internal/configfile"
	"safehome/internal/power"
	"safehome/internal/powerapp"
)

// Board says where the module is: the X728's lines and bus.
type Board struct {
	GPIOChip  string
	MainsLine int
	CutLine   int
	CutPulse  time.Duration
	I2CBus    string
}

// Result is what the file describes.
type Result struct {
	configfile.Program
	App   powerapp.Config
	Board Board
}

type file struct {
	configfile.ProgramFile
	DeviceID      string `json:"device_id"`
	GPIOChip      string `json:"gpio_chip"`
	MainsLine     *int   `json:"mains_line"`
	CutLine       *int   `json:"cut_line"`
	I2CBus        string `json:"i2c_bus"`
	CutMilliVolts int    `json:"cut_millivolts"`
}

// Load reads the configuration at path.
func Load(path string) (Result, error) {
	var f file
	if err := configfile.Decode(path, &f); err != nil {
		return Result{}, fmt.Errorf("powerconfig: %w", err)
	}
	r, err := build(f)
	if err != nil {
		return Result{}, fmt.Errorf("powerconfig: %s: %w", path, err)
	}
	return r, nil
}

func build(f file) (Result, error) {
	p, err := f.Build("safehome-power-", "the counter of its messages")
	if err != nil {
		return Result{}, err
	}
	b := Board{
		GPIOChip: cmp.Or(f.GPIOChip, "pinctrl-bcm2711"), MainsLine: or(f.MainsLine, 6), CutLine: or(f.CutLine, 26),
		CutPulse: 3 * time.Second, I2CBus: cmp.Or(f.I2CBus, "/dev/i2c-1"),
	}
	if b.MainsLine < 0 || b.MainsLine > 27 || b.CutLine < 0 || b.CutLine > 27 || b.MainsLine == b.CutLine {
		return Result{}, errors.New("mains_line and cut_line: two different lines of the Pi's connector, 0 to 27")
	}
	pw := power.DefaultConfig()
	pw.CutMilliVolts = cmp.Or(f.CutMilliVolts, pw.CutMilliVolts)
	r := Result{Program: p, Board: b, App: powerapp.DefaultConfig(f.Site, f.DeviceID, pw)}
	if err := r.App.Validate(); err != nil {
		return Result{}, err
	}
	return r, nil
}

func or(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}
