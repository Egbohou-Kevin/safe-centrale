// Package agent is the core of the link agent: what goes from the hub to the cloud,
// in which order, what is kept while the internet is down, and what the cloud may
// send back. It decides nothing about the alarm: if the agent stops, the engine, the
// siren and the GSM backup go on.
//
// Its job is to move contract messages, so unlike the domain cores it knows the
// contract. Like them it does no I/O and never reads the clock: inputs carry their
// time, and the application (internal/agentapp) connects the two brokers.
package agent

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"time"

	"safehome/internal/contract"
	"safehome/internal/sealcode"
)

// Config holds the relay's settings.
type Config struct {
	Site string

	// The queue for the cloud is bounded: when full, the least important and oldest
	// items go first; alarm and alert messages go last. Items older than QueueAge
	// are dropped (nobody needs a week-old health).
	QueueItems int
	QueueBytes int
	QueueAge   time.Duration

	// ConfirmWait is how long the cloud has to confirm an alert for one of its
	// channels (app, WhatsApp) before the hub is told it failed.
	ConfirmWait time.Duration

	// The cloud may send at most DownBurst messages at once, then DownPerMinute per
	// minute: a broken or hostile cloud cannot flood the hub with orders.
	DownBurst     int
	DownPerMinute int

	// SeenSize is how many ids of messages from the cloud are remembered, to drop
	// duplicates.
	SeenSize int

	// The journal keeps in memory what the cloud's broker acknowledged, to send it
	// again if the cloud asks (agent.replay): at most JournalItems messages and
	// JournalBytes, the oldest go first.
	JournalItems int
	JournalBytes int

	// Economy, when set, sends the heartbeat and agent.status to the cloud at most this
	// often (EconomyEvery on mobile data paid by the client); zero sends them all.
	Economy time.Duration

	// Seal opens the access codes the phones sealed for this agent (internal/sealcode): the
	// cloud carries no code in the clear. Without it, an order from the app that needs a
	// code is refused.
	Seal Opener

	// CloudURLs are the cloud's addresses (cloud.urls). A photo goes only to one of
	// their hosts (photo.go); without them, no photo leaves.
	CloudURLs []string
}

// Opener is the agent's key for the sealed codes: *sealcode.Key.
type Opener interface {
	Open(sealed, site, command string, now time.Time) (sealcode.Opened, error)
}

// DefaultConfig returns the defaults of docs/agent.md.
func DefaultConfig(site string) Config {
	return Config{
		Site:       site,
		QueueItems: 20000, QueueBytes: 10 << 20, QueueAge: 7 * 24 * time.Hour,
		ConfirmWait: 10 * time.Second,
		DownBurst:   20, DownPerMinute: 60,
		SeenSize:     2048,
		JournalItems: 5000, JournalBytes: 2 << 20,
	}
}

// Validate checks the settings.
func (c Config) Validate() error {
	switch {
	case !contract.ValidSite(c.Site):
		return fmt.Errorf("agent: invalid site %q", c.Site)
	case c.QueueItems < 1 || c.QueueBytes < 1024 || c.QueueAge <= 0:
		return fmt.Errorf("agent: the queue bounds must be positive (at least 1 KB)")
	case c.ConfirmWait <= 0:
		return fmt.Errorf("agent: ConfirmWait must be positive")
	case c.DownBurst < 1 || c.DownPerMinute < 1:
		return fmt.Errorf("agent: DownBurst and DownPerMinute must be positive")
	case c.SeenSize < 1:
		return fmt.Errorf("agent: SeenSize must be positive")
	case c.JournalItems < 1 || c.JournalBytes < 1024:
		return fmt.Errorf("agent: the journal bounds must be positive (at least 1 KB)")
	case c.Economy < 0:
		return fmt.Errorf("agent: Economy must not be negative")
	}
	for i, raw := range c.CloudURLs {
		if u, err := url.Parse(raw); err != nil || u.Hostname() == "" {
			return fmt.Errorf("agent: CloudURLs[%d] has no host", i)
		}
	}
	return nil
}

// How long the relay remembers what it forwarded, to check what the cloud answers.
const (
	alertMemory    = time.Hour      // an alert request, for its results
	incidentMemory = 24 * time.Hour // an incident, for the acknowledgements from the app
)

// pendingAlert is an alert for a cloud channel, waiting for the cloud's word.
type pendingAlert struct {
	Deadline  time.Time `json:"deadline"` // to confirm it; zero once confirmed
	Forget    time.Time `json:"forget"`   // when to stop accepting results for it
	Incident  string    `json:"incident"`
	Confirmed bool      `json:"confirmed"`
}

// Snapshot is the relay's state, saved after every change. The queue for the cloud
// is saved apart, item by item, and the numbering on its own (see queue.go).
type Snapshot struct {
	Pending map[string]pendingAlert `json:"pending"` // alert.request id -> its state
	// GivenUp holds the alerts the hub was told failed while the cloud may still
	// hold them (alert.request id -> when to forget it): the cloud's word on them,
	// should it come, is late, not unknown.
	GivenUp   map[string]time.Time `json:"given_up,omitempty"`
	Incidents map[string]time.Time `json:"incidents"` // incidents seen going up -> last seen
	Seen      []string             `json:"seen"`      // ids of the last messages from the cloud
	// Seals holds the seals opened (the phone's one-time key -> the phone's time in it),
	// while they still open: apart from Seen, so that a flood of ids cannot push one out
	// and let it be used again (down.go).
	Seals     map[string]time.Time `json:"seals,omitempty"`
	Dropped   int                  `json:"dropped"` // items that left the queue without reaching the cloud
	Tokens    float64              `json:"tokens"`  // the cloud's allowance of messages
	TokensAt  time.Time            `json:"tokens_at"`
	Answers   float64              `json:"answers"` // the allowance of answers to orders over the limit (down.go)
	AnswersAt time.Time            `json:"answers_at"`
	Replies   float64              `json:"replies"` // the allowance of the cloud's replies (down.go)
	RepliesAt time.Time            `json:"replies_at"`
	Photos    map[string]photo     `json:"photos,omitempty"`   // photo id -> its upload
	Retained  map[string]string    `json:"retained,omitempty"` // retained topic -> id of the hub's last state queued (see up.go)
	Move      *moving              `json:"move,omitempty"`     // a move under way (move.go), never its secret
}

// Relay is the agent's core.
type Relay struct {
	cfg       Config
	s         Snapshot
	queue     []Item // saved item by item (see queue.go)
	order     uint64 // the last Order given
	uplink    uint64 // the last number given to a message for the cloud, saved on its own
	up        bool
	transport string
	sending   string // the photo being uploaded: one at a time

	// In memory only (see replay.go): what the cloud's broker took, and what the
	// cloud asked for again.
	journal      []Item
	journalBytes int
	replay       []Item
	// In memory only (see economy.go): when the cloud last got each repeated message.
	got map[string]sentMark
	// In memory only (see restate.go): the hub's latest states said again at each
	// connection, those this connection still owes the cloud, and when each was last
	// said again.
	states   map[string][]byte
	owed     map[string]bool
	restated map[string]time.Time
	// Saved as tombstones in the queue's store (see sent.go): the ids of what
	// reached the cloud lately, oldest first, and the same as a set.
	tombs  []tomb
	buried map[string]bool
}

// New builds a relay with an empty queue.
func New(cfg Config) (*Relay, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	r := &Relay{cfg: cfg}
	r.Restore(Snapshot{})
	return r, nil
}

// Snapshot returns the state to save.
func (r *Relay) Snapshot() Snapshot {
	s := r.s
	s.Seen = slices.Clone(s.Seen)
	s.Pending = maps.Clone(s.Pending)
	s.GivenUp = maps.Clone(s.GivenUp)
	s.Incidents = maps.Clone(s.Incidents)
	s.Photos = maps.Clone(s.Photos)
	s.Retained = maps.Clone(s.Retained)
	s.Seals = maps.Clone(s.Seals)
	s.Move = s.Move.clone()
	return s
}

// Restore puts a saved state back, without the queue: RestoreQueue follows. The link
// is down until told otherwise.
func (r *Relay) Restore(s Snapshot) {
	if s.Pending == nil {
		s.Pending = map[string]pendingAlert{}
	}
	if s.GivenUp == nil {
		s.GivenUp = map[string]time.Time{}
	}
	if s.Incidents == nil {
		s.Incidents = map[string]time.Time{}
	}
	if s.Photos == nil {
		s.Photos = map[string]photo{}
	}
	if s.Retained == nil {
		s.Retained = map[string]string{}
	}
	if s.Seals == nil {
		s.Seals = map[string]time.Time{}
	}
	if s.TokensAt.IsZero() {
		s.Tokens = float64(r.cfg.DownBurst)
	}
	if s.RepliesAt.IsZero() {
		s.Replies = float64(replyAllowance * r.cfg.DownBurst)
	}
	if s.AnswersAt.IsZero() {
		s.Answers = float64(r.cfg.DownBurst)
	}
	r.s = s
	r.queue, r.order, r.uplink = nil, 0, 0
	r.up, r.transport, r.sending = false, "", ""
	r.journal, r.journalBytes, r.replay = nil, 0, nil
	r.got = map[string]sentMark{}
	r.states, r.owed, r.restated = map[string][]byte{}, map[string]bool{}, map[string]time.Time{}
	r.tombs, r.buried = nil, map[string]bool{}
}

// Up says whether the cloud can be reached.
func (r *Relay) Up() bool { return r.up }

// Status returns the state of the link at now.
func (r *Relay) Status(now time.Time) Status {
	st := Status{Up: r.up && !r.Moving(), Transport: r.transport, Queued: len(r.queue) + len(r.replay), Dropped: r.s.Dropped}
	for _, it := range r.queue {
		if age := now.Sub(it.At); age > st.Oldest {
			st.Oldest = age
		}
	}
	return st
}

// NextWake returns when the relay needs a Tick: a confirmation deadline, an item
// growing too old, something to forget; zero if never.
func (r *Relay) NextWake() time.Time {
	var wake time.Time
	soonest := func(t time.Time) {
		if !t.IsZero() && (wake.IsZero() || t.Before(wake)) {
			wake = t
		}
	}
	for _, p := range r.s.Pending {
		if p.Confirmed {
			soonest(p.Forget)
		} else {
			soonest(p.Deadline)
		}
	}
	for _, at := range r.s.GivenUp {
		soonest(at)
	}
	for _, at := range r.s.Incidents {
		soonest(at.Add(incidentMemory))
	}
	for _, it := range r.queue {
		soonest(it.At.Add(r.cfg.QueueAge))
	}
	for _, t := range r.photoWakes() {
		soonest(t)
	}
	if r.s.Move != nil {
		soonest(r.s.Move.GiveUp)
	}
	return wake
}

// Handle processes an input.
func (r *Relay) Handle(in Input) []Action {
	out := r.expire(in.at())
	switch v := in.(type) {
	case Local:
		out = append(out, r.local(v.Now, v.Topic, v.Payload, false)...)
	case Own:
		out = append(out, r.local(v.Now, v.Topic, v.Payload, true)...)
	case Cloud:
		out = append(out, r.cloud(v)...)
	case Sent:
		out = append(out, r.sent(v.Now, v.ID)...)
	case Rejected:
		out = append(out, r.rejected(v.ID)...)
	case Link:
		out = append(out, r.link(v)...)
	case Uploaded:
		out = append(out, r.uploaded(v)...)
	case MoveFailed:
		out = append(out, r.moveFailed(v)...)
	}
	return append(out, r.uploads(in.at())...)
}

// --- the link --------------------------------------------------------------------

func (r *Relay) link(l Link) []Action {
	arrived := l.Up && l.Next && r.Moving()
	if l.Up && l.Next && !arrived {
		return nil // cell B, for a move already ended: the application leaves it
	}
	if !arrived && l.Up == r.up && l.Transport == r.transport {
		return nil
	}
	out := []Action{}
	if arrived {
		out = r.arrived()
	}
	r.up = l.Up
	r.transport = ""
	if l.Up {
		r.transport = l.Transport
	}
	if l.Up {
		out = append(out, r.restate(l.Now)...)
	} else {
		clear(r.owed)
		// The alerts the cloud did not confirm will not be: tell the hub now rather
		// than at their deadline, and do not send them later (the escalation sends
		// them again when the link returns).
		out = append(out, r.failUnconfirmed(func(pendingAlert) bool { return true })...)
	}
	return append(out, StatusChanged{Status: r.Status(l.Now)})
}

// failUnconfirmed fails the unconfirmed alerts that match, and takes them out of the
// queue if they are still there.
func (r *Relay) failUnconfirmed(match func(pendingAlert) bool) []Action {
	var refs []string
	for ref, p := range r.s.Pending {
		if !p.Confirmed && match(p) {
			refs = append(refs, ref)
		}
	}
	slices.Sort(refs) // the same order every time
	var out []Action
	for _, ref := range refs {
		fail := r.giveUp(ref)
		_, drop := r.removeIf(func(it Item) bool { return it.ID == ref })
		out = append(out, drop...)
		out = append(out, fail)
	}
	return out
}

// giveUp stops following an alert and returns the action that tells the hub it
// failed. The cloud may hold it all the same (its broker keeps a session while the
// cloud restarts): the ref is remembered as long as the alert would have been, so
// that the cloud's word on it is recognised as late, and dropped.
func (r *Relay) giveUp(ref string) Action {
	r.s.GivenUp[ref] = r.s.Pending[ref].Forget
	delete(r.s.Pending, ref)
	return FailAlert{Ref: ref}
}

// expire does what time has made due: unconfirmed alerts fail, items too old leave
// the queue, old memories are forgotten.
func (r *Relay) expire(now time.Time) []Action {
	out := r.failUnconfirmed(func(p pendingAlert) bool { return !now.Before(p.Deadline) })
	for ref, p := range r.s.Pending {
		if p.Confirmed && !now.Before(p.Forget) {
			delete(r.s.Pending, ref)
		}
	}
	for ref, at := range r.s.GivenUp {
		if !now.Before(at) {
			delete(r.s.GivenUp, ref)
		}
	}
	for inc, at := range r.s.Incidents {
		if !now.Before(at.Add(incidentMemory)) {
			delete(r.s.Incidents, inc)
		}
	}
	old, _ := r.removeIf(func(it Item) bool { return !now.Before(it.At.Add(r.cfg.QueueAge)) })
	out = append(out, r.dropped(old, "too old")...)
	out = append(out, r.forgetSent(now)...)
	out = append(out, r.expireMove(now)...)
	return append(out, r.expirePhotos(now)...)
}
