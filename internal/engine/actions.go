package engine

import "time"

// Input is what the engine receives: a sensor signal, an order, a clock tick, or a
// device that left. Every input carries the time it happened; the engine never reads
// the clock.
type Input interface{ at() time.Time }

// Tick advances the engine's clock. The driver sends one at the time returned by
// NextWake.
type Tick struct{ Now time.Time }

// Signal is a change reported by a sensor: the device became active or inactive.
type Signal struct {
	Now      time.Time
	DeviceID string
	Sensor   SensorKind
	Active   bool
}

// Auth is the result of checking the access code, done outside the engine (codes
// are stored hashed on the hub). Checked is false if no code was checked.
type Auth struct {
	Checked bool
	OK      bool
	User    string // the person this code identifies
}

// Command is an order to the engine.
type Command struct {
	Now     time.Time
	ID      string // order identifier, echoed in the reply
	Name    CommandName
	Origin  Origin
	User    string
	Force   bool
	Devices []string
	Exp     time.Time // zero means no expiry
	Auth    Auth
}

// Forget tells the engine that a device was removed from the site, so that a door
// left open when it was taken away does not block arming any more.
type Forget struct {
	Now      time.Time
	DeviceID string
}

func (t Tick) at() time.Time    { return t.Now }
func (s Signal) at() time.Time  { return s.Now }
func (c Command) at() time.Time { return c.Now }
func (f Forget) at() time.Time  { return f.Now }

// Action is what the engine asks the outside world to do. The engine has no side
// effects of its own: the driver carries the actions out.
type Action interface{ isAction() }

// Emit publishes a domain event.
type Emit struct{ Event Event }

// SirenOn asks to sound the siren for Duration.
type SirenOn struct{ Duration time.Duration }

// SirenOff asks to stop the siren.
type SirenOff struct{}

// Alert asks for an alert to be sent. Who is told, and through which channel, is
// the escalation's business, not the alarm's.
type Alert struct {
	Incident string
	Kind     AlertKind
	Urgency  Urgency
	Cause    *Cause
	Devices  []string // devices concerned, when the alert is about several
}

// Reply answers an order.
type Reply struct {
	Ref    string
	Status Status
	Reason Reason
	Detail map[string]any
}

func (Emit) isAction()     {}
func (SirenOn) isAction()  {}
func (SirenOff) isAction() {}
func (Alert) isAction()    {}
func (Reply) isAction()    {}
