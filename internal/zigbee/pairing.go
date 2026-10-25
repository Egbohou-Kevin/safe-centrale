package zigbee

import (
	"strconv"
	"time"
)

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
	// A new order is a new window: open once Zigbee2MQTT confirms it, even if the
	// network already is for another order.
	n.closePairing()
	n.s.PairUntil, n.s.PairRef = p.Now.Add(min(p.For, n.cfg.PairMax)), p.Ref
	return []Action{n.permit(p.Now)}
}

// permit asks for the next piece of the window: a new request, with its own
// transaction, whose answer is awaited.
func (n *Network) permit(now time.Time) PermitJoin {
	n.s.PairSeq++
	n.s.PairAsk = Asking{Txn: n.s.PairRef + "." + strconv.Itoa(n.s.PairSeq), Attempt: 1, Deadline: now.Add(n.cfg.ConfirmWait)}
	return n.request(now)
}

// request opens the network for what is left of the window, PermitJoinMax at most,
// and plans the next piece when that is not enough. Each request starts
// Zigbee2MQTT's count again, so the renewal is planned from the last one. A second
// started counts whole: the network never closes before the window does.
func (n *Network) request(now time.Time) PermitJoin {
	left := n.s.PairUntil.Sub(now)
	n.s.PairRenew = time.Time{}
	if left > PermitJoinMax {
		left = PermitJoinMax
		n.s.PairRenew = now.Add(PermitJoinMax - renewBefore)
	}
	return PermitJoin{Seconds: int((left + time.Second - 1) / time.Second), Txn: n.s.PairAsk.Txn}
}

// pairingDue closes the window at its end, asks for its next piece, or asks again
// for a piece Zigbee2MQTT has not answered.
func (n *Network) pairingDue(now time.Time) []Action {
	switch {
	case n.s.PairUntil.IsZero():
		return nil
	case !now.Before(n.s.PairUntil):
		n.closePairing()
		return []Action{PermitJoin{}}
	case !n.s.PairRenew.IsZero() && !now.Before(n.s.PairRenew):
		return []Action{n.permit(now)}
	case n.s.PairAsk.Txn != "" && !now.Before(n.s.PairAsk.Deadline):
		return n.askAgain(now)
	}
	return nil
}

// askAgain sends again a request Zigbee2MQTT has not answered, under the same
// transaction (a late answer to the first still counts), or gives up on the window:
// a Zigbee2MQTT that answers nothing opens nothing, and the installer must not wait
// in front of a closed network.
func (n *Network) askAgain(now time.Time) []Action {
	if n.s.PairAsk.Attempt < n.cfg.Attempts {
		n.s.PairAsk.Attempt++
		n.s.PairAsk.Deadline = now.Add(n.cfg.ConfirmWait)
		return []Action{n.request(now)}
	}
	return n.failPairing(ReasonNoJoinAnswer)
}

func (n *Network) closePairing() {
	n.s.PairUntil, n.s.PairRenew, n.s.PairRef = time.Time{}, time.Time{}, ""
	n.s.PairOpen, n.s.PairAsk, n.s.PairSeq = false, Asking{}, 0
}

// joinAnswered takes Zigbee2MQTT's answer to a request to open the network. Only the
// answer to the request awaited counts: one to an older request (a closing order, a
// window replaced since) says nothing about the network now. A refusal ends the
// window that Zigbee2MQTT would not open (or keep open).
func (n *Network) joinAnswered(a JoinAnswer) []Action {
	if a.Txn == "" || a.Txn != n.s.PairAsk.Txn {
		return []Action{Ignored{What: "an answer to a request to open the network that is no longer awaited"}}
	}
	n.s.PairAsk = Asking{}
	if !a.OK {
		return n.failPairing(a.Reason)
	}
	n.s.PairOpen = true
	return nil
}

// failPairing ends the window and says so to whoever asked for it. The network is
// told to close, in case a previous piece is still open.
func (n *Network) failPairing(reason string) []Action {
	ref := n.s.PairRef
	n.closePairing()
	return []Action{PermitJoin{}, PairFailed{Ref: ref, Reason: reason}}
}

// interviewed decides about a device that joined. Only a model with a profile, that
// entered during a pairing window, is kept: a device we cannot read is a device we
// cannot watch. Its questioning may end after the window (see JoinWait).
func (n *Network) interviewed(i Interviewed) []Action {
	if _, known := n.byIEEE(i.IEEE); known {
		return []Action{Ignored{What: "a registered device was questioned again"}}
	}
	ref, inWindow := n.windowOf(i.IEEE, i.Now)
	reject := func(reason, ref string) []Action {
		return []Action{Expel{IEEE: i.IEEE}, Rejected{Ref: ref, Model: i.Model, Reason: reason}}
	}
	p, ok := n.byModel[i.Model]
	switch {
	case !inWindow:
		return reject(RejectNotPairing, "")
	case !i.OK:
		return reject(RejectInterviewFailed, ref)
	case !ok:
		return reject(RejectUnknownModel, ref)
	}
	id := n.freshID()
	if id == "" {
		return append(reject(RejectInterviewFailed, ref), Ignored{What: "no free device id"})
	}
	d := Device{ID: id, IEEE: i.IEEE, Model: i.Model, Profile: p.Name, Capabilities: p.capabilities()}
	n.s.Devices[id] = d
	n.spokeAt(id, i.Now, i.Now) // joining is news from it
	out := []Action{Rename{IEEE: i.IEEE, To: id}}
	if p.carriesCode() {
		out = append(out, CarriesCode{IEEE: i.IEEE})
	}
	return append(out, Added{Ref: ref, Device: d}, RegistryChanged{Devices: n.Devices()})
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
	delete(n.news, d.ID)
	delete(n.said, d.ID)
	delete(n.s.Heard, d.ID)
	n.s.Retired[d.ID] = true
	return append(out, Removed{Ref: r.Ref, Device: d}, RegistryChanged{Devices: n.Devices()})
}
