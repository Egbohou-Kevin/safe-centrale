package power

import "time"

// Input is what the core is told. Every input carries the time it happened.
type Input interface{ at() time.Time }

// Line is a reading of the module's mains line.
type Line struct {
	Now  time.Time
	Lost bool // the module runs on its battery
}

// Gauge is a reading of the module's fuel gauge.
type Gauge struct {
	Now        time.Time
	MilliVolts int // the cell's voltage
	Percent    int // the charge left, as the gauge estimates it
}

// Tick says time has passed.
type Tick struct{ Now time.Time }

func (i Line) at() time.Time  { return i.Now }
func (i Gauge) at() time.Time { return i.Now }
func (i Tick) at() time.Time  { return i.Now }

// Action is what the core asks the application to do.
type Action interface{ isAction() }

// Mains: tell the site the supply changed (or what it is, at start).
type Mains struct{ Lost bool }

// Battery: tell the site the charge left.
type Battery struct{ Percent int }

// Cut: ask the module to shut the hub down cleanly and cut its power, before the
// cell is empty. The module powers the hub on again when the mains come back.
type Cut struct{ MilliVolts int }

func (Mains) isAction()   {}
func (Battery) isAction() {}
func (Cut) isAction()     {}
