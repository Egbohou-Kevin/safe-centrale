// Package x728 drives the Geekworm X728, the hub's battery module (docs/alimentation.md),
// through Linux's GPIO character device and the I2C bus, with the standard library
// only:
//
//   - its mains line, an input that reads high while the module runs on its battery;
//   - its fuel gauge on the I2C bus, at 0x36, which gives the cell's voltage and the
//     charge left;
//   - its power button line, an output: held high a few seconds, it makes the module
//     shut the Pi down and cut the power (the Pi must let it, see docs/alimentation.md).
//
// The lines are numbered as on the Pi's connector (BCM). They are those of the
// maker's scripts for the board revisions 2.1 to 2.3; the power button line is 13 on
// older boards.
package x728

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Options says where the module is.
type Options struct {
	GPIOChip  string        // the label of the Pi's GPIO chip: "pinctrl-bcm2711" on a Pi 4
	MainsLine int           // 6
	CutLine   int           // the power button line: 26, or 13 on boards before 2.1
	CutPulse  time.Duration // how long the button is held: 3 s, as the maker's script
	I2CBus    string        // "/dev/i2c-1"
}

// gaugeAddress is the fuel gauge's address on the bus; the board fixes it.
const gaugeAddress = 0x36

// The gauge's registers, each a big-endian word.
const (
	regVoltage = 0x02
	regCharge  = 0x04
)

// Board is an open module.
type Board struct {
	opts  Options
	mains line
	cut   line
	gauge smbus

	mu sync.Mutex // one pulse at a time
}

// line is one GPIO line held by the program.
type line interface {
	get() (bool, error)
	set(high bool) error
	close() error
}

// smbus is a device on an I2C bus.
type smbus interface {
	readWord(reg byte) (uint16, error)
	close() error
}

// Open takes the module's lines and its gauge. The power button line is held low,
// its rest position.
func Open(o Options) (*Board, error) {
	if o.GPIOChip == "" || o.I2CBus == "" || o.MainsLine < 0 || o.CutLine < 0 || o.MainsLine == o.CutLine || o.CutPulse <= 0 {
		return nil, errors.New("x728: GPIOChip, I2CBus, two different lines and CutPulse are required")
	}
	b := &Board{opts: o}
	var err error
	if b.mains, err = openLine(o.GPIOChip, o.MainsLine, false); err != nil {
		return nil, fmt.Errorf("x728: mains line %d: %w", o.MainsLine, err)
	}
	if b.cut, err = openLine(o.GPIOChip, o.CutLine, true); err != nil {
		b.mains.close()
		return nil, fmt.Errorf("x728: power button line %d: %w", o.CutLine, err)
	}
	if b.gauge, err = openSMBus(o.I2CBus, gaugeAddress); err != nil {
		b.mains.close()
		b.cut.close()
		return nil, fmt.Errorf("x728: fuel gauge on %s: %w", o.I2CBus, err)
	}
	return b, nil
}

// Close lets the lines and the bus go.
func (b *Board) Close() error {
	return errors.Join(b.mains.close(), b.cut.close(), b.gauge.close())
}

// MainsLost reads the mains line: true while the module runs on its battery.
func (b *Board) MainsLost(context.Context) (bool, error) {
	high, err := b.mains.get()
	if err != nil {
		return false, fmt.Errorf("x728: mains line: %w", err)
	}
	return high, nil
}

// Gauge reads the cell's voltage and the charge left.
func (b *Board) Gauge(context.Context) (milliVolts, percent int, err error) {
	v, err := b.gauge.readWord(regVoltage)
	if err != nil {
		return 0, 0, fmt.Errorf("x728: fuel gauge: %w", err)
	}
	c, err := b.gauge.readWord(regCharge)
	if err != nil {
		return 0, 0, fmt.Errorf("x728: fuel gauge: %w", err)
	}
	return milliVoltsOf(swap(v)), percentOf(swap(c)), nil
}

// Cut holds the power button for CutPulse, then lets it go: the module shuts the Pi
// down and cuts the power. ctx ending lets the button go at once.
func (b *Board) Cut(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.cut.set(true); err != nil {
		return fmt.Errorf("x728: power button line: %w", err)
	}
	t := time.NewTimer(b.opts.CutPulse)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
	// Let it go, or the module would need two presses to power on again.
	if err := b.cut.set(false); err != nil {
		return fmt.Errorf("x728: power button line: %w", err)
	}
	return ctx.Err()
}

// swap turns an SMBus word, low byte first, into the gauge's big-endian register.
func swap(w uint16) uint16 { return w<<8 | w>>8 }

// milliVoltsOf converts the voltage register: 1.25 mV per unit of its upper 12 bits.
func milliVoltsOf(reg uint16) int { return int(reg) * 5 / 64 }

// percentOf converts the charge register: whole points in its high byte.
func percentOf(reg uint16) int { return min(int(reg>>8), 100) }
