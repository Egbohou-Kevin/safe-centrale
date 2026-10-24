package zigbee

import "time"

// PermitJoinMax is the longest one request can open the network for: Zigbee's
// permit-join duration is one byte, 255 meaning "until closed", and Zigbee2MQTT
// refuses more than 254 s. A longer pairing window is opened in pieces, each asked
// renewBefore the previous one ends, so that the network never closes in between.
const (
	PermitJoinMax = 254 * time.Second
	renewBefore   = 10 * time.Second
)

func (n *Network) pair(p Pair) []Action {
	if p.For <= 0 {
		n.closePairing()
		return []Action{PermitJoin{}}
	}
	n.s.PairUntil, n.s.PairRef = p.Now.Add(min(p.For, n.cfg.PairMax)), p.Ref
	return []Action{n.permit(p.Now)}
}

// permit opens the network for what is left of the window, PermitJoinMax at most, and
// plans the next request when that is not enough. A second started counts whole: the
// network never closes before the window does.
func (n *Network) permit(now time.Time) PermitJoin {
	left := n.s.PairUntil.Sub(now)
	n.s.PairRenew = time.Time{}
	if left > PermitJoinMax {
		left = PermitJoinMax
		n.s.PairRenew = now.Add(PermitJoinMax - renewBefore)
	}
	return PermitJoin{Seconds: int((left + time.Second - 1) / time.Second)}
}

// pairingDue closes the window at its end, or asks for its next piece.
func (n *Network) pairingDue(now time.Time) []Action {
	switch {
	case n.s.PairUntil.IsZero():
		return nil
	case !now.Before(n.s.PairUntil):
		n.closePairing()
		return []Action{PermitJoin{}}
	case !n.s.PairRenew.IsZero() && !now.Before(n.s.PairRenew):
		return []Action{n.permit(now)}
	}
	return nil
}

func (n *Network) closePairing() {
	n.s.PairUntil, n.s.PairRenew, n.s.PairRef = time.Time{}, time.Time{}, ""
}

// pairRefused ends the window that Zigbee2MQTT would not open (or keep open), and
// says so to whoever asked for it: the installer must not wait in front of a closed
// network. The network is told to close, in case a previous piece is still open.
func (n *Network) pairRefused(r PairRefused) []Action {
	if n.s.PairUntil.IsZero() {
		return []Action{Ignored{What: "a refused pairing request while the network is closed"}}
	}
	ref := n.s.PairRef
	n.closePairing()
	return []Action{PermitJoin{}, PairFailed{Ref: ref, Reason: r.Reason}}
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
