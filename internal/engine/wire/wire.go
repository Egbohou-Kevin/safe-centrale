// Package wire translates between the alarm engine's domain types and the event
// contract. It is the only place that knows both: the engine never imports the
// contract, so a new contract version only changes this package.
package wire

import (
	"fmt"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
)

// sensors maps contract capabilities to the sensor kinds the engine understands.
// Other capabilities (battery, link, keypad, siren...) are not zone signals.
var sensors = map[string]engine.SensorKind{
	contract.CapContact:    engine.SensorContact,
	contract.CapMotion:     engine.SensorMotion,
	contract.CapSmoke:      engine.SensorSmoke,
	contract.CapVibration:  engine.SensorVibration,
	contract.CapGlassBreak: engine.SensorGlassBreak,
	contract.CapPanic:      engine.SensorPanic,
	contract.CapTamper:     engine.SensorTamper,
}

// Signal turns a device event into an engine signal. ok is false when the event
// is not a zone signal (a battery level, for instance) and must not reach the
// engine.
func Signal(d contract.DeviceEventData, now time.Time) (sig engine.Signal, ok bool) {
	kind, isSensor := sensors[d.Capability]
	if !isSensor {
		return engine.Signal{}, false
	}
	return engine.Signal{
		Now:      now,
		DeviceID: d.DeviceID,
		Sensor:   kind,
		Active:   contract.IsActive(d.Capability, d.StringValue()),
	}, true
}

// Forget tells the engine that a device left the registry (registry.device_removed).
func Forget(d contract.RegistryDeviceRemovedData, now time.Time) engine.Forget {
	return engine.Forget{Now: now, DeviceID: d.DeviceID}
}

var commands = map[string]engine.CommandName{
	contract.CmdArm:      engine.CmdArm,
	contract.CmdArmNow:   engine.CmdArmNow,
	contract.CmdDisarm:   engine.CmdDisarm,
	contract.CmdSilence:  engine.CmdSilence,
	contract.CmdBypass:   engine.CmdBypass,
	contract.CmdPostpone: engine.CmdPostpone,
}

// Command turns an alarm.command payload into an engine command. ref is the id of
// the message (echoed in the acknowledgement) and auth is the outcome of the code
// check, done by the caller because the code is hashed on the hub.
//
// An unknown command name is passed through as is: the engine answers it with an
// unknown_command failure.
func Command(d contract.AlarmCommandData, ref string, now time.Time, auth engine.Auth) engine.Command {
	name, known := commands[d.Command]
	if !known {
		name = engine.CommandName(d.Command)
	}
	c := engine.Command{
		Now:     now,
		ID:      ref,
		Name:    name,
		Origin:  engine.Origin(d.Origin),
		User:    d.User,
		Force:   d.Args.Force,
		Devices: d.Args.Devices,
		Auth:    auth,
	}
	if d.Exp != nil {
		c.Exp = d.Exp.Time
	}
	return c
}

// Event is a contract message body to publish on the alarm/event topic.
type Event struct {
	Type string
	Data any
}

// SirenCommand is a command for the siren device (topic device/{id}/cmd).
type SirenCommand struct {
	Command   string // contract.CmdSirenOn or contract.CmdSirenOff
	DurationS int
}

// Data builds the device command for one siren. exp is the time after which the
// siren must ignore the command, so that a command delayed by an outage does not
// sound the siren long after the alarm.
func (s SirenCommand) Data(deviceID string, exp time.Time) contract.DeviceCommandData {
	return contract.DeviceCommandData{
		DeviceID: deviceID,
		Command:  s.Command,
		Args:     contract.DeviceCommandArgs{DurationS: s.DurationS},
		Exp:      contract.TSPtr(exp),
	}
}

// Out groups what the driver must publish after a call to Engine.Handle.
type Out struct {
	Events []Event                 // alarm/event
	Acks   []contract.AlarmAckData // alarm/ack
	Siren  []SirenCommand          // device/{siren}/cmd
}

// Translate turns engine actions into contract payloads. The engine's alerts are
// left out: they do not leave as they are, the escalation turns each into one alert
// request per contact and channel.
func Translate(actions []engine.Action) (Out, error) {
	var out Out
	for _, a := range actions {
		switch v := a.(type) {
		case engine.Emit:
			ev, err := translateEvent(v.Event)
			if err != nil {
				return Out{}, err
			}
			out.Events = append(out.Events, ev)
		case engine.Reply:
			ack, err := translateReply(v)
			if err != nil {
				return Out{}, err
			}
			out.Acks = append(out.Acks, ack)
		case engine.Alert, engine.Ended:
			// left to the escalation
		case engine.SirenOn:
			out.Siren = append(out.Siren, SirenCommand{Command: contract.CmdSirenOn, DurationS: sirenSeconds(v.Duration)})
		case engine.SirenOff:
			out.Siren = append(out.Siren, SirenCommand{Command: contract.CmdSirenOff})
		default:
			return Out{}, fmt.Errorf("wire: unknown action %T", a)
		}
	}
	return out, nil
}

// sirenSeconds rounds a siren duration up to whole seconds: what is left of a cycle
// is rarely round, and a siren order must last at least a second.
func sirenSeconds(d time.Duration) int {
	return int((d + time.Second - 1) / time.Second)
}

var clearedBy = map[engine.ClearedBy]string{
	engine.ClearedByUser:    contract.OriginUser,
	engine.ClearedByTimeout: contract.OriginTimeout,
}

func translateEvent(e engine.Event) (Event, error) {
	switch v := e.(type) {
	case engine.ArmingScheduled:
		return Event{contract.TypeAlarmArmingScheduled, contract.ArmingScheduledData{ScheduledFor: contract.TS(v.ScheduledFor)}}, nil
	case engine.Postponed:
		return Event{contract.TypeAlarmPostponed, contract.PostponedData{
			ScheduledFor: contract.TS(v.ScheduledFor), Count: v.Count, Max: v.Max,
		}}, nil
	case engine.ArmingStarted:
		return Event{contract.TypeAlarmArming, contract.ArmingData{
			Origin: string(v.Origin), User: v.User, EndsAt: contract.TS(v.EndsAt),
			Forced: v.Forced, Bypassed: v.Bypassed,
		}}, nil
	case engine.Armed:
		return Event{contract.TypeAlarmArmed, contract.ArmedData{Origin: string(v.Origin), User: v.User}}, nil
	case engine.Disarmed:
		// Always a list, empty when the disarm ended no incident.
		incidents := append([]string{}, v.Incidents...)
		return Event{contract.TypeAlarmDisarmed, contract.DisarmedData{Origin: string(v.Origin), User: v.User, Incidents: incidents}}, nil
	case engine.EntryDelayStarted:
		// The site configuration names a device by its zone (device id -> name, kind).
		return Event{contract.TypeAlarmEntryDelay, contract.EntryDelayData{
			DeviceID: v.DeviceID, DeviceName: contract.DeviceName(v.Zone), EndsAt: contract.TS(v.EndsAt),
		}}, nil
	case engine.Triggered:
		return Event{contract.TypeAlarmTriggered, contract.TriggeredData{
			Incident: v.Incident, DeviceID: v.Cause.DeviceID, DeviceName: contract.DeviceName(v.Cause.Zone),
			Capability: string(v.Cause.Sensor), Zone: v.Cause.Zone,
		}}, nil
	case engine.SirenCycleStarted:
		return Event{contract.TypeAlarmSirenCycle, contract.SirenCycleData{Incident: v.Incident, Cycle: v.Cycle, Of: v.Of}}, nil
	case engine.Cleared:
		origin, ok := clearedBy[v.By]
		if !ok {
			return Event{}, fmt.Errorf("wire: unknown ClearedBy %q", v.By)
		}
		return Event{contract.TypeAlarmCleared, contract.ClearedData{
			Incident: v.Incident, Origin: origin, User: v.User, DurationS: int(v.Duration / time.Second),
		}}, nil
	case engine.AuthFailed:
		return Event{contract.TypeAlarmAuthFailed, contract.AuthFailedData{
			Origin: string(v.Origin), Attempts: v.Attempts, LockedUntil: contract.TSPtr(v.LockedUntil),
		}}, nil
	}
	return Event{}, fmt.Errorf("wire: unknown event %T", e)
}

var statuses = map[engine.Status]string{
	engine.StatusDone:    contract.AckDone,
	engine.StatusRefused: contract.AckRefused,
	engine.StatusExpired: contract.AckExpired,
	engine.StatusFailed:  contract.AckFailed,
}

var reasons = map[engine.Reason]string{
	engine.ReasonZoneOpen:       contract.ReasonZoneOpen,
	engine.ReasonBadCode:        contract.ReasonBadCode,
	engine.ReasonLockedOut:      contract.ReasonLockedOut,
	engine.ReasonWrongState:     contract.ReasonWrongState,
	engine.ReasonPostponeLimit:  contract.ReasonPostponeLimit,
	engine.ReasonUnknownCommand: contract.ReasonUnknownCommand,
}

func translateReply(r engine.Reply) (contract.AlarmAckData, error) {
	status, ok := statuses[r.Status]
	if !ok {
		return contract.AlarmAckData{}, fmt.Errorf("wire: unknown status %q", r.Status)
	}
	ack := contract.AlarmAckData{Ref: r.Ref, Status: status, Detail: r.Detail}
	if r.Reason != "" {
		reason, ok := reasons[r.Reason]
		if !ok {
			return contract.AlarmAckData{}, fmt.Errorf("wire: unknown reason %q", r.Reason)
		}
		ack.Reason = reason
	}
	return ack, nil
}

var modes = map[engine.Mode]string{
	engine.ModeDisarmed:   contract.StateDisarmed,
	engine.ModeArming:     contract.StateArming,
	engine.ModeArmed:      contract.StateArmed,
	engine.ModeEntryDelay: contract.StateEntryDelay,
	engine.ModeTriggered:  contract.StateTriggered,
}

// StateData turns the engine summary into the payload of the retained alarm.state
// message.
func StateData(s engine.State) (contract.AlarmStateData, error) {
	mode, ok := modes[s.Mode]
	if !ok {
		return contract.AlarmStateData{}, fmt.Errorf("wire: unknown mode %q", s.Mode)
	}
	return contract.AlarmStateData{
		State:       mode,
		Since:       contract.TS(s.Since),
		EndsAt:      contract.TSPtr(s.EndsAt),
		Cycle:       s.Cycle,
		Of:          s.Of,
		LockedUntil: contract.TSPtr(s.LockedUntil),

		NextScheduled: contract.TSPtr(s.NextScheduled),
	}, nil
}
