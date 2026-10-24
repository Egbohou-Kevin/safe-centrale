package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"safehome/internal/schematest"
)

// The JSON Schemas in schemas/ describe the contract for the other languages (the
// Java cloud, the firmware). These tests stop them from drifting away from the Go
// structures: without them, a field added on one side would be forgotten on the other.

const schemasDir = "../../schemas"

type schemaFile struct {
	ID         string                    `json:"$id"`
	Title      string                    `json:"title"`
	Properties map[string]map[string]any `json:"properties"`
	Required   []string                  `json:"required"`
}

func loadSchema(t *testing.T, name string) schemaFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(schemasDir, name))
	if err != nil {
		t.Fatal(err)
	}
	var s schemaFile
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%s: invalid JSON: %v", name, err)
	}
	if s.ID == "" || s.Title == "" {
		t.Errorf("%s: $id and title are required", name)
	}
	return s
}

// jsonFields returns the JSON fields of a struct, and those that are required
// (no omitempty or omitzero).
func jsonFields(typ reflect.Type) (all, required []string) {
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		all = append(all, name)
		if !strings.Contains(opts, "omitempty") && !strings.Contains(opts, "omitzero") {
			required = append(required, name)
		}
	}
	slices.Sort(all)
	slices.Sort(required)
	return
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestSchemasMatchGoTypes(t *testing.T) {
	cases := map[string]reflect.Type{
		"envelope.json":                 reflect.TypeOf(Envelope{}),
		"device-event.json":             reflect.TypeOf(DeviceEventData{}),
		"alarm-command.json":            reflect.TypeOf(AlarmCommandData{}),
		"alarm-ack.json":                reflect.TypeOf(AlarmAckData{}),
		"alarm-arming.json":             reflect.TypeOf(ArmingData{}),
		"alarm-armed.json":              reflect.TypeOf(ArmedData{}),
		"alarm-disarmed.json":           reflect.TypeOf(DisarmedData{}),
		"alarm-entry-delay.json":        reflect.TypeOf(EntryDelayData{}),
		"alarm-triggered.json":          reflect.TypeOf(TriggeredData{}),
		"alarm-siren-cycle.json":        reflect.TypeOf(SirenCycleData{}),
		"alarm-siren-failed.json":       reflect.TypeOf(SirenFailedData{}),
		"alarm-cleared.json":            reflect.TypeOf(ClearedData{}),
		"alarm-auth-failed.json":        reflect.TypeOf(AuthFailedData{}),
		"alarm-state.json":              reflect.TypeOf(AlarmStateData{}),
		"device-command.json":           reflect.TypeOf(DeviceCommandData{}),
		"device-command-ack.json":       reflect.TypeOf(CommandAckData{}),
		"system-heartbeat.json":         reflect.TypeOf(HeartbeatData{}),
		"system-health.json":            reflect.TypeOf(HealthData{}),
		"alarm-arming-scheduled.json":   reflect.TypeOf(ArmingScheduledData{}),
		"alarm-postponed.json":          reflect.TypeOf(PostponedData{}),
		"alert-request.json":            reflect.TypeOf(AlertRequestData{}),
		"alert-result.json":             reflect.TypeOf(AlertResultData{}),
		"alert-ack.json":                reflect.TypeOf(AlertAckData{}),
		"gsm-status.json":               reflect.TypeOf(GSMStatusData{}),
		"registry-pair.json":            reflect.TypeOf(RegistryPairData{}),
		"registry-remove.json":          reflect.TypeOf(RegistryRemoveData{}),
		"registry-snapshot.json":        reflect.TypeOf(RegistrySnapshotData{}),
		"registry-device-added.json":    reflect.TypeOf(RegistryDeviceAddedData{}),
		"registry-device-rejected.json": reflect.TypeOf(RegistryDeviceRejectedData{}),
		"registry-device-removed.json":  reflect.TypeOf(RegistryDeviceRemovedData{}),
		"registry-pair-failed.json":     reflect.TypeOf(RegistryPairFailedData{}),
		"zigbee-status.json":            reflect.TypeOf(ZigbeeStatusData{}),
		"agent-status.json":             reflect.TypeOf(AgentStatusData{}),
		"video-request.json":            reflect.TypeOf(VideoRequestData{}),
		"video-stop.json":               reflect.TypeOf(VideoStopData{}),
		"video-session.json":            reflect.TypeOf(VideoSessionData{}),
		"video-status.json":             reflect.TypeOf(VideoStatusData{}),
		"config-set.json":               reflect.TypeOf(ConfigSetData{}),
		"config-applied.json":           reflect.TypeOf(ConfigAppliedData{}),
		"photo-taken.json":              reflect.TypeOf(PhotoTakenData{}),
		"photo-upload.json":             reflect.TypeOf(PhotoUploadData{}),
		"photo-sent.json":               reflect.TypeOf(PhotoSentData{}),
		"maint-open.json":               reflect.TypeOf(MaintOpenData{}),
		"maint-close.json":              reflect.TypeOf(MaintCloseData{}),
		"maint-session.json":            reflect.TypeOf(MaintSessionData{}),
		"agent-replay.json":             reflect.TypeOf(ReplayData{}),
		"agent-replayed.json":           reflect.TypeOf(ReplayedData{}),
		"install-mode.json":             reflect.TypeOf(InstallModeData{}),
		"install-close.json":            reflect.TypeOf(InstallCloseData{}),
	}
	for file, typ := range cases {
		t.Run(file, func(t *testing.T) {
			s := loadSchema(t, file)
			all, required := jsonFields(typ)
			if got := keys(s.Properties); !slices.Equal(got, all) {
				t.Errorf("schema properties %v, Go fields %v", got, all)
			}
			req := slices.Clone(s.Required)
			slices.Sort(req)
			if !slices.Equal(req, required) {
				t.Errorf("schema required fields %v, Go fields without omitempty %v", req, required)
			}
		})
	}

	// No schema in the folder may be left out of the table above.
	entries, err := os.ReadDir(schemasDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := cases[e.Name()]; !ok && strings.HasSuffix(e.Name(), ".json") {
			t.Errorf("%s is not checked against any Go struct", e.Name())
		}
	}
}

// A bound said by a schema is the Go constant's.
func TestSchemaBoundsMatchConstants(t *testing.T) {
	for _, file := range []string{"alarm-entry-delay.json", "alarm-triggered.json"} {
		if got := loadSchema(t, file).Properties["device_name"]["maxLength"]; got != float64(MaxDeviceName) {
			t.Errorf("%s: device_name maxLength %v, MaxDeviceName %d", file, got, MaxDeviceName)
		}
	}
}

func enumOf(t *testing.T, s schemaFile, prop string) []string {
	t.Helper()
	raw, ok := s.Properties[prop]["enum"].([]any)
	if !ok {
		t.Fatalf("%s: property %s has no enum", s.Title, prop)
	}
	var out []string
	for _, v := range raw {
		out = append(out, v.(string))
	}
	slices.Sort(out)
	return out
}

func sorted(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

func TestSchemaEnumsMatchConstants(t *testing.T) {
	check := func(file, prop string, want []string) {
		t.Helper()
		if got := enumOf(t, loadSchema(t, file), prop); !slices.Equal(got, sorted(want)) {
			t.Errorf("%s.%s: schema %v, Go %v", file, prop, got, sorted(want))
		}
	}
	check("envelope.json", "type", []string{
		TypeDeviceEvent, TypeDeviceState, TypeCommand, TypeCommandAck,
		TypeAlarmCommand, TypeAlarmAck, TypeAlarmArming, TypeAlarmArmed, TypeAlarmDisarmed,
		TypeAlarmEntryDelay, TypeAlarmTriggered, TypeAlarmSirenCycle, TypeAlarmSirenFailed, TypeAlarmCleared,
		TypeAlarmArmingScheduled, TypeAlarmPostponed, TypeAlarmAuthFailed, TypeAlarmState,
		TypeAlertRequest, TypeAlertResult, TypeAlertAck,
		TypeRegistryPair, TypeRegistryRemove, TypeRegistrySnapshot, TypeRegistryDeviceAdded,
		TypeRegistryDeviceRejected, TypeRegistryDeviceRemoved, TypeRegistryPairFailed,
		TypeHeartbeat, TypeHealth, TypeGSMStatus, TypeZigbeeStatus, TypeAgentStatus,
		TypeVideoRequest, TypeVideoStop, TypeVideoSession, TypeVideoStatus,
		TypeConfigSet, TypeConfigApplied,
		TypePhotoTaken, TypePhotoUpload, TypePhotoSent,
		TypeMaintOpen, TypeMaintClose, TypeMaintSession,
		TypeAgentReplay, TypeAgentReplayed,
		TypeInstallMode, TypeInstallClose,
	})
	check("alarm-command.json", "origin", origins)
	check("alarm-ack.json", "status", []string{AckDone, AckRefused, AckExpired, AckFailed})
	check("alarm-ack.json", "reason", []string{ReasonZoneOpen, ReasonBadCode, ReasonLockedOut,
		ReasonWrongState, ReasonPostponeLimit, ReasonUnknownCommand, ReasonRateLimited})
	check("alarm-state.json", "state", []string{StateDisarmed, StateArming, StateArmed, StateEntryDelay, StateTriggered})
	check("alarm-state.json", "siren", []string{SirenOn, SirenPaused})
	check("alarm-cleared.json", "origin", []string{OriginUser, OriginTimeout})
	check("device-command.json", "command", deviceCommands)
	check("device-command-ack.json", "status", []string{CmdAccepted, CmdDone, CmdFailed, CmdExpired})
	check("system-heartbeat.json", "engine", []string{StateDisarmed, StateArming, StateArmed, StateEntryDelay, StateTriggered})
	check("system-health.json", "mains", []string{ValPresent, ValLost})
	check("alert-request.json", "kind", []string{AlertIntrusion, AlertFire, AlertPanic, AlertTamper,
		AlertAuthFailed, AlertArmingBypassed, AlertBatteryLowHub, AlertMainsRestored, AlertDeviceOffline, AlertBatteryLow, AlertGSMDegraded, AlertZigbeeDown, AlertSirenFailed})
	check("alert-request.json", "urgency", []string{UrgencyCritical, UrgencyHigh, UrgencyLow})
	check("alert-request.json", "channel", []string{ChannelApp, ChannelWhatsApp, ChannelSMS, ChannelCall})
	check("alert-result.json", "status", []string{ResultQueued, ResultSent, ResultDelivered, ResultFailed})
	check("registry-device-rejected.json", "reason", []string{RejectUnknownModel, RejectNotPairing, RejectInterviewFailed})
	check("zigbee-status.json", "bridge", []string{BridgeOnline, BridgeOffline})
	check("agent-status.json", "uplink", []string{UplinkUp, UplinkDown})
	check("video-request.json", "reason", videoReasons)
	check("video-status.json", "uplink", videoUplinks)
	check("video-session.json", "status", videoStatuses)
	check("video-session.json", "reason", videoEnds)
	check("config-applied.json", "status", []string{ConfigApplied, ConfigRejected})
	check("photo-sent.json", "status", photoStatuses)
	check("photo-sent.json", "reason", photoFailures)
	check("maint-session.json", "status", maintStatuses)
	check("maint-session.json", "reason", maintReasons)

	caps := keys(allowedValues)
	for c := range numericCapabilities {
		caps = append(caps, c)
	}
	check("device-event.json", "capability", caps)
}

// The values allowed for each capability must be the same in the schema and in the code.
func TestSchemaCapabilityValuesMatchGo(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(schemasDir, "device-event.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		AllOf []struct {
			If struct {
				Properties map[string]struct {
					Const string `json:"const"`
				} `json:"properties"`
			} `json:"if"`
			Then struct {
				Properties struct {
					Value struct {
						Enum []string `json:"enum"`
					} `json:"value"`
				} `json:"properties"`
			} `json:"then"`
		} `json:"allOf"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, c := range s.AllOf {
		capability := c.If.Properties["capability"].Const
		if numericCapabilities[capability] {
			continue
		}
		seen++
		got := sorted(c.Then.Properties.Value.Enum)
		want := sorted(allowedValues[capability])
		if !slices.Equal(got, want) {
			t.Errorf("%s: schema %v, Go %v", capability, got, want)
		}
	}
	if seen != len(allowedValues) {
		t.Errorf("the schema describes %d enumerated capabilities, the code %d", seen, len(allowedValues))
	}
}

// --- real validation with the JSON Schema validator ---------------------------

func TestSchemasAcceptValidMessages(t *testing.T) {
	ends := time.Date(2026, 10, 17, 21, 6, 5, 0, time.UTC)
	good := map[string]any{
		"device-event.json": DeviceEventData{DeviceID: "dev_k7p2", Capability: CapContact, Value: json.RawMessage(`"open"`)},
		"alarm-command.json": AlarmCommandData{Command: CmdDisarm, Args: CommandArgs{Code: "482915"},
			Origin: OriginApp, User: "u_42", Exp: TSPtr(ends)},
		"alarm-ack.json":              AlarmAckData{Ref: "x", Status: AckRefused, Reason: ReasonZoneOpen, Detail: map[string]any{"devices": []string{"d"}}},
		"alarm-arming.json":           ArmingData{Origin: OriginKeypad, EndsAt: TS(ends), Bypassed: []string{}},
		"alarm-state.json":            AlarmStateData{State: StateArming, Since: TS(ends), EndsAt: TSPtr(ends)},
		"alert-request.json":          AlertRequestData{Incident: "i", IncidentAt: TS(ends), Zone: "Africa/Lome", Kind: AlertFire, Urgency: UrgencyCritical, Channel: ChannelSMS},
		"alert-result.json":           AlertResultData{Ref: "r", Status: ResultSent},
		"alert-ack.json":              AlertAckData{Incident: "i", Contact: "c", Via: "sms"},
		"alarm-arming-scheduled.json": ArmingScheduledData{ScheduledFor: TS(ends)},
		"alarm-postponed.json":        PostponedData{ScheduledFor: TS(ends), Count: 1, Max: 3},
		"alarm-siren-failed.json":     SirenFailedData{Incident: "i", DeviceID: "dev_siren1"},
		"device-command.json":         DeviceCommandData{DeviceID: "dev_s3x9", Command: CmdSirenOn, Args: DeviceCommandArgs{DurationS: 300}, Exp: TSPtr(ends)},
		"device-command-ack.json":     CommandAckData{Ref: "r", Status: CmdDone},
		"system-heartbeat.json":       HeartbeatData{BootID: "b", UptimeS: 86412, Engine: StateArmed, FW: map[string]string{"engine": "0.1.0"}},
		"system-health.json": HealthData{Mains: ValLost, HubBattery: intPtr(82), Links: map[string]string{"zigbee": "ok", "internet": "down"},
			GSM: &GSMHealth{Lines: []GSMLine{{ID: "line1", Operator: "togocom", Responding: true, Registered: true, Signal: 71, Credit: "ok", LastTest: TSPtr(ends), LastTestOK: boolPtr(true)}}}},
		"gsm-status.json":      GSMStatusData{Queued: 2, Lines: []GSMLine{{ID: "line1", Responding: true, Registered: true, Signal: 70, Credit: "empty", Restarts: 1}}},
		"alarm-triggered.json": TriggeredData{Incident: "i", DeviceID: "d", Capability: CapContact, Zone: "entrance"},
		"registry-pair.json":   RegistryPairData{DurationS: 120},
		"registry-remove.json": RegistryRemoveData{DeviceID: "dev_k7p2"},
		"registry-snapshot.json": RegistrySnapshotData{Devices: []RegistryDevice{{DeviceID: "dev_k7p2", Model: "aqara.contact.mccgq11lm",
			Capabilities: []string{CapContact, CapBattery}, Adapter: AdapterZigbee}}},
		"registry-device-added.json":    RegistryDeviceAddedData{Ref: "r", DeviceID: "dev_k7p2", Model: "aqara.contact.mccgq11lm", Capabilities: []string{CapContact}},
		"registry-device-rejected.json": RegistryDeviceRejectedData{Model: "", Reason: RejectInterviewFailed},
		"registry-device-removed.json":  RegistryDeviceRemovedData{DeviceID: "dev_k7p2"},
		"registry-pair-failed.json":     RegistryPairFailedData{Ref: "r", Reason: "Cannot permit join for more than 254 seconds."},
		"zigbee-status.json":            ZigbeeStatusData{Bridge: BridgeOnline, Devices: 4, Offline: []string{"dev_k7p2"}},
		"agent-status.json":             AgentStatusData{Uplink: UplinkUp, Transport: "mqtts", Queued: 3, OldestS: 42},
		"video-request.json":            goodVideoRequest(ends),
		"video-stop.json":               VideoStopData{Session: "s1"},
		"video-session.json":            VideoSessionData{Session: "s1", Camera: "cam_caisse", Status: VideoEnded, Reason: VideoTimeout, DurationS: 120, Bytes: 7_500_000},
		"video-status.json": VideoStatusData{Cameras: []VideoCamera{{ID: "cam_caisse", Name: "Caisse", Reachable: true}},
			Active: 1, Uplink: UplinkMobile, UsedMBMonth: 340, QuotaMB: 2000, MaxS: 300},
		"config-set.json":     goodConfig(),
		"config-applied.json": ConfigAppliedData{Version: 7, Status: ConfigRejected, Reason: "contact u_x: unknown role"},
		"photo-taken.json":    PhotoTakenData{Photo: "p1", Incident: "i1", Camera: "cam_caisse", Bytes: 41_000, Width: 640},
		"photo-upload.json":   goodPhotoUpload(ends),
		"photo-sent.json":     PhotoSentData{Photo: "p1", Status: PhotoFailed, Reason: PhotoURLExpired},
		"maint-open.json":     MaintOpenData{Session: "m1", User: "kevin", Reason: "the modem is silent", DurationS: 3600, Exp: TS(ends)},
		"maint-close.json":    MaintCloseData{Session: "m1"},
		"maint-session.json":  MaintSessionData{Session: "m1", User: "kevin", Status: MaintOpen, Until: TSPtr(ends)},
		"agent-replay.json":   ReplayData{From: 120, To: 130},
		"agent-replayed.json": ReplayedData{From: 120, To: 130, Last: 4012, Resent: 8, Missing: []SeqRange{{120, 121}, {127, 127}}},
		"install-mode.json":   InstallModeData{Status: InstallOpen, Until: TSPtr(ends)},
		"install-close.json":  InstallCloseData{},
	}
	schematest.Validate(t, "install-mode.json", InstallModeData{Status: InstallClosed, Reason: InstallTimeout})
	schematest.Validate(t, "config-set.json", ConfigSetData{Version: 1})
	for file, v := range good {
		schematest.Validate(t, file, v)
	}
	schematest.Validate(t, "device-event.json", DeviceEventData{DeviceID: "dev_kp", Capability: CapKeypad, Value: json.RawMessage(`"disarm"`), Code: "482915"})
	schematest.Validate(t, "device-command.json", DeviceCommandData{DeviceID: "dev_s3x9", Command: CmdSirenOff})
	schematest.Validate(t, "system-health.json", HealthData{Links: map[string]string{}}) // the smallest valid health
	for _, c := range []struct {
		capability string
		value      string
	}{{CapBattery, `82`}, {CapLink, `0`}, {CapPanic, `"pressed"`}, {CapKeypad, `"arm_now"`}, {CapAvailability, `"offline"`}} {
		schematest.Validate(t, "device-event.json", DeviceEventData{DeviceID: "d", Capability: c.capability, Value: json.RawMessage(c.value)})
	}

	e := newTestEnvelope(t, DeviceEventData{DeviceID: "d", Capability: CapContact, Value: json.RawMessage(`"open"`)})
	schematest.Validate(t, "envelope.json", e)
}

func TestSchemasRejectInvalidMessages(t *testing.T) {
	const okEnvelope = `{"v":1,"id":"01JB8ZK3M2Q7T4X9H6N5R0WYCE","site":"tg-lom-0001","src":"x","seq":1,"ts":"2026-10-17T21:00:00.000Z","type":"device.event","data":{}}`
	bad := []struct{ name, file, json string }{
		{"config without version", "config-set.json", `{"site_name":"x"}`},
		{"maintenance of two hours", "maint-open.json", `{"session":"m","user":"u","reason":"r","duration_s":7200,"exp":"2026-10-17T21:00:00.000Z"}`},
		{"maintenance without reason", "maint-open.json", `{"session":"m","user":"u","duration_s":600,"exp":"2026-10-17T21:00:00.000Z"}`},
		{"replay from zero", "agent-replay.json", `{"from":0,"to":3}`},
		{"replayed without resent", "agent-replayed.json", `{"from":1,"to":3,"last":9}`},
		{"installation mode ajar", "install-mode.json", `{"status":"ajar"}`},
		{"installation mode closed for no reason", "install-mode.json", `{"status":"closed"}`},
		{"installation mode open for ever", "install-mode.json", `{"status":"open"}`},
		{"installation mode closed with an end", "install-mode.json", `{"status":"closed","reason":"timeout","until":"2026-10-17T21:00:00.000Z"}`},
		{"maintenance with an unknown status", "maint-session.json", `{"session":"m","status":"ajar"}`},
		{"photo of no size", "photo-taken.json", `{"photo":"p","incident":"i","camera":"c","bytes":0,"width":640}`},
		{"photo address in http", "photo-upload.json", `{"photo":"p","url":"http://x.example/p","token":"t","exp":"2026-10-17T21:00:00.000Z"}`},
		{"photo address with a query", "photo-upload.json", `{"photo":"p","url":"https://x.example/p?sig=1","token":"t","exp":"2026-10-17T21:00:00.000Z"}`},
		{"photo address with credentials", "photo-upload.json", `{"photo":"p","url":"https://u:pw@x.example/p","token":"t","exp":"2026-10-17T21:00:00.000Z"}`},
		{"photo upload without token", "photo-upload.json", `{"photo":"p","url":"https://x.example/p","exp":"2026-10-17T21:00:00.000Z"}`},
		{"photo sent with an unknown reason", "photo-sent.json", `{"photo":"p","status":"failed","reason":"lost"}`},
		{"config with a long entry delay", "config-set.json", `{"version":2,"entry_delay_s":600}`},
		{"config with a huge video quota", "config-set.json", `{"version":2,"video":{"quota_mb":8388609}}`},
		{"config with a local phone", "config-set.json", `{"version":2,"contacts":[{"id":"u_a","name":"A","role":"owner","channels":["sms"],"phone":"90000001"}]}`},
		{"config with an unknown role", "config-set.json", `{"version":2,"contacts":[{"id":"u_a","name":"A","role":"king","channels":["app"]}]}`},
		{"config with a bad closing time", "config-set.json", `{"version":2,"closing":{"mon":"25:00"}}`},
		{"config with an unknown day", "config-set.json", `{"version":2,"closing":{"someday":"20:00"}}`},
		{"applied with an unknown status", "config-applied.json", `{"version":2,"status":"maybe"}`},
		{"unknown capability", "device-event.json", `{"device_id":"d","capability":"unknown","value":"x"}`},
		{"value not allowed for the capability", "device-event.json", `{"device_id":"d","capability":"contact","value":"ajar"}`},
		{"enumerated capability with a number", "device-event.json", `{"device_id":"d","capability":"contact","value":1}`},
		{"battery above 100", "device-event.json", `{"device_id":"d","capability":"battery","value":101}`},
		{"battery as text", "device-event.json", `{"device_id":"d","capability":"battery","value":"full"}`},
		{"missing device_id", "device-event.json", `{"capability":"contact","value":"open"}`},
		{"SMS origin not allowed in v1", "alarm-command.json", `{"command":"alarm.arm","args":{},"origin":"sms"}`},
		{"missing args", "alarm-command.json", `{"command":"alarm.arm","origin":"app"}`},
		{"order code of three digits", "alarm-command.json", `{"command":"alarm.disarm","args":{"code":"482"},"origin":"app"}`},
		{"order code of nine digits", "alarm-command.json", `{"command":"alarm.disarm","args":{"code":"482915736"},"origin":"app"}`},
		{"order code with letters", "alarm-command.json", `{"command":"alarm.disarm","args":{"code":"48a915"},"origin":"app"}`},
		{"order code with a new line", "alarm-command.json", `{"command":"alarm.disarm","args":{"code":"482915\n"},"origin":"app"}`},
		{"unknown ack status", "alarm-ack.json", `{"ref":"x","status":"maybe"}`},
		{"unknown reason", "alarm-ack.json", `{"ref":"x","status":"refused","reason":"because"}`},
		{"malformed date", "alarm-entry-delay.json", `{"device_id":"d","ends_at":"tomorrow"}`},
		{"unknown state", "alarm-state.json", `{"state":"asleep","since":"2026-10-17T21:00:00.000Z"}`},
		{"unknown urgency", "alert-request.json", `{"incident":"i","kind":"fire","urgency":"medium"}`},
		{"unknown channel", "alert-request.json", `{"incident":"i","kind":"fire","urgency":"high","channel":"pigeon"}`},
		{"acknowledgement through an unknown channel", "alert-ack.json", `{"incident":"i","contact":"c","via":"fax"}`},
		{"postponement without a count", "alarm-postponed.json", `{"scheduled_for":"2026-10-17T21:30:00.000Z","max":3}`},
		{"postponement count of zero", "alarm-postponed.json", `{"scheduled_for":"2026-10-17T21:30:00.000Z","count":0,"max":3}`},
		{"notice without a date", "alarm-arming-scheduled.json", `{}`},
		{"battery percent above 100", "alert-request.json", `{"incident":"i","kind":"battery_low_hub","urgency":"high","percent":101}`},
		{"alert devices as text", "alert-request.json", `{"incident":"i","kind":"arming_bypassed","urgency":"high","devices":"d"}`},
		{"code on a door", "device-event.json", `{"device_id":"d","capability":"contact","value":"open","code":"482915"}`},
		{"code on a keypad arm", "device-event.json", `{"device_id":"d","capability":"keypad","value":"arm","code":"482915"}`},
		{"code with letters", "device-event.json", `{"device_id":"d","capability":"keypad","value":"disarm","code":"48a915"}`},
		{"siren.on without duration", "device-command.json", `{"device_id":"d","command":"siren.on","args":{}}`},
		{"unknown device command", "device-command.json", `{"device_id":"d","command":"door.unlock","args":{}}`},
		{"command without args", "device-command.json", `{"device_id":"d","command":"siren.off"}`},
		{"unknown command ack status", "device-command-ack.json", `{"ref":"r","status":"maybe"}`},
		{"heartbeat with an unknown state", "system-heartbeat.json", `{"boot_id":"b","uptime_s":1,"engine":"asleep","queue_depth":0}`},
		{"negative uptime", "system-heartbeat.json", `{"boot_id":"b","uptime_s":-1,"engine":"armed","queue_depth":0}`},
		{"health without links", "system-health.json", `{"mains":"lost"}`},
		{"hub battery above 100", "system-health.json", `{"links":{},"hub_battery":120}`},
		{"gsm line without signal", "system-health.json", `{"links":{},"gsm":{"lines":[{"id":"l","responding":true,"registered":true}]}}`},
		{"gsm status without queue", "gsm-status.json", `{"lines":[]}`},
		{"gsm line negative restarts", "gsm-status.json", `{"queued":0,"lines":[{"id":"l","responding":true,"registered":true,"signal":3,"restarts":-1}]}`},
		{"zero cycle", "alarm-siren-cycle.json", `{"incident":"i","cycle":0,"of":3}`},
		{"envelope: malformed id", "envelope.json", strings.Replace(okEnvelope, "01JB8ZK3M2Q7T4X9H6N5R0WYCE", "not-a-ulid", 1)},
		{"envelope: unknown type", "envelope.json", strings.Replace(okEnvelope, "device.event", "alarm.explode", 1)},
		{"envelope: data as text", "envelope.json", strings.Replace(okEnvelope, `"data":{}`, `"data":"text"`, 1)},
		{"envelope: uppercase site", "envelope.json", strings.Replace(okEnvelope, "tg-lom-0001", "TG-LOM", 1)},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if err := schematest.Check(c.file, []byte(c.json)); err == nil {
				t.Fatalf("%s should have refused %s", c.file, c.json)
			}
		})
	}
	// The reference envelope itself must be valid, or the cases above prove nothing.
	if err := schematest.Check("envelope.json", []byte(okEnvelope)); err != nil {
		t.Fatalf("the reference envelope must be valid: %v", err)
	}
}

// An alert that is part of an alarm names it (alarm_incident), and only an alert
// that names its alarm may say that alarm is handled (dependentRequired): a handled
// flag without its alarm would quiet the phones about nothing they can tie it to.
func TestAnAlertSaysItsAlarmIsHandledOnlyWithThatAlarm(t *testing.T) {
	at := TS(time.Date(2026, 10, 24, 16, 15, 0, 0, time.UTC))
	loud := AlertRequestData{Incident: "y", IncidentAt: at, Zone: "Africa/Lome", Kind: AlertSirenFailed,
		Urgency: UrgencyHigh, Contact: "u_guard", Channel: ChannelSMS, AlarmIncident: "x"}
	quiet := loud
	quiet.Urgency, quiet.AlarmHandled = UrgencyLow, true
	schematest.Validate(t, "alert-request.json", loud)
	schematest.Validate(t, "alert-request.json", quiet)

	const ok = `{"incident":"y","incident_at":"2026-10-24T16:15:00.000Z","zone":"Africa/Lome","kind":"siren_failed","urgency":"low","alarm_incident":"x","alarm_handled":true}`
	if err := schematest.Check("alert-request.json", []byte(ok)); err != nil {
		t.Fatalf("the reference alert must be valid: %v", err)
	}
	for name, raw := range map[string]string{
		"handled without its alarm": strings.Replace(ok, `"alarm_incident":"x",`, "", 1),
		"an empty alarm":            strings.Replace(ok, `"alarm_incident":"x"`, `"alarm_incident":""`, 1),
		"handled said in words":     strings.Replace(ok, `"alarm_handled":true`, `"alarm_handled":"true"`, 1),
	} {
		if err := schematest.Check("alert-request.json", []byte(raw)); err == nil {
			t.Errorf("%s: accepted %s", name, raw)
		}
	}
	for name, raw := range map[string]string{
		"without its siren": `{"incident":"x"}`,
		"without its alarm": `{"device_id":"dev_siren1"}`,
		"of an empty alarm": `{"incident":"","device_id":"dev_siren1"}`,
	} {
		if err := schematest.Check("alarm-siren-failed.json", []byte(raw)); err == nil {
			t.Errorf("a silent siren %s: accepted", name)
		}
	}
}

// While triggered, the state names the incident that is ringing and where its siren
// schedule stands (siren, ends_at): the three come together, or not at all from a hub
// older than them. No other state says them.
func TestTheStateSaysTheSirenOnlyWhileTriggered(t *testing.T) {
	ends := TSPtr(time.Date(2026, 10, 25, 16, 20, 0, 0, time.UTC))
	ringing := AlarmStateData{State: StateTriggered, Since: TS(ends.Add(-5 * time.Minute)), EndsAt: ends,
		Incident: "i", Siren: SirenOn, Cycle: 2, Of: 3}
	paused := ringing
	paused.Siren = SirenPaused
	older := AlarmStateData{State: StateTriggered, Since: ringing.Since, Cycle: 2, Of: 3}
	for _, d := range []AlarmStateData{ringing, paused, older} {
		schematest.Validate(t, "alarm-state.json", d)
	}

	const ok = `{"state":"triggered","since":"2026-10-25T16:15:00.000Z","ends_at":"2026-10-25T16:20:00.000Z","incident":"i","siren":"on","cycle":2,"of":3}`
	if err := schematest.Check("alarm-state.json", []byte(ok)); err != nil {
		t.Fatalf("the reference state must be valid: %v", err)
	}
	for name, raw := range map[string]string{
		"a siren without its incident":  strings.Replace(ok, `"incident":"i",`, "", 1),
		"an incident without its siren": strings.Replace(ok, `"siren":"on",`, "", 1),
		"a siren without its end":       strings.Replace(ok, `"ends_at":"2026-10-25T16:20:00.000Z",`, "", 1),
		"an empty incident":             strings.Replace(ok, `"incident":"i"`, `"incident":""`, 1),
		"a siren off":                   strings.Replace(ok, `"siren":"on"`, `"siren":"off"`, 1),
		"a siren while armed":           strings.Replace(ok, `"triggered"`, `"armed"`, 1),
		"a siren while arming":          strings.Replace(ok, `"triggered"`, `"arming"`, 1),
		"an incident while disarmed":    `{"state":"disarmed","since":"2026-10-25T16:15:00.000Z","incident":"i"}`,
	} {
		if err := schematest.Check("alarm-state.json", []byte(raw)); err == nil {
			t.Errorf("%s: accepted %s", name, raw)
		}
	}
}

// Unknown fields stay allowed: a field can be added without bumping the contract
// version.
func TestSchemasToleratesUnknownFields(t *testing.T) {
	raw := []byte(`{"device_id":"d","capability":"contact","value":"open","future_field":42}`)
	if err := schematest.Check("device-event.json", raw); err != nil {
		t.Fatalf("an unknown field must stay allowed: %v", err)
	}
}

func intPtr(n int) *int    { return &n }
func boolPtr(b bool) *bool { return &b }

func goodVideoRequest(exp time.Time) VideoRequestData {
	return VideoRequestData{
		Session: "s1", Camera: "cam_caisse", Reason: VideoForAlarm, User: "u_owner", Incident: "inc1", MaxS: 120,
		PushURL: "rtmps://relay.safehome.example/live/s1", Token: "tok-secret-123", Exp: TS(exp),
	}
}

func TestVideoSchemasRefuseWhatTheContractRefuses(t *testing.T) {
	for name, raw := range map[string]string{
		"too long":   `{"session":"s","camera":"c","reason":"demand","max_s":86401,"push_url":"rtmps://r/x","token":"t","exp":"2026-10-17T21:06:05.000Z"}`,
		"plain rtmp": `{"session":"s","camera":"c","reason":"demand","max_s":60,"push_url":"rtmp://r/x","token":"t","exp":"2026-10-17T21:06:05.000Z"}`,
		"no token":   `{"session":"s","camera":"c","reason":"demand","max_s":60,"push_url":"rtmps://r/x","exp":"2026-10-17T21:06:05.000Z"}`,
		"bad reason": `{"session":"s","camera":"c","reason":"curious","max_s":60,"push_url":"rtmps://r/x","token":"t","exp":"2026-10-17T21:06:05.000Z"}`,
		"no exp":     `{"session":"s","camera":"c","reason":"demand","max_s":60,"push_url":"rtmps://r/x","token":"t"}`,
	} {
		if err := schematest.Check("video-request.json", []byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// On a fixed line a request has no limit.
	unlimited := goodVideoRequest(time.Date(2026, 10, 17, 21, 6, 5, 0, time.UTC))
	unlimited.MaxS = 0
	schematest.Validate(t, "video-request.json", unlimited)
	schematest.Validate(t, "video-status.json", VideoStatusData{Cameras: []VideoCamera{}, Uplink: UplinkFixed})
	if err := schematest.Check("video-status.json", []byte(`{"cameras":[],"active":0,"uplink":"satellite","used_mb_month":0,"quota_mb":0,"max_s":0}`)); err == nil {
		t.Error("unknown uplink accepted")
	}
	if err := schematest.Check("video-session.json", []byte(`{"session":"s","camera":"c","status":"paused"}`)); err == nil {
		t.Error("unknown status accepted")
	}
}
