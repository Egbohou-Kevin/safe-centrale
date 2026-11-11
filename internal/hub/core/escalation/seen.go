package escalation

import "time"

// seen records that a known contact has seen an incident ("Vu" in the app), without
// handling it: the escalation of the incident goes on. Their phone was silenced by
// the app; the escalation silences the rest for them: what their current round still
// owed (the GSM takeover or the double, a held internet message) is dropped, and the
// next steps skip them. A step left with nobody is skipped at once, as an empty one
// is: the others are told sooner. An incident no longer escalated changes nothing.
func (e *Escalation) seen(s Seen) []Action {
	if _, ok := e.cfg.contact(s.Contact); !ok {
		return nil
	}
	var out []Action
	noted := false
	for _, key := range e.keysOf(s.Incident) {
		in := e.s.Incidents[key]
		if _, already := in.Seen[s.Contact]; !already {
			if in.Seen == nil {
				in.Seen = map[string]time.Time{}
			}
			in.Seen[s.Contact] = s.Now
			if !noted {
				out, noted = append(out, Noted{Incident: in.ID, Contact: s.Contact}), true
			}
		}
		if r, ok := in.Rounds[s.Contact]; ok {
			// The app reached them: nothing of this round is owed to them any more.
			r.NetOK, r.NetHeld, r.NetDeadline, r.GSMDue = true, false, time.Time{}, time.Time{}
			in.Rounds[s.Contact] = r
		}
		e.s.Incidents[key] = in
		out = append(out, e.finishIfDone(key)...)
	}
	return out
}

// reachable tells whether a step of the incident still tells a contact: not one who
// said they have seen it, nor one without a channel this site can use (only SMS and
// calls on a site without a GSM line). A step left with nobody is skipped.
func (e *Escalation) reachable(in IncidentSnap, ct Contact) bool {
	_, seen := in.Seen[ct.ID]
	return !seen && len(e.cfg.usable(ct.Channels)) > 0
}
