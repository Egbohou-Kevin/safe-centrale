package hub

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"safehome/internal/access"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/engine"
	"safehome/internal/escalation"
	"safehome/internal/health"
	"safehome/internal/outbox"
)

// Config holds everything the hub needs to know about its site.
type Config struct {
	Site        string // for example "tg-lom-0001"
	HubDeviceID string // the hub's battery module: its mains and battery readings are the hub's
	SirenIDs    []string

	Engine engine.Config
	Health health.Config // HubDeviceID is taken from the field above
	Access access.Config
	// Escalation holds the contacts and the plans: who is told, and how.
	Escalation escalation.Config

	HeartbeatEvery time.Duration // sign of life, one minute by default
	BootWindow     time.Duration // how long to listen for retained device states at start
	// LinkWait is how long the boot waits for LinkUp before listening anyway: the
	// window only makes sense once the subscriptions are active. Zero does not wait.
	LinkWait        time.Duration
	SirenCommandTTL time.Duration // a siren command not carried out within this is void

	DedupeSize int           // message ids remembered
	OutboxSize int           // messages kept while the broker is away
	RetryMin   time.Duration // first wait before sending again
	RetryMax   time.Duration // longest wait
	AuthQueue  int           // code checks that may wait

	Versions map[string]string // component -> version, for the heartbeat
}

// DefaultConfig returns the defaults decided in the design documents.
func DefaultConfig(site, hubDeviceID string, sirenIDs ...string) Config {
	return Config{
		Site: site, HubDeviceID: hubDeviceID, SirenIDs: sirenIDs,
		Engine: engine.DefaultConfig(), Health: health.DefaultConfig(), Access: access.DefaultConfig(),
		Escalation:     escalation.DefaultConfig(site, nil),
		HeartbeatEvery: time.Minute, BootWindow: 2 * time.Second, LinkWait: 30 * time.Second, SirenCommandTTL: 30 * time.Second,
		DedupeSize: 2048, OutboxSize: 2000, RetryMin: 200 * time.Millisecond, RetryMax: 5 * time.Second,
		AuthQueue: 16,
	}
}

// Validate checks the whole configuration, domains included, without building
// anything.
func (c Config) Validate() error {
	if err := c.validate(); err != nil {
		return err
	}
	if err := c.Engine.Validate(); err != nil {
		return err
	}
	if err := c.healthConfig().Validate(); err != nil {
		return err
	}
	return c.Access.Validate()
}

// validate checks what the hub itself reads; the domains check the rest when they
// are built.
func (c Config) validate() error {
	switch {
	case !contract.ValidSite(c.Site):
		return fmt.Errorf("hub: invalid site %q", c.Site)
	case c.HubDeviceID == "":
		return errors.New("hub: HubDeviceID is required")
	case len(c.SirenIDs) == 0:
		return errors.New("hub: at least one siren is required: an alarm that cannot sound is a misconfiguration")
	case c.HeartbeatEvery <= 0 || c.SirenCommandTTL <= 0 || c.RetryMin <= 0 || c.RetryMax < c.RetryMin:
		return errors.New("hub: durations must be positive")
	case c.BootWindow < 0:
		return errors.New("hub: BootWindow must not be negative")
	case c.LinkWait < 0:
		return errors.New("hub: LinkWait must not be negative")
	case c.DedupeSize < 1 || c.OutboxSize < 1 || c.AuthQueue < 1:
		return errors.New("hub: DedupeSize, OutboxSize and AuthQueue must be at least 1")
	}
	if err := c.Escalation.Validate(); err != nil {
		return fmt.Errorf("hub: %w", err)
	}
	for _, id := range c.SirenIDs {
		if id == "" {
			return errors.New("hub: empty siren id")
		}
	}
	return nil
}

// healthConfig is the monitor's configuration, with the hub's battery module.
func (c Config) healthConfig() health.Config {
	hc := c.Health
	hc.HubDeviceID = c.HubDeviceID
	return hc
}

// Publisher is the port the hub needs from the MQTT adapter (see package outbox).
type Publisher = outbox.Publisher

// ErrUnavailable is what a Publisher returns when the broker cannot be reached right
// now: the message is kept and sent again.
var ErrUnavailable = outbox.ErrUnavailable

// Deps are the ports the hub needs.
type Deps struct {
	Clock     driver.Clock
	Store     driver.Store
	Publisher Publisher
	NewID     func() string // message and incident identifiers (ULIDs)
	Logger    *slog.Logger
	// ClockSynced says whether the system clock is synchronised, for the heartbeat;
	// known is false when it cannot tell. Nil leaves clock_ok out.
	ClockSynced func() (synced, known bool)
	// DevicesChanged says whether the installer's tools changed the zones or the
	// sirens since the hub loaded them; RestartWhenQuiet has the hub restart to load
	// them, at a quiet moment. Asked when the installation mode closes; both or
	// neither.
	DevicesChanged   func() bool
	RestartWhenQuiet func()
}

func (d Deps) validate() error {
	if d.Clock == nil || d.Store == nil || d.Publisher == nil || d.NewID == nil {
		return errors.New("hub: Clock, Store, Publisher and NewID are required")
	}
	if (d.DevicesChanged == nil) != (d.RestartWhenQuiet == nil) {
		return errors.New("hub: DevicesChanged and RestartWhenQuiet go together")
	}
	return nil
}

// Message is a message received from the broker.
type Message struct {
	Topic   string
	Payload []byte
	Retain  bool
}

// Subscription is a topic filter the hub wants to listen to.
type Subscription struct {
	Filter string
	QoS    byte
}
