package engine

import (
	"maps"
	"slices"
)

// ZoneKind is the type of a zone: it decides how its devices behave.
type ZoneKind string

const (
	ZoneEntry    ZoneKind = "entry"      // entry door: entry delay
	ZoneInstant  ZoneKind = "instant"    // perimeter: immediate alarm
	ZoneInterior ZoneKind = "interior"   // interior motion
	ZoneFire     ZoneKind = "24h_fire"   // smoke, always active
	ZonePanic    ZoneKind = "24h_panic"  // panic button, always active
	ZoneTamper   ZoneKind = "24h_tamper" // tamper, always active
)

// Zone describes the zone of a device. The name and the room (empty when none) come
// from the site configuration; the room only goes to the device list, never into an
// alarm.
type Zone struct {
	Name string
	Kind ZoneKind
	Room string
}

// zoneSensors lists, for each kind of zone, the sensors it reacts to: a smoke
// detector's tamper is not a fire. The tamper sensor of any device is apart: it
// always means tamper, whatever the zone (see Engine.signal). The installer's tools
// use this table too (Accepts), so that a device is never put in a zone it cannot
// trigger.
var zoneSensors = map[ZoneKind][]SensorKind{
	ZoneEntry:    {SensorContact, SensorMotion, SensorVibration, SensorGlassBreak},
	ZoneInstant:  {SensorContact, SensorMotion, SensorVibration, SensorGlassBreak},
	ZoneInterior: {SensorMotion, SensorContact},
	ZoneFire:     {SensorSmoke},
	ZonePanic:    {SensorPanic},
	ZoneTamper:   {SensorTamper},
}

func (k ZoneKind) valid() bool {
	_, ok := zoneSensors[k]
	return ok
}

// Sensors returns the sensors a zone of this kind reacts to; nil for an unknown kind.
func (k ZoneKind) Sensors() []SensorKind { return slices.Clone(zoneSensors[k]) }

// Accepts tells whether a zone of this kind reacts to this sensor.
func Accepts(kind ZoneKind, sensor SensorKind) bool {
	return slices.Contains(zoneSensors[kind], sensor)
}

// ZoneKinds lists the kinds of zone, sorted.
func ZoneKinds() []ZoneKind { return slices.Sorted(maps.Keys(zoneSensors)) }
