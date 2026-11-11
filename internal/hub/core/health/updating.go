package health

import "time"

// --- update announcement ---------------------------------------------------

// updating records an update announcement, or its withdrawal (a zero Until). It never
// lasts more than MaxUpdating after it was made, nor after it was received: a file
// left behind by an update cut short, or a time written wrong, never hides a real
// fault for longer. One already over is none.
func (m *Monitor) updating(u Updating) {
	made := u.Made
	if made.IsZero() || made.After(u.Now) {
		made = u.Now
	}
	until := u.Until
	if limit := made.Add(MaxUpdating); until.After(limit) {
		until = limit
	}
	if !until.After(u.Now) {
		until = time.Time{}
	}
	if !until.IsZero() {
		until = until.UTC()
	}
	m.s.UpdatingUntil = until
}

// endUpdating drops an announcement whose time has come. It changes nothing else:
// the alarm, the alerts and the watches go on as without it.
func (m *Monitor) endUpdating(now time.Time) {
	if !m.s.UpdatingUntil.IsZero() && !now.Before(m.s.UpdatingUntil) {
		m.s.UpdatingUntil = time.Time{}
	}
}
