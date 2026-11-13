// Package zones is the rule of the kinds of zone: which sensors a zone of each kind
// reacts to. The hub's engine applies it to the alarms, the installer's tools to the
// devices they put in a zone (a device is never put in a zone it cannot trigger):
// both programs use it, so it lies in shared, once (docs/rangement-par-programme.md,
// 13/11/2026). It depends on nothing, like the engine.
package zones

import (
	"maps"
	"slices"
)

// Kind is the type of a zone: it decides how its devices behave.
type Kind string

const (
	Entry    Kind = "entry"      // entry door: entry delay
	Instant  Kind = "instant"    // perimeter: immediate alarm
	Interior Kind = "interior"   // interior motion
	Fire     Kind = "24h_fire"   // smoke, always active
	Panic    Kind = "24h_panic"  // panic button, always active
	Tamper   Kind = "24h_tamper" // tamper, always active
)

// Sensor is what a device senses. The values are spelled like the contract's
// capabilities, which the installer's tools rely on (a test of the engine's wire
// subpackage keeps it so).
type Sensor string

const (
	Contact    Sensor = "contact"     // door or window contact; active when open
	Motion     Sensor = "motion"      // active on motion
	Smoke      Sensor = "smoke"       // active on smoke
	Vibration  Sensor = "vibration"   // active on vibration
	GlassBreak Sensor = "glass_break" // active on glass break
	PanicPress Sensor = "panic"       // active when the button is pressed
	Tampered   Sensor = "tamper"      // active when the device is tampered with
)

// sensors lists, for each kind of zone, the sensors it reacts to: a smoke detector's
// tamper is not a fire. The tamper sensor of any device is apart: it always means
// tamper, whatever the zone (see the engine's signal).
var sensors = map[Kind][]Sensor{
	Entry:    {Contact, Motion, Vibration, GlassBreak},
	Instant:  {Contact, Motion, Vibration, GlassBreak},
	Interior: {Motion, Contact},
	Fire:     {Smoke},
	Panic:    {PanicPress},
	Tamper:   {Tampered},
}

// Valid tells whether k is a kind of zone.
func (k Kind) Valid() bool {
	_, ok := sensors[k]
	return ok
}

// Sensors returns the sensors a zone of this kind reacts to; nil for an unknown kind.
func (k Kind) Sensors() []Sensor { return slices.Clone(sensors[k]) }

// Accepts tells whether a zone of this kind reacts to this sensor.
func Accepts(kind Kind, sensor Sensor) bool {
	return slices.Contains(sensors[kind], sensor)
}

// Kinds lists the kinds of zone, sorted.
func Kinds() []Kind { return slices.Sorted(maps.Keys(sensors)) }
