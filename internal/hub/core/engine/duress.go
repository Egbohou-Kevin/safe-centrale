package engine

// duress raises the alert of a disarm made with a duress code (23 November 2026). The
// disarm itself is the person's own (disarmBy): the same mode, reply and events, so
// that nothing on site shows the threat. The alert has an incident of its own, which
// the engine keeps nowhere: no Ended carries it, no disarm closes it at the centre,
// no siren rings for it, and a restart does not raise it again (the escalation keeps
// it). Silencing or ending with a duress code raises nothing: the person is already
// in charge of an alarm the centre sees.
func (e *Engine) duress(origin Origin, user string) []Action {
	al := e.alert(AlertDuress, nil)
	al.User = user
	return []Action{Emit{DuressRaised{Incident: al.Incident, Origin: origin}}, al}
}
