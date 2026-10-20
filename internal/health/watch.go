package health

import "time"

// LinkState is what the monitor knows of a component that reports its own state.
type LinkState string

const (
	LinkUnknown LinkState = ""       // it never spoke: none on this site, or not yet
	LinkOK      LinkState = "ok"     // it works
	LinkDown    LinkState = "down"   // it says it does not work
	LinkSilent  LinkState = "silent" // it stopped giving news
)

// Watch follows a component that repeats its state (the GSM gateway, the Zigbee
// adapter): when it wrote its last state (zero: it never spoke), what it said, and
// whether it has been silent too long since. A dead or stuck component cannot say
// so itself: its silence does.
type Watch struct {
	Seen   time.Time `json:"seen"`
	Usable bool      `json:"usable"`
	Silent bool      `json:"silent"`
	// Down since when (zero while it works), and whether the owner was told for this
	// outage.
	DownSince time.Time `json:"down_since"`
	Alerted   bool      `json:"alerted"`
}

type watchRule struct {
	silentAfter time.Duration // silence after which the last state stops counting
	alertAfter  time.Duration // how long an outage lasts before the owner is told
	kind        AlertKind     // empty: nobody is told
}

// report records a state. A state from the future (a clock set back) counts as
// written now. A state older than the last one (a retained copy delivered late)
// is ignored, unless the last one is itself in the future: the clock was set back
// since, and refusing every new state would make the component look silent.
func (w *Watch) report(now, seen time.Time, usable bool) {
	if seen.IsZero() || seen.After(now) {
		seen = now
	}
	if seen.Before(w.Seen) && !w.Seen.After(now) {
		return
	}
	w.Seen, w.Usable = seen, usable
}

// check follows the component after every input: it works while its last state says
// so and is recent enough. An outage starts when it said it did not work, or when
// its silence became too long. It returns true once per outage, when the outage has
// lasted long enough to tell the owner.
func (w *Watch) check(now time.Time, r watchRule) bool {
	if w.Seen.IsZero() {
		return false
	}
	silentAt := w.Seen.Add(r.silentAfter)
	w.Silent = !now.Before(silentAt)
	switch {
	case w.Usable && !w.Silent:
		w.DownSince, w.Alerted = time.Time{}, false
		return false
	case !w.DownSince.IsZero():
	case w.Usable:
		w.DownSince = silentAt
	default:
		w.DownSince = w.Seen
	}
	if r.kind == "" || w.Alerted || now.Sub(w.DownSince) < r.alertAfter {
		return false
	}
	w.Alerted = true
	return true
}

// nextWake is when the component needs a look: when its silence becomes too long
// (a component that said it is down turns silent too, and the published state must
// say so), or when its alert is due; zero if never.
func (w *Watch) nextWake(r watchRule) time.Time {
	if w.Seen.IsZero() {
		return time.Time{}
	}
	var wake time.Time
	if !w.Silent {
		wake = w.Seen.Add(r.silentAfter)
	}
	down := !w.Usable || w.Silent
	if down && !w.Alerted && r.kind != "" {
		if due := w.DownSince.Add(r.alertAfter); wake.IsZero() || due.Before(wake) {
			wake = due
		}
	}
	return wake
}

func (w *Watch) state() LinkState {
	switch {
	case w.Seen.IsZero():
		return LinkUnknown
	case w.Silent:
		return LinkSilent
	case w.Usable:
		return LinkOK
	}
	return LinkDown
}
