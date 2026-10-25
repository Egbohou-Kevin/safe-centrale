package gsm

import (
	"maps"
	"slices"
	"time"
)

// Snapshot is the full state, saved by the driver after every change: a restart
// loses no request (docs/gsm-gateway.md, "File d'envoi").
//
// What a line is doing now (a job, a check, a restart, and when its next check is
// due) is not saved: it changes twice per check, every 30 seconds, and would wear
// the SD card for nothing, since a restart starts every line afresh (Restore). The
// fields tagged json:"-" are those.
type Snapshot struct {
	NextJob  uint64               `json:"next_job"`
	Jobs     []Job                `json:"jobs"`
	Seen     []string             `json:"seen"` // the latest request ids, oldest first
	Lines    map[string]LineState `json:"lines"`
	LastGood string               `json:"last_good"` // the line that sent last
	// Pending matches delivery reports (line/reference) to requests.
	Pending  map[string]string   `json:"pending"`
	Order    []string            `json:"order"`  // Pending keys, oldest first
	Open     map[string]OpenItem `json:"open"`   // contact -> the incident last sent to them
	Closed   []string            `json:"closed"` // the latest incidents over (Cancel), oldest first
	NextTest time.Time           `json:"next_test"`
	// Told is, for each contact, the incidents sent to them within ackAmbiguity, once
	// each, first told first: the ones a "1" may be meant for (see ackTarget).
	Told map[string][]OpenItem `json:"told,omitempty"`
}

// Job is a request in the queue, or a test SMS.
type Job struct {
	ID        uint64    `json:"id"`
	Ref       string    `json:"ref"` // the request id, empty for a test
	Incident  string    `json:"incident"`
	Contact   string    `json:"contact"`
	Number    string    `json:"number"`
	Channel   Channel   `json:"channel"`
	Priority  Priority  `json:"priority"`
	Secondary bool      `json:"secondary,omitempty"` // see Request.Secondary
	Text      string    `json:"text"`
	Received  time.Time `json:"received"`          // when it was asked, at most when it arrived
	Attempts  int       `json:"attempts"`          // full rounds over the lines that failed
	NotBefore time.Time `json:"not_before"`        // waiting before the next round
	Tried     []string  `json:"tried"`             // lines tried in the current round
	Failure   Reason    `json:"failure,omitempty"` // why the last line tried failed
	Line      string    `json:"-"`                 // the line sending it now, if any
	Deadline  time.Time `json:"-"`                 // when that line is given up
	Reported  bool      `json:"reported"`          // a failure was already reported
	Test      bool      `json:"test"`
}

// LineState is what the gateway knows of a line.
type LineState struct {
	Operator   string    `json:"operator"`
	Responding bool      `json:"responding"`
	Registered bool      `json:"registered"`
	Signal     int       `json:"signal"` // rounded to tens: the exact value changes all the time
	Credit     Credit    `json:"credit"`
	LastTest   time.Time `json:"last_test"`
	LastTestOK *bool     `json:"last_test_ok"`
	Restarts   int       `json:"restarts"`
	Misses     int       `json:"misses"`
	Busy       string    `json:"-"` // "", busyJob, busyCheck or busyRestart
	BusyUntil  time.Time `json:"-"`
	NextCheck  time.Time `json:"-"`
}

const (
	busyJob     = "job"
	busyCheck   = "check"
	busyRestart = "restart"
)

// free ends what the line was doing.
func (l *LineState) free() { l.Busy, l.BusyUntil = "", time.Time{} }

// usable reports whether the line can be given a job.
func (l LineState) usable() bool { return l.Responding && l.Registered }

// OpenItem is an incident a contact was told about, when (last, and in Told first
// too), how urgent, and whether it is secondary to an alarm told alongside it (see
// Request.Secondary).
type OpenItem struct {
	Incident  string    `json:"incident"`
	At        time.Time `json:"at"`
	First     time.Time `json:"first,omitzero"` // zero when saved before it was kept: At
	Priority  Priority  `json:"priority"`
	Secondary bool      `json:"secondary,omitempty"`
}

// first is when the incident was first told to the contact.
func (o OpenItem) first() time.Time {
	if o.First.IsZero() {
		return o.At
	}
	return o.First
}

const (
	seenSize    = 512
	pendingSize = 256
	closedSize  = 64
)

func (s Snapshot) clone() Snapshot {
	out := s
	out.Jobs = make([]Job, len(s.Jobs))
	for i, j := range s.Jobs {
		j.Tried = slices.Clone(j.Tried)
		out.Jobs[i] = j
	}
	out.Seen = slices.Clone(s.Seen)
	out.Closed = slices.Clone(s.Closed)
	out.Lines = make(map[string]LineState, len(s.Lines))
	for id, l := range s.Lines {
		l.LastTestOK = cloneBool(l.LastTestOK)
		out.Lines[id] = l
	}
	out.Pending = maps.Clone(s.Pending)
	if out.Pending == nil {
		out.Pending = map[string]string{}
	}
	out.Order = slices.Clone(s.Order)
	out.Open = maps.Clone(s.Open)
	if out.Open == nil {
		out.Open = map[string]OpenItem{}
	}
	out.Told = make(map[string][]OpenItem, len(s.Told))
	for c, items := range s.Told {
		out.Told[c] = slices.Clone(items)
	}
	return out
}

func cloneBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}
