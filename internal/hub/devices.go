package hub

import (
	"time"

	"safehome/internal/contract"
	"safehome/internal/health"
	healthwire "safehome/internal/health/wire"
	"safehome/internal/inbox"
)

// The device list (system.devices): the health of each registered device, for the
// owner's list of sensors. The monitor builds it from the registry, the zones of the
// site, the battery readings and the retained availability of each device; the hub
// publishes it, and the link agent hands it to the cloud.

// registrySnapshotMessage takes the adapter's registry (registry.snapshot, retained):
// the monitor lists the devices it names.
func (h *Hub) registrySnapshotMessage(m inbox.Message) {
	_, d, ok := decode[contract.RegistrySnapshotData](h, m, contract.TypeRegistrySnapshot, "device registry")
	if !ok {
		return
	}
	reg := healthwire.Registry(d)
	err := h.monitor.loop.Submit(h.life, func(now time.Time) health.Input { reg.Now = now; return reg })
	h.submitFailed(err)
}

// publishDevices publishes the retained system.devices when what it says changed: a
// device online or not, its rounded battery, a weak battery, a device added or
// removed (a name or a zone changes with the site's configuration, which restarts the
// hub). Never again for nothing: the link agent says it again to the cloud at each
// connection. Not before the end of the boot either, when the retained states coming
// back one by one would each make a list. It runs on the monitor's loop.
func (h *Hub) publishDevices(now time.Time) {
	if !h.monitor.booted {
		return
	}
	data := healthwire.Devices(h.monitor.mon.Devices())
	h.publishChanged(&h.monitor.lastDevices, contract.DevicesTopic(h.cfg.Site), contract.TypeDevices, data, now)
}
