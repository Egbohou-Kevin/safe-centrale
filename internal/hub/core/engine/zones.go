package engine

import "safehome/internal/shared/core/zones"

// ZoneKind is the type of a zone: it decides how its devices behave. The kinds and
// the sensors each reacts to are the rule of shared/core/zones, which the
// installer's tools use too: the engine names them in its own words.
type ZoneKind = zones.Kind

const (
	ZoneEntry    = zones.Entry    // entry door: entry delay
	ZoneInstant  = zones.Instant  // perimeter: immediate alarm
	ZoneInterior = zones.Interior // interior motion
	ZoneFire     = zones.Fire     // smoke, always active
	ZonePanic    = zones.Panic    // panic button, always active
	ZoneTamper   = zones.Tamper   // tamper, always active
)

// Zone describes the zone of a device. The name and the room (empty when none) come
// from the site configuration; the room only goes to the device list, never into an
// alarm.
type Zone struct {
	Name string
	Kind ZoneKind
	Room string
}

// Accepts tells whether a zone of this kind reacts to this sensor (zones.Accepts).
func Accepts(kind ZoneKind, sensor SensorKind) bool { return zones.Accepts(kind, sensor) }

// ZoneKinds lists the kinds of zone, sorted (zones.Kinds).
func ZoneKinds() []ZoneKind { return zones.Kinds() }
