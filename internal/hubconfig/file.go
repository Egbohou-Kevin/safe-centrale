package hubconfig

import (
	"fmt"

	"safehome/internal/configfile"
)

// file is the configuration file as written. Every section is optional: what is
// left out keeps its documented default.
type file struct {
	Site           string              `json:"site"`
	SiteName       string              `json:"site_name"`
	Contacts       []contactFile       `json:"contacts"`
	Escalation     *escalationFile     `json:"escalation"`
	BrokerURL      string              `json:"broker_url"`
	ClientID       string              `json:"client_id"`
	StateDir       string              `json:"state_dir"`
	LogLevel       string              `json:"log_level"`
	Timezone       string              `json:"timezone"`
	HubDeviceID    string              `json:"hub_device_id"`
	SirenIDs       []string            `json:"siren_ids"`
	Zones          map[string]zoneFile `json:"zones"`
	Closing        map[string]string   `json:"closing"`
	Alarm          *alarmFile          `json:"alarm"`
	Health         *healthFile         `json:"health"`
	Access         *accessFile         `json:"access"`
	HeartbeatEvery string              `json:"heartbeat_every"`
	BootWindow     string              `json:"boot_window"`
	Versions       map[string]string   `json:"versions"`
}

type zoneFile struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type alarmFile struct {
	ExitDelay       string `json:"exit_delay"`
	EntryDelay      string `json:"entry_delay"`
	SirenCycle      string `json:"siren_cycle"`
	SirenPause      string `json:"siren_pause"`
	SirenCycles     *int   `json:"siren_cycles"`
	LockoutAttempts *int   `json:"lockout_attempts"`
	LockoutDuration string `json:"lockout_duration"`
	PanicSiren      *bool  `json:"panic_siren"`
	AutoArmNotice   string `json:"auto_arm_notice"`
	PostponeStep    string `json:"postpone_step"`
	PostponeMax     *int   `json:"postpone_max"`
	AutoArmLimit    string `json:"auto_arm_limit"`
}

type healthFile struct {
	HubBatteryWarn     *int   `json:"hub_battery_warn"`
	HubBatteryCritical *int   `json:"hub_battery_critical"`
	HubBatteryRearm    *int   `json:"hub_battery_rearm"`
	DeviceBatteryLow   *int   `json:"device_battery_low"`
	DeviceBatteryRearm *int   `json:"device_battery_rearm"`
	SummaryAt          string `json:"summary_at"`
	GSMDegradedAfter   string `json:"gsm_degraded_after"`
	GSMSilentAfter     string `json:"gsm_silent_after"`
	ZigbeeSilentAfter  string `json:"zigbee_silent_after"`
	ZigbeeDownAfter    string `json:"zigbee_down_after"`
	InternetSilent     string `json:"internet_silent_after"`
}

type accessFile struct {
	Iterations *int `json:"iterations"`
}

type contactFile struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	Channels []string `json:"channels"`
}

type escalationFile struct {
	InternetConfirm string              `json:"internet_confirm"`
	GSMDelay        string              `json:"gsm_delay"`
	MaxPerHour      *int                `json:"max_per_hour"`
	Plans           map[string]planFile `json:"plans"`
}

// planFile is the plan of one kind of alert. Written, it replaces the default plan of
// that kind entirely; a kind left out keeps its default (docs/escalation.md).
type planFile struct {
	Waves        []waveFile  `json:"waves"`
	Repeat       *repeatFile `json:"repeat"`
	InternetOnly bool        `json:"internet_only"`
	GSMAfter     string      `json:"gsm_after"`
}

type waveFile struct {
	After  string   `json:"after"`
	To     []string `json:"to"`
	SMSNow bool     `json:"sms_now"`
	Double bool     `json:"double"`
}

type repeatFile struct {
	Every string `json:"every"`
	Times int    `json:"times"`
}

// readConfig reads and decodes the file at path, strictly: an unknown key or
// trailing data is an error.
func readConfig(path string) (file, error) {
	var f file
	if err := configfile.Decode(path, &f); err != nil {
		return file{}, fmt.Errorf("hubconfig: %w", err)
	}
	return f, nil
}
