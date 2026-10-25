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
	Health health.Config // HubDeviceID is taken from the field above, Zones from Engine
	Access access.Config
	// Escalation holds the contacts and the plans: who is told, and how.
	Escalation escalation.Config

	HeartbeatEvery time.Duration // sign of life, one minute by default
	BootWindow     time.Duration // how long to listen for retained device states at start
	// LinkWait is how long the boot waits for LinkUp before listening anyway: the
	// window only makes sense once the subscriptions are active. Zero does not wait.
	LinkWait        time.Duration
	SirenCommandTTL time.Duration // a siren command not carried out within this is void

	DedupeSize int // message ids remembered
	Outbox     outbox.Settings
	AuthQueue  int // code checks that may wait

	Versions map[string]string // component -> version, for the heartbeat
}

// DefaultConfig returns the defaults decided in the design documents.
func DefaultConfig(site, hubDeviceID string, sirenIDs ...string) Config {
	return Config{
		Site: site, HubDeviceID: hubDeviceID, SirenIDs: sirenIDs,
		Engine: engine.DefaultConfig(), Health: health.DefaultConfig(), Access: access.DefaultConfig(),
		Escalation:     escalation.DefaultConfig(site, nil),
		HeartbeatEvery: time.Minute, BootWindow: 2 * time.Second, LinkWait: 30 * time.Second, SirenCommandTTL: 30 * time.Second,
		DedupeSize: 2048, Outbox: outbox.DefaultSettings(2000), AuthQueue: 16,
	}
}

// errNoSiren refuses a site without a siren.
var errNoSiren = errors.New("hub: at least one siren is required: an alarm that cannot sound is a misconfiguration")

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
		return errNoSiren
	case c.HeartbeatEvery <= 0 || c.SirenCommandTTL <= 0:
		return errors.New("hub: durations must be positive")
	case c.BootWindow < 0:
		return errors.New("hub: BootWindow must not be negative")
	case c.LinkWait < 0:
		return errors.New("hub: LinkWait must not be negative")
	case c.DedupeSize < 1 || c.AuthQueue < 1:
		return errors.New("hub: DedupeSize and AuthQueue must be at least 1")
	}
	if err := c.Outbox.Validate(); err != nil {
		return fmt.Errorf("hub: %w", err)
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

// healthConfig is the monitor's configuration, with the hub's battery module and the
// zones of the devices, which its device list names them by.
func (c Config) healthConfig() health.Config {
	hc := c.Health
	hc.HubDeviceID = c.HubDeviceID
	hc.Zones = make(map[string]health.Zone, len(c.Engine.Zones))
	for id, z := range c.Engine.Zones {
		hc.Zones[id] = health.Zone{Name: z.Name, Kind: string(z.Kind)}
	}
	return hc
}

// Deps are the ports the hub needs.
type Deps struct {
	Clock     driver.Clock
	Store     driver.Store
	Publisher outbox.Publisher
	NewID     func() string // message and incident identifiers (ULIDs)
	Logger    *slog.Logger
	// ClockSynced says whether the system clock is synchronised, for the heartbeat;
	// known is false when it cannot tell. Nil leaves clock_ok out.
	ClockSynced func() (synced, known bool)
	// Contacts returns the ids of the site's contacts as saved now, which decide whose
	// codes the hub takes. The configuration from the cloud may change them while the
	// hub runs: during the installation mode it does not restart for it. Nil keeps the
	// contacts of Config.Escalation.
	Contacts func() ([]string, error)
}

func (d Deps) validate() error {
	if d.Clock == nil || d.Store == nil || d.Publisher == nil || d.NewID == nil {
		return errors.New("hub: Clock, Store, Publisher and NewID are required")
	}
	return nil
}
