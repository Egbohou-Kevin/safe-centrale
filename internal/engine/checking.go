package engine

import (
	"maps"
	"time"
)

// CodeCheckHold is how long the end of an entry delay waits for the check of a code
// typed in its last moments (see CodeEntered). The hub checks every stored code with a
// deliberately slow hash, about a second each on its board, one order at a time: a
// right code typed a second before the end would otherwise reach the engine after it,
// and set off the siren and the alerts for nothing. It is not a setting: it only has
// to cover the check, and it bounds what a stream of wrong codes can add to the delay.
const CodeCheckHold = 10 * time.Second

// codeEntered records a code being checked.
func (e *Engine) codeEntered(c CodeEntered) {
	if e.s.Checking == nil {
		e.s.Checking = map[string]time.Time{}
	}
	e.s.Checking[c.Ref] = c.Now
}

// checked forgets the code of an order that arrived (ref, which may be empty), and
// the codes typed long enough ago that they hold nothing any more: their check ended
// without an order reaching the engine (the installer code, an order the hub dropped).
func (e *Engine) checked(ref string, now time.Time) {
	delete(e.s.Checking, ref)
	maps.DeleteFunc(e.s.Checking, func(_ string, at time.Time) bool { return !now.Before(at.Add(2 * CodeCheckHold)) })
	if len(e.s.Checking) == 0 {
		e.s.Checking = nil
	}
}

// due is when the current deadline falls due: the deadline itself, except at the end
// of an entry delay while a code typed shortly before it is still being checked. That
// end then waits for the order, CodeCheckHold at most.
func (e *Engine) due() time.Time {
	d := e.s.Deadline
	if e.s.Mode != ModeEntryDelay || d.IsZero() {
		return d
	}
	for _, at := range e.s.Checking {
		if !at.After(d) && !at.Before(d.Add(-CodeCheckHold)) {
			return d.Add(CodeCheckHold)
		}
	}
	return d
}
