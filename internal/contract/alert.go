package contract

// AlertRequestData: alert.request.
type AlertRequestData struct {
	Incident string   `json:"incident"`
	Kind     string   `json:"kind"`
	Urgency  string   `json:"urgency"`
	Percent  int      `json:"percent,omitempty"` // battery level, for battery alerts
	Devices  []string `json:"devices,omitempty"`
	Wave     int      `json:"wave,omitempty"`
	Contact  string   `json:"contact,omitempty"`
	Channel  string   `json:"channel,omitempty"`
	Text     string   `json:"text,omitempty"`
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
