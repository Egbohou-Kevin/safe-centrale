package wire

import (
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/health"
	healthwire "safehome/internal/health/wire"
	"safehome/internal/power"
)

// What the module reports is what the supervision reads as the hub's supply.
func TestTheReportsAreWhatTheSupervisionReads(t *testing.T) {
	now := time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		d    contract.DeviceEventData
		want health.Input
	}{
		{Mains("dev_hubpower", power.Mains{Lost: true}), health.Mains{Now: now, Lost: true}},
		{Mains("dev_hubpower", power.Mains{}), health.Mains{Now: now}},
		{Battery("dev_hubpower", power.Battery{Percent: 38}), health.Battery{Now: now, DeviceID: "dev_hubpower", Percent: 38}},
	} {
		if err := c.d.Validate(); err != nil {
			t.Fatalf("%v: %v", c.d, err)
		}
		got, ok := healthwire.Input(c.d, now)
		if !ok || got != c.want {
			t.Errorf("%v reads as %+v, want %+v", c.d, got, c.want)
		}
	}
}
