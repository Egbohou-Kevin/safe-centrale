package install

import (
	"context"
	"time"

	"safehome/internal/contract"
)

// What the session reads, told to whoever follows it: the local console's page, and
// the command-line tool's watch.

// Observe has view and event told of every message the session reads, as Follow
// tells them, also while a pairing or a removal reads them: the local console's page
// must not freeze during a pairing window (a device that just joined, the events of
// a sensor tried at once). It is called before the session reads anything.
func (s *Session) Observe(view func(View), event func(at time.Time, e contract.DeviceEventData)) {
	s.onView, s.onEvent = view, event
}

// observed tells the observer of a message read (see Observe).
func (s *Session) observed(r received) {
	if v, ok := s.Current(); ok && s.onView != nil {
		s.onView(v)
	}
	if e, ok := deviceEvent(r); ok && s.onEvent != nil {
		s.onEvent(r.env.TS.Time, e)
	}
}

// Current returns what the session knows of the Zigbee side now, without waiting;
// false before the registry arrived.
func (s *Session) Current() (View, bool) {
	if s.registry == nil {
		return View{}, false
	}
	return View{Devices: s.registry.Devices, Status: s.status}, true
}

// Follow reads the broker until ctx ends, which is the normal way out: it then returns
// nil. Each message reaches the observer (see Observe): the view once the registry is
// known, and each device event, without a keypad's code. The local console keeps its
// page up to date with it.
func (s *Session) Follow(ctx context.Context) error {
	for {
		if _, err := s.next(ctx); err != nil {
			return nil
		}
	}
}

// Watch reports the events of the devices (all of them, or only deviceID) until ctx
// ends, which is the normal way out: it then returns nil. A keypad's code is never
// reported.
func (s *Session) Watch(ctx context.Context, deviceID string, report func(at time.Time, e contract.DeviceEventData)) error {
	for {
		r, err := s.next(ctx)
		if err != nil {
			return nil
		}
		if e, ok := deviceEvent(r); ok && (deviceID == "" || e.DeviceID == deviceID) {
			report(r.env.TS.Time, e)
		}
	}
}

// deviceEvent decodes a device event, without the keypad's code: the tools show
// events to people, and a code must never be shown or logged.
func deviceEvent(r received) (contract.DeviceEventData, bool) {
	if r.env.Type != contract.TypeDeviceEvent {
		return contract.DeviceEventData{}, false
	}
	e, err := contract.DecodeData[contract.DeviceEventData](r.env)
	if err != nil {
		return contract.DeviceEventData{}, false
	}
	e.Code = ""
	return e, true
}
