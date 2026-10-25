package contract

// AlertRequestData: alert.request.
type AlertRequestData struct {
	Incident string `json:"incident"`
	// IncidentAt is when the incident was raised: the same on every wave and every
	// reminder of the incident, unlike the envelope's ts (when this message was asked).
	IncidentAt Timestamp `json:"incident_at"`
	// Zone is the site's IANA time zone ("Africa/Lome"), to read IncidentAt in the
	// site's own hours wherever the message is shown.
	Zone    string   `json:"zone"`
	Kind    string   `json:"kind"`
	Urgency string   `json:"urgency"`
	Percent int      `json:"percent,omitempty"` // battery level, for battery alerts
	Devices []string `json:"devices,omitempty"`
	Wave    int      `json:"wave,omitempty"`
	Contact string   `json:"contact,omitempty"`
	Channel string   `json:"channel,omitempty"`
	Text    string   `json:"text,omitempty"`
	// AlarmIncident names the alarm under way this alert is part of, never the
	// alert's own incident: today only a siren_failed raised during an alarm says it.
	AlarmIncident string `json:"alarm_incident,omitempty"`
	// AlarmHandled: that alarm is already handled (acknowledged, or stopped with a
	// code). The alert is told without ringing: no call, no ring tone, no sound on an
	// iPhone. Its urgency is then low, which says so but decides nothing. Only with
	// AlarmIncident (the schema's dependentRequired).
	AlarmHandled bool `json:"alarm_handled,omitempty"`
	// ZoneName is the configured name of the zone of the alert's cause ("Entree" as configured, accents kept),
	// cut like a device name (DeviceName): absent when the alert has no such zone.
	// Not to be confused with Zone, the time zone.
	ZoneName string `json:"zone_name,omitempty"`
}

// AlertResultData: alert.result.
type AlertResultData struct {
	Ref    string `json:"ref"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// AlertAckData: alert.ack.
type AlertAckData struct {
	Incident string `json:"incident"`
	Contact  string `json:"contact"`
	Via      string `json:"via"` // "sms" or "app"
}
