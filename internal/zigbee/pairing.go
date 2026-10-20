package zigbee

import "time"

func (n *Network) pair(p Pair) []Action {
	if p.For <= 0 {
		n.s.PairUntil, n.s.PairRef = time.Time{}, ""
		return []Action{PermitJoin{}}
	}
	d := min(p.For, n.cfg.PairMax)
	n.s.PairUntil, n.s.PairRef = p.Now.Add(d), p.Ref
	return []Action{PermitJoin{Seconds: int(d / time.Second)}}
}

// interviewed decides about a device that joined. Only a model with a profile, during
// a pairing window, is kept: a device we cannot read is a device we cannot watch.
func (n *Network) interviewed(i Interviewed) []Action {
	if _, known := n.byIEEE(i.IEEE); known {
		return []Action{Ignored{What: "a registered device was questioned again"}}
	}
	reject := func(reason, ref string) []Action {
		return []Action{Expel{IEEE: i.IEEE}, Rejected{Ref: ref, Model: i.Model, Reason: reason}}
	}
	p, ok := n.byModel[i.Model]
	switch {
	case n.s.PairUntil.IsZero():
		return reject(RejectNotPairing, "")
	case !i.OK:
		return reject(RejectInterviewFailed, n.s.PairRef)
	case !ok:
		return reject(RejectUnknownModel, n.s.PairRef)
	}
	id := n.freshID()
	if id == "" {
		return append(reject(RejectInterviewFailed, n.s.PairRef), Ignored{What: "no free device id"})
	}
	d := Device{ID: id, IEEE: i.IEEE, Model: i.Model, Profile: p.Name, Capabilities: p.capabilities()}
	n.s.Devices[id] = d
	n.seen[id] = i.Now
	return []Action{Rename{IEEE: i.IEEE, To: id}, Added{Ref: n.s.PairRef, Device: d}, RegistryChanged{Devices: n.Devices()}}
}

// freshID asks newID for an id never given before, a bounded number of times.
func (n *Network) freshID() string {
	for range 100 {
		id := n.newID()
		if id == "" || n.s.Retired[id] {
			continue
		}
		if _, taken := n.s.Devices[id]; !taken {
			return id
		}
	}
	return ""
}

func (n *Network) remove(r Remove) []Action {
	d, ok := n.s.Devices[r.DeviceID]
	if !ok {
		return []Action{Ignored{What: "removal of a device that is not registered"}}
	}
	out := []Action{Expel{IEEE: d.IEEE}}
	if _, ok := n.s.Pending[d.ID]; ok {
		out = append(out, n.finish(d.ID, AckFailed, ReasonRemoved))
	}
	delete(n.s.Devices, d.ID)
	delete(n.s.States, d.ID)
	delete(n.s.Offline, d.ID)
	delete(n.seen, d.ID)
	n.s.Retired[d.ID] = true
	return append(out, Removed{Ref: r.Ref, Device: d}, RegistryChanged{Devices: n.Devices()})
}
