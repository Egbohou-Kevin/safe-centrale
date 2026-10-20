package gsm

import (
	"maps"
	"slices"
	"time"
)

// Snapshot is the full state, saved by the driver after every change: a restart
// loses no request (docs/gsm-gateway.md, "File d'envoi").
type Snapshot struct {
	NextJob  uint64               `json:"next_job"`
	Jobs     []Job                `json:"jobs"`
	Seen     []string             `json:"seen"` // the latest request ids, oldest first
	Lines    map[string]LineState `json:"lines"`
	LastGood string               `json:"last_good"` // the line that sent last
	// Pending matches delivery reports (line/reference) to requests.
	Pending  map[string]string   `json:"pending"`
	Order    []string            `json:"order"` // Pending keys, oldest first
	Open     map[string]OpenItem `json:"open"`  // contact -> the incident last sent to them
	NextTest time.Time           `json:"next_test"`
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
	Text      string    `json:"text"`
	Received  time.Time `json:"received"`
	Attempts  int       `json:"attempts"`   // full rounds over the lines that failed
	NotBefore time.Time `json:"not_before"` // waiting before the next round
	Tried     []string  `json:"tried"`      // lines tried in the current round
	Line      string    `json:"line"`       // the line sending it now, if any
	Deadline  time.Time `json:"deadline"`   // when that line is given up
	Reported  bool      `json:"reported"`   // a failure was already reported
	Test      bool      `json:"test"`
}

// LineState is what the gateway knows of a line.
type LineState struct {
	Operator   string    `json:"operator"`
	Responding bool      `json:"responding"`
	Registered bool      `json:"registered"`
	Signal     int       `json:"signal"`
	Credit     string    `json:"credit"`
	LastTest   time.Time `json:"last_test"`
	LastTestOK *bool     `json:"last_test_ok"`
	Restarts   int       `json:"restarts"`
	Misses     int       `json:"misses"`
	Busy       string    `json:"busy"` // "", busyJob, busyCheck or busyRestart
	BusyUntil  time.Time `json:"busy_until"`
	NextCheck  time.Time `json:"next_check"`
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

// OpenItem is the incident a contact was last told about.
type OpenItem struct {
	Incident string    `json:"incident"`
	At       time.Time `json:"at"`
}

const (
	seenSize    = 512
	pendingSize = 256
)

func (s Snapshot) clone() Snapshot {
	out := s
	out.Jobs = make([]Job, len(s.Jobs))
	for i, j := range s.Jobs {
		j.Tried = slices.Clone(j.Tried)
		out.Jobs[i] = j
	}
	out.Seen = slices.Clone(s.Seen)
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
	return out
}

func cloneBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}
