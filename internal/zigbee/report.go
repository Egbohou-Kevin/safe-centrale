package zigbee

// report reads a device's message. Only changes are announced: Zigbee2MQTT repeats
// the whole state in every message. The first value of a state is recorded without
// an event (nothing changed that anyone saw), except for numbers, which the
// supervision needs to know. Actions are announced every time.
func (n *Network) report(r Report) []Action {
	d, ok := n.resolve(r.Name)
	if !ok {
		return []Action{Ignored{What: "a message from a device that is not registered"}}
	}
	p, ok := n.byName[d.Profile]
	if !ok {
		return []Action{Ignored{What: "a message from a device whose profile is gone: " + d.Profile}}
	}
	if r.Retained {
		return n.readFields(d.ID, p, r) // old news: the device is not alive for it
	}
	out := n.heard(d.ID, r.Now)
	out = append(out, n.readFields(d.ID, p, r)...)
	return append(out, n.confirm(d.ID, p, r)...)
}

// readFields turns the fields of a message into changes, capability by capability.
func (n *Network) readFields(id string, p Profile, r Report) []Action {
	var out []Action
	for _, c := range p.capabilities() {
		rule := p.Capabilities[c]
		raw, present := r.Fields[rule.From]
		if !present {
			continue
		}
		v, ok := rule.read(raw)
		if !ok {
			continue
		}
		if rule.Momentary {
			if !r.Retained {
				out = append(out, momentary(id, c, v, rule, r))
			}
			continue
		}
		if ch, moved := n.setState(id, c, v, rule, r); moved {
			out = append(out, ch)
		}
	}
	return out
}

// momentary announces an action (a button, a keypad): an event every time, no state.
func momentary(id, capability, v string, rule Rule, r Report) Changed {
	ch := Changed{DeviceID: id, Capability: capability, Value: v, Since: r.Now, Notify: true}
	if rule.CodeFrom != "" {
		ch.Code = text(r.Fields[rule.CodeFrom])
	}
	return ch
}

// setState records a new value of a state when it moved enough to be announced.
func (n *Network) setState(id, capability, v string, rule Rule, r Report) (Changed, bool) {
	prev, known := n.s.States[id][capability]
	if known && !rule.moved(prev.Value, v, n.cfg.Step) {
		return Changed{}, false
	}
	if n.s.States[id] == nil {
		n.s.States[id] = map[string]State{}
	}
	n.s.States[id][capability] = State{Value: v, Since: r.Now}
	return Changed{
		DeviceID: id, Capability: capability, Value: v, Numeric: rule.numeric(), Since: r.Now,
		State: true, Notify: !r.Retained && (known || rule.numeric()),
	}, true
}

// confirm completes the command waiting on the device if the message reports the
// state it waits for, changed or not. The value is read from this message, not from
// the state kept: a field that cannot be read confirms nothing, and a number that
// moved too little to be announced still counts.
func (n *Network) confirm(id string, p Profile, r Report) []Action {
	pend, ok := n.s.Pending[id]
	if !ok {
		return nil
	}
	e := p.Commands[pend.Name].Confirm
	if e == nil {
		return nil
	}
	rule := p.Capabilities[e.Capability]
	raw, present := r.Fields[rule.From]
	if !present {
		return nil
	}
	if v, ok := rule.read(raw); !ok || v != e.Value {
		return nil
	}
	return []Action{n.finish(id, AckDone, "")}
}
