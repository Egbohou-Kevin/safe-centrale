package zigbee

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Profile says how to read and drive one kind of device. Adding a model is adding
// a profile: data, not code (see docs/adapter-zigbee.md).
type Profile struct {
	Name         string                    // ours, for example aqara.contact.mccgq11lm
	Models       []string                  // the Zigbee2MQTT models it applies to
	Capabilities map[string]Rule           // our capability -> how to read it
	Commands     map[string]ProfileCommand // our command -> what to send
	OfflineAfter time.Duration             // silence before the device counts as offline; zero: Config.OfflineAfter
}

// Rule reads one capability from a Zigbee2MQTT field.
type Rule struct {
	From string // the Zigbee2MQTT field
	// Map turns the raw value, written as text ("true", "false", "single"), into
	// ours. Without Map the value is a number from 0 to 100.
	Map map[string]string
	// Max is a number's full scale (255 for linkquality), brought to 0-100; zero
	// when the number already is a percentage.
	Max int
	// Step is how much a number must move to be announced (zero: Config.Step).
	Step int
	// Momentary marks an action (a panic button, a keypad): every message is an
	// event, there is no state to keep.
	Momentary bool
	// CodeFrom is the field holding the code typed with the action (keypads).
	CodeFrom string
	// Rest is the value the state goes back to without a word from the device, when
	// Zigbee2MQTT says it by itself: a motion sensor tells a movement, and Zigbee2MQTT
	// publishes "no movement" on its own timer, with the last_seen of the movement
	// (see noNews). Empty: only the device moves the state.
	Rest string
}

// ProfileCommand is what a command of the profile sends, and what confirms it.
type ProfileCommand struct {
	// Payload goes to the device's set topic. The string "{duration_s}" anywhere in
	// it is replaced by the command's duration, as a number.
	Payload map[string]any
	// Confirm, when set, is the state the device must report for the command to be
	// done; without it the command is done once sent.
	Confirm *Expect
}

// Expect is a value a capability must reach.
type Expect struct{ Capability, Value string }

func (r Rule) numeric() bool { return len(r.Map) == 0 }

// read turns a raw Zigbee2MQTT value into ours.
func (r Rule) read(raw any) (string, bool) {
	if !r.numeric() {
		v, ok := r.Map[text(raw)]
		return v, ok && v != ""
	}
	f, ok := raw.(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return "", false
	}
	if r.Max > 0 {
		f = f * 100 / float64(r.Max)
	}
	n := int(math.Round(f))
	return strconv.Itoa(min(max(n, 0), 100)), true
}

// produces tells whether read can return v. A command waiting for a value its rule
// never gives would fail every time, after sending again and again.
func (r Rule) produces(v string) bool {
	if !r.numeric() {
		return v != "" && slices.Contains(slices.Collect(maps.Values(r.Map)), v)
	}
	n, err := strconv.Atoi(v)
	return err == nil && n >= 0 && n <= 100 && strconv.Itoa(n) == v
}

// moved tells whether a new value is worth announcing.
func (r Rule) moved(prev, next string, step int) bool {
	if !r.numeric() {
		return prev != next
	}
	if r.Step > 0 {
		step = r.Step
	}
	a, errA := strconv.Atoi(prev)
	b, errB := strconv.Atoi(next)
	if errA != nil || errB != nil {
		return prev != next
	}
	d := a - b
	return d >= step || -d >= step
}

// text writes a raw value the way profiles name it.
func text(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// payload returns the command's payload with the duration put in.
func (c ProfileCommand) payload(durationS int) map[string]any {
	out, _ := fill(c.Payload, durationS).(map[string]any)
	return out
}

func fill(v any, durationS int) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = fill(e, durationS)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = fill(e, durationS)
		}
		return out
	case string:
		if x == "{duration_s}" {
			return durationS
		}
	}
	return v
}

// capabilities lists what the device is, sorted: what the profile reads, and what
// its commands drive ("siren.on" makes it a siren). A siren that reports nothing
// when it sounds is still a siren.
func (p Profile) capabilities() []string {
	caps := maps.Clone(p.Capabilities)
	for name := range p.Commands {
		if c, verb, _ := strings.Cut(name, "."); verb == "on" || verb == "off" {
			caps[c] = Rule{}
		}
	}
	return slices.Sorted(maps.Keys(caps))
}

// carriesCode tells whether the device's messages carry an access code: one of its
// rules reads a code (a keypad). Derived from the rules, not declared apart, so the
// two cannot disagree.
func (p Profile) carriesCode() bool {
	for _, r := range p.Capabilities {
		if r.CodeFrom != "" {
			return true
		}
	}
	return false
}

func (p Profile) validate() error {
	switch {
	case p.Name == "":
		return fmt.Errorf("zigbee: a profile has no name")
	case len(p.Models) == 0:
		return fmt.Errorf("zigbee: profile %s applies to no model", p.Name)
	case len(p.Capabilities) == 0:
		return fmt.Errorf("zigbee: profile %s reads nothing", p.Name)
	case p.OfflineAfter < 0:
		return fmt.Errorf("zigbee: profile %s: negative offline delay", p.Name)
	}
	for c, r := range p.Capabilities {
		if err := r.validate(c); err != nil {
			return fmt.Errorf("zigbee: profile %s: %w", p.Name, err)
		}
	}
	for name, cmd := range p.Commands {
		if err := p.validateCommand(name, cmd); err != nil {
			return fmt.Errorf("zigbee: profile %s: %w", p.Name, err)
		}
	}
	return nil
}

func (r Rule) validate(capability string) error {
	switch {
	case capability == "" || r.From == "":
		return fmt.Errorf("a capability and its field are required")
	case r.Momentary && r.numeric():
		return fmt.Errorf("%s is an action, it needs a map of values", capability)
	case r.CodeFrom != "" && !r.Momentary:
		return fmt.Errorf("%s carries a code, it must be an action", capability)
	case r.Max < 0 || r.Step < 0:
		return fmt.Errorf("%s has a negative scale or step", capability)
	case r.Rest != "" && (r.Momentary || !r.produces(r.Rest)):
		return fmt.Errorf("%s rests at %q, which its rule never reads as a state", capability, r.Rest)
	}
	for raw, v := range r.Map {
		if v == "" {
			return fmt.Errorf("%s maps %q to nothing", capability, raw)
		}
	}
	return nil
}

func (p Profile) validateCommand(name string, cmd ProfileCommand) error {
	if name == "" || len(cmd.Payload) == 0 {
		return fmt.Errorf("command %q sends nothing", name)
	}
	e := cmd.Confirm
	if e == nil {
		return nil
	}
	r, ok := p.Capabilities[e.Capability]
	if !ok || r.Momentary {
		return fmt.Errorf("command %s waits for %s, which the profile does not keep", name, e.Capability)
	}
	if !r.produces(e.Value) {
		return fmt.Errorf("command %s waits for %s %q, which the profile never reads", name, e.Capability, e.Value)
	}
	return nil
}
