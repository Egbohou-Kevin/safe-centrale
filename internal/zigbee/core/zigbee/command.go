package zigbee

import (
	"maps"
	"slices"
	"time"
)

func (n *Network) command(c Command) []Action {
	d, ok := n.s.Devices[c.DeviceID]
	if !ok {
		return []Action{Ack{Ref: c.Ref, DeviceID: c.DeviceID, Status: AckFailed, Reason: ReasonUnknownDevice}}
	}
	if !c.Exp.IsZero() && !c.Now.Before(c.Exp) {
		return []Action{Ack{Ref: c.Ref, DeviceID: d.ID, Status: AckExpired}}
	}
	spec, ok := n.byName[d.Profile].Commands[c.Name]
	if !ok {
		return []Action{Ack{Ref: c.Ref, DeviceID: d.ID, Status: AckFailed, Reason: ReasonUnsupported}}
	}
	var out []Action
	if _, ok := n.s.Pending[d.ID]; ok {
		out = append(out, n.finish(d.ID, AckFailed, ReasonSuperseded))
	}
	out = append(out,
		Send{IEEE: d.IEEE, Payload: spec.payload(c.DurationS)},
		Ack{Ref: c.Ref, DeviceID: d.ID, Status: AckAccepted})
	if spec.Confirm == nil {
		return append(out, Ack{Ref: c.Ref, DeviceID: d.ID, Status: AckDone})
	}
	n.s.Pending[d.ID] = Pending{
		Ref: c.Ref, Name: c.Name, DurationS: c.DurationS, Attempt: 1,
		Deadline: c.Now.Add(n.cfg.ConfirmWait), Exp: c.Exp,
	}
	return out
}

// retryPending sends again the commands not confirmed in time, or gives up on them.
func (n *Network) retryPending(now time.Time) []Action {
	var out []Action
	for _, id := range slices.Sorted(maps.Keys(n.s.Pending)) {
		p := n.s.Pending[id]
		if now.Before(p.Deadline) {
			continue
		}
		d := n.s.Devices[id]
		spec, known := n.byName[d.Profile].Commands[p.Name]
		switch {
		case !p.Exp.IsZero() && !now.Before(p.Exp):
			out = append(out, n.finish(id, AckExpired, ""))
		case !known:
			// The profile changed since the state was saved: sending an empty
			// payload would only hide it.
			out = append(out, n.finish(id, AckFailed, ReasonUnsupported))
		case p.Attempt < n.cfg.Attempts:
			p.Attempt++
			p.Deadline = now.Add(n.cfg.ConfirmWait)
			n.s.Pending[id] = p
			out = append(out, Send{IEEE: d.IEEE, Payload: spec.payload(p.DurationS)})
		default:
			out = append(out, n.finish(id, AckFailed, ReasonNoConfirmation))
		}
	}
	return out
}

// finish drops the command waiting on a device and answers it.
func (n *Network) finish(id, status, reason string) Ack {
	p := n.s.Pending[id]
	delete(n.s.Pending, id)
	return Ack{Ref: p.Ref, DeviceID: id, Status: status, Reason: reason}
}
