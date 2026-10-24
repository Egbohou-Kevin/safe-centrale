package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"safehome/internal/contract"
	"safehome/internal/inbox"
)

// Source names the installer's tools in the envelopes they send.
const Source = "tool.install"

var (
	// ErrNoRegistry: no registry on the broker, the Zigbee adapter has never run here.
	ErrNoRegistry = errors.New("no device registry on the broker: is safehome-zigbee running?")
	// ErrNoAnswer: the Zigbee adapter did not answer an order in time.
	ErrNoAnswer = errors.New("the Zigbee adapter did not answer: is safehome-zigbee running?")
	// ErrNotPaired: the device to remove is not in the registry.
	ErrNotPaired = errors.New("no such device in the registry")
	// ErrPairRefused: Zigbee2MQTT would not open the network, or keep it open, or
	// never answered the adapter; the window is over.
	ErrPairRefused = errors.New("the network could not be opened")
)

// Publisher sends one message to the broker. The program gives the MQTT client.
type Publisher interface {
	Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error
}

// Deps is what the program gives the session.
type Deps struct {
	Publisher Publisher
	Now       func() time.Time
	NewID     func() string
}

// Timing bounds the waits of a session. The zero value means the defaults.
type Timing struct {
	Confirm time.Duration // for the adapter to confirm an order (default 5 s)
	// Open is for the adapter to confirm that the network is open (default 15 s). The
	// adapter waits for Zigbee2MQTT's own confirmation, asking again, and reports
	// that none came (confirm_wait x attempts, 9 s by default): Open must be longer,
	// for the tool to hear why.
	Open  time.Duration
	Grace time.Duration // after the end of a pairing window, for its last news (default 5 s)
}

// Session is one run of a tool against the broker: it sends orders and reads what the
// Zigbee adapter publishes. Its methods are meant to be called one after the other,
// not at the same time.
type Session struct {
	site    string
	d       Deps
	timing  Timing
	in      chan inbox.Message
	dropped atomic.Int64

	registry *contract.RegistrySnapshotData // the last snapshot seen, nil before
	status   *contract.ZigbeeStatusData     // the last status seen, nil before
}

// NewSession prepares a session for a site.
func NewSession(site string, d Deps, t Timing) (*Session, error) {
	if !contract.ValidSite(site) {
		return nil, fmt.Errorf("install: invalid site %q", site)
	}
	if d.Publisher == nil || d.Now == nil || d.NewID == nil {
		return nil, errors.New("install: Publisher, Now and NewID are required")
	}
	if t.Confirm <= 0 {
		t.Confirm = 5 * time.Second
	}
	if t.Open <= 0 {
		t.Open = 15 * time.Second
	}
	if t.Grace <= 0 {
		t.Grace = 5 * time.Second
	}
	return &Session{site: site, d: d, timing: t, in: make(chan inbox.Message, 1024)}, nil
}

// Subscriptions lists what the program must subscribe to before calling the session.
func (s *Session) Subscriptions() []inbox.Subscription {
	return []inbox.Subscription{
		{Filter: contract.RegistryDevicesTopic(s.site), QoS: 1},
		{Filter: contract.RegistryEventTopic(s.site), QoS: 1},
		{Filter: contract.ZigbeeStatusTopic(s.site), QoS: 1},
		{Filter: contract.Topic(s.site, "device", "+", "event"), QoS: 1},
	}
}

// Deliver hands a received message to the session. It never blocks (the MQTT client
// calls it from its own goroutine); when the session is far behind, the message is
// dropped and counted.
func (s *Session) Deliver(m inbox.Message) {
	select {
	case s.in <- m:
	default:
		s.dropped.Add(1)
	}
}

// Dropped returns how many messages were dropped because the session was behind.
func (s *Session) Dropped() int64 { return s.dropped.Load() }

// received is one message read by the session, already decoded.
type received struct {
	env    contract.Envelope
	retain bool
}

// next returns the next message of the site, keeping the registry and the status up
// to date on the way. It returns ctx's error when ctx ends first.
func (s *Session) next(ctx context.Context) (received, error) {
	for {
		select {
		case <-ctx.Done():
			return received{}, ctx.Err()
		case m := <-s.in:
			env, err := contract.ParseEnvelope(m.Payload)
			if err != nil || env.Site != s.site {
				continue // not ours to read; the other programs log what they refuse
			}
			switch env.Type {
			case contract.TypeRegistrySnapshot:
				if d, err := contract.DecodeData[contract.RegistrySnapshotData](env); err == nil {
					s.registry = &d
				}
			case contract.TypeZigbeeStatus:
				if d, err := contract.DecodeData[contract.ZigbeeStatusData](env); err == nil {
					s.status = &d
				}
			}
			return received{env: env, retain: m.Retain}, nil
		}
	}
}

// View is what the broker knows of the Zigbee side.
type View struct {
	Devices []contract.RegistryDevice
	Status  *contract.ZigbeeStatusData // nil when the adapter never published one
}

// Snapshot waits for the retained registry (and status) until ctx ends. Without a
// registry it returns ErrNoRegistry.
func (s *Session) Snapshot(ctx context.Context) (View, error) {
	for s.registry == nil || s.status == nil {
		if _, err := s.next(ctx); err != nil {
			break
		}
	}
	if s.registry == nil {
		return View{}, ErrNoRegistry
	}
	return View{Devices: s.registry.Devices, Status: s.status}, nil
}

// send publishes an order and returns its id: the adapter's answers carry it as ref.
func (s *Session) send(ctx context.Context, typ string, data any) (string, error) {
	id := s.d.NewID()
	now := s.d.Now()
	env, err := contract.NewEnvelope(s.site, Source, uint64(now.UnixMilli()), id, now, typ, data)
	if err != nil {
		return "", fmt.Errorf("install: %w", err)
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("install: %w", err)
	}
	if err := s.d.Publisher.Publish(ctx, contract.RegistryCmdTopic(s.site), payload, 1, false); err != nil {
		return "", fmt.Errorf("install: sending %s: %w", typ, err)
	}
	return id, nil
}

// PairNews is one thing that happened during a pairing window: exactly one field is
// set.
type PairNews struct {
	Added    *contract.RegistryDeviceAddedData
	Rejected *contract.RegistryDeviceRejectedData
}

// Pair opens the network for d (whole seconds, at least one) and reports each device
// added or refused until the window closes. If ctx ends first (Ctrl-C), it closes the
// network at once. The adapter confirms the opening once Zigbee2MQTT has. It returns
// ErrNoAnswer if the adapter does not answer, and ErrPairRefused, with the reason
// (Zigbee2MQTT's words, or that it did not answer), if the network would not open.
func (s *Session) Pair(ctx context.Context, d time.Duration, report func(PairNews)) error {
	seconds := int(d / time.Second)
	if seconds < 1 {
		// Zero seconds is the order that closes the network.
		return errors.New("install: the pairing duration must be at least one second")
	}
	ref, err := s.send(ctx, contract.TypeRegistryPair, contract.RegistryPairData{DurationS: seconds})
	if err != nil {
		return err
	}
	confirmCtx, stopConfirm := context.WithTimeout(ctx, s.timing.Open)
	defer stopConfirm()
	windowCtx, stopWindow := context.WithTimeout(ctx, time.Duration(seconds)*time.Second+s.timing.Grace)
	defer stopWindow()
	w := pairWindow{ref: ref, report: report}
	for {
		wait := confirmCtx
		if w.confirmed {
			wait = windowCtx
		}
		r, err := s.next(wait)
		switch {
		case err != nil && ctx.Err() != nil:
			return s.closeNetwork() // interrupted by the installer
		case err != nil && !w.confirmed:
			return ErrNoAnswer
		case err != nil:
			return nil // the window is over
		}
		if w.read(r) {
			return w.err // the adapter closed the network: the window is over
		}
	}
}

// pairWindow follows what the adapter says about one pairing order.
type pairWindow struct {
	ref       string
	report    func(PairNews)
	confirmed bool  // the adapter opened the network for this order
	err       error // why the window ended early, nil if it did not
}

// read takes one message and reports whether the adapter closed the network.
func (w *pairWindow) read(r received) (closed bool) {
	switch r.env.Type {
	case contract.TypeZigbeeStatus:
		if r.retain {
			return false // a status from before the order says nothing about it
		}
		st, err := contract.DecodeData[contract.ZigbeeStatusData](r.env)
		if err != nil {
			return false
		}
		if st.Pairing {
			w.confirmed = true
			return false
		}
		return w.confirmed
	case contract.TypeRegistryDeviceAdded:
		if a, err := contract.DecodeData[contract.RegistryDeviceAddedData](r.env); err == nil && a.Ref == w.ref {
			w.confirmed = true
			w.report(PairNews{Added: &a})
		}
	case contract.TypeRegistryPairFailed:
		if f, err := contract.DecodeData[contract.RegistryPairFailedData](r.env); err == nil && f.Ref == w.ref {
			w.err = fmt.Errorf("%w: %s", ErrPairRefused, f.Reason)
			return true
		}
	case contract.TypeRegistryDeviceRejected:
		// A refusal outside any window has no ref: report it too, it tells the
		// installer a device tried to join.
		if j, err := contract.DecodeData[contract.RegistryDeviceRejectedData](r.env); err == nil && (j.Ref == w.ref || j.Ref == "") {
			w.report(PairNews{Rejected: &j})
		}
	}
	return false
}

// closeNetwork sends "pair for 0 s", which closes the network, even though the
// session's context is over.
func (s *Session) closeNetwork() error {
	ctx, cancel := context.WithTimeout(context.Background(), s.timing.Confirm)
	defer cancel()
	if _, err := s.send(ctx, contract.TypeRegistryPair, contract.RegistryPairData{DurationS: 0}); err != nil {
		return fmt.Errorf("install: the network may still be open: %w", err)
	}
	return nil
}

// Remove takes a device out of the network and of the registry for good, and waits
// for the adapter to confirm. The device must be in the registry: the adapter does
// not answer for a device it does not know.
func (s *Session) Remove(ctx context.Context, deviceID string) error {
	snapCtx, stop := context.WithTimeout(ctx, s.timing.Confirm)
	view, err := s.Snapshot(snapCtx)
	stop()
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(view.Devices, func(d contract.RegistryDevice) bool { return d.DeviceID == deviceID }) {
		return fmt.Errorf("%w: %s", ErrNotPaired, deviceID)
	}
	ref, err := s.send(ctx, contract.TypeRegistryRemove, contract.RegistryRemoveData{DeviceID: deviceID})
	if err != nil {
		return err
	}
	waitCtx, stopWait := context.WithTimeout(ctx, s.timing.Confirm)
	defer stopWait()
	for {
		r, err := s.next(waitCtx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrNoAnswer
		}
		if r.env.Type != contract.TypeRegistryDeviceRemoved {
			continue
		}
		if d, err := contract.DecodeData[contract.RegistryDeviceRemovedData](r.env); err == nil && d.Ref == ref {
			return nil
		}
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
// nil. After each message it reports the view (once the registry is known), and each
// device event, without a keypad's code. The local console keeps its page up to date
// with it.
func (s *Session) Follow(ctx context.Context, view func(View), event func(at time.Time, e contract.DeviceEventData)) error {
	for {
		r, err := s.next(ctx)
		if err != nil {
			return nil
		}
		if v, ok := s.Current(); ok {
			view(v)
		}
		if e, ok := deviceEvent(r); ok {
			event(r.env.TS.Time, e)
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
