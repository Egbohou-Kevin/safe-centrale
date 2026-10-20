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
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"safehome/internal/contract"
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

// Snapshot is the relay's state, saved after every change: the queue survives a
// restart of the agent and of the hub.
type Snapshot struct {
	Queue     []Item                  `json:"queue"`
	Pending   map[string]pendingAlert `json:"pending"`   // alert.request id -> its state
	Incidents map[string]time.Time    `json:"incidents"` // incidents seen going up -> last seen
	Seen      []string                `json:"seen"`      // ids of the last messages from the cloud
	Dropped   int                     `json:"dropped"`   // items dropped since the queue started
	Tokens    float64                 `json:"tokens"`    // the cloud's allowance of messages
	TokensAt  time.Time               `json:"tokens_at"`
	Photos    map[string]photo        `json:"photos,omitempty"` // photo id -> its upload
	Uplink    uint64                  `json:"uplink,omitempty"` // the last number given to a message for the cloud
}

// Relay is the agent's core.
type Relay struct {
	cfg       Config
	s         Snapshot
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
	s.Queue = slices.Clone(s.Queue)
	s.Seen = slices.Clone(s.Seen)
	s.Pending = clone(s.Pending)
	s.Incidents = clone(s.Incidents)
	s.Photos = clone(s.Photos)
	return s
}

func clone[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Restore puts a saved state back. The link is down until told otherwise.
func (r *Relay) Restore(s Snapshot) {
	if s.Pending == nil {
		s.Pending = map[string]pendingAlert{}
	}
	if s.Incidents == nil {
		s.Incidents = map[string]time.Time{}
	}
	if s.Photos == nil {
		s.Photos = map[string]photo{}
	}
	if s.TokensAt.IsZero() {
		s.Tokens = float64(r.cfg.DownBurst)
	}
	r.s = s
	r.up, r.transport, r.sending = false, "", ""
	r.journal, r.journalBytes, r.replay = nil, 0, nil
	r.got = map[string]sentMark{}
}

// Next returns the item to send now: alarm and alert messages first, then device
// events, then states, each in order of arrival, then what the cloud asked for again.
// An item gets its number for the cloud here, at its first attempt, and keeps it.
func (r *Relay) Next() (Item, bool) {
	for _, p := range []Priority{PriorityAlarm, PriorityNormal, PriorityState} {
		for i := range r.s.Queue {
			if it := &r.s.Queue[i]; it.Prio == p {
				if it.Seq == 0 {
					r.number(it)
				}
				return *it, true
			}
		}
	}
	if len(r.replay) > 0 {
		return r.replay[0], true
	}
	return Item{}, false
}

// Up says whether the cloud can be reached.
func (r *Relay) Up() bool { return r.up }

// Status returns the state of the link at now.
func (r *Relay) Status(now time.Time) Status {
	st := Status{Up: r.up, Transport: r.transport, Queued: len(r.s.Queue) + len(r.replay), Dropped: r.s.Dropped}
	for _, it := range r.s.Queue {
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
	for _, at := range r.s.Incidents {
		soonest(at.Add(incidentMemory))
	}
	for _, it := range r.s.Queue {
		soonest(it.At.Add(r.cfg.QueueAge))
	}
	for _, t := range r.photoWakes() {
		soonest(t)
	}
	return wake
}

// Handle processes an input.
func (r *Relay) Handle(in Input) []Action {
	out := r.expire(in.at())
	switch v := in.(type) {
	case Local:
		out = append(out, r.local(v.Now, v.Topic, v.Payload)...)
	case Own:
		out = append(out, r.local(v.Now, v.Topic, v.Payload)...)
	case Cloud:
		out = append(out, r.cloud(v)...)
	case Sent:
		r.sent(v.Now, v.ID)
	case Link:
		out = append(out, r.link(v)...)
	case Uploaded:
		out = append(out, r.uploaded(v)...)
	}
	return append(out, r.uploads(in.at())...)
}

// --- from the hub to the cloud -------------------------------------------------

// local takes a message of the hub's broker and queues it for the cloud if it goes
// there.
func (r *Relay) local(now time.Time, topic string, payload []byte) []Action {
	site, parts, ok := contract.ParseTopic(topic)
	if !ok || site != r.cfg.Site {
		return nil
	}
	route, ok := upRoutes[routeKey(parts)]
	if !ok {
		return nil
	}
	env, err := contract.ParseEnvelope(payload)
	if err != nil || env.Site != r.cfg.Site || !slices.Contains(route.types, env.Type) {
		return nil // the components that read it log what is wrong with it
	}
	if route.device {
		d, err := contract.DecodeData[contract.DeviceEventData](env)
		if err != nil || !slices.Contains(upCapabilities, d.Capability) || d.Code != "" {
			return nil
		}
	}
	if slices.ContainsFunc(r.s.Queue, func(it Item) bool { return it.ID == env.ID }) {
		return nil // the same message again (QoS 1)
	}
	if inc := incidentOf(env); inc != "" {
		r.s.Incidents[inc] = now
	}

	if env.Type == contract.TypeAlertRequest {
		if d, err := contract.DecodeData[contract.AlertRequestData](env); err == nil && cloudChannel(d.Channel) {
			if !r.up {
				// The hub would wait for a confirmation that cannot come: tell it now,
				// its escalation goes to the GSM and sends this again when the link
				// returns.
				return []Action{FailAlert{Ref: env.ID}}
			}
			r.s.Pending[env.ID] = pendingAlert{Deadline: now.Add(r.cfg.ConfirmWait), Forget: now.Add(alertMemory), Incident: d.Incident}
		}
	}

	it := Item{ID: env.ID, Topic: topic, Payload: json.RawMessage(payload), Prio: route.prio, Retain: route.retain, At: now}
	if route.retain || env.Type == contract.TypeHeartbeat {
		if r.thinned(now, env, topic) {
			return nil // economy: the cloud got one a moment ago
		}
		it.Key = topic
		// An older one may have been tried without an acknowledgement: the newer takes
		// its number, so that replacing it leaves no hole in the cloud.
		for _, q := range r.s.Queue {
			if q.Key == it.Key && q.Seq != 0 {
				stamp(&it, q.Seq)
			}
		}
		r.s.Queue = slices.DeleteFunc(r.s.Queue, func(q Item) bool { return q.Key == it.Key })
	}
	r.s.Queue = append(r.s.Queue, it)
	out := r.bound()
	if env.Type == contract.TypePhotoTaken {
		out = append(out, r.photoTaken(now, env)...)
	}
	return out
}

func cloudChannel(ch string) bool { return ch == contract.ChannelApp || ch == contract.ChannelWhatsApp }

// incidentOf reads the incident a message is about, if any.
func incidentOf(env contract.Envelope) string {
	var d struct {
		Incident string `json:"incident"`
	}
	if json.Unmarshal(env.Data, &d) != nil {
		return ""
	}
	return d.Incident
}

// bound keeps the queue within its limits: it drops the least important items
// first, the oldest of them first.
func (r *Relay) bound() []Action {
	size := 0
	for _, it := range r.s.Queue {
		size += len(it.Payload)
	}
	dropped := 0
	var out []Action
	for len(r.s.Queue) > r.cfg.QueueItems || (size > r.cfg.QueueBytes && len(r.s.Queue) > 1) {
		victim := r.victim()
		id := r.s.Queue[victim].ID
		size -= len(r.s.Queue[victim].Payload)
		r.s.Queue = slices.Delete(r.s.Queue, victim, victim+1)
		dropped++
		if _, ok := r.s.Pending[id]; ok {
			delete(r.s.Pending, id) // an alert that will not reach the cloud: the hub must know
			out = append(out, FailAlert{Ref: id})
		}
	}
	if dropped == 0 {
		return out
	}
	r.s.Dropped += dropped
	return append(out, Dropped{Count: dropped, Reason: "queue full"})
}

// victim is the index of the item to drop: the oldest of the lowest priority.
func (r *Relay) victim() int {
	for _, p := range []Priority{PriorityState, PriorityNormal, PriorityAlarm} {
		for i, it := range r.s.Queue {
			if it.Prio == p {
				return i
			}
		}
	}
	return 0
}

// --- the link --------------------------------------------------------------------

func (r *Relay) link(l Link) []Action {
	if l.Up == r.up && l.Transport == r.transport {
		return nil
	}
	r.up = l.Up
	r.transport = ""
	if l.Up {
		r.transport = l.Transport
	}
	out := []Action{}
	if !l.Up {
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
		delete(r.s.Pending, ref)
		r.s.Queue = slices.DeleteFunc(r.s.Queue, func(it Item) bool { return it.ID == ref })
		out = append(out, FailAlert{Ref: ref})
	}
	return out
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
	for inc, at := range r.s.Incidents {
		if !now.Before(at.Add(incidentMemory)) {
			delete(r.s.Incidents, inc)
		}
	}
	n := len(r.s.Queue)
	r.s.Queue = slices.DeleteFunc(r.s.Queue, func(it Item) bool { return !now.Before(it.At.Add(r.cfg.QueueAge)) })
	if old := n - len(r.s.Queue); old > 0 {
		r.s.Dropped += old
		out = append(out, Dropped{Count: old, Reason: "too old"})
	}
	return append(out, r.expirePhotos(now)...)
}

// --- from the cloud to the hub ---------------------------------------------------

// cloud checks a message from the cloud and passes it to the hub if it may go.
func (r *Relay) cloud(c Cloud) []Action {
	refuse := func(reason string) []Action { return []Action{Refused{Topic: c.Topic, Reason: reason}} }
	site, parts, ok := contract.ParseTopic(c.Topic)
	if !ok || site != r.cfg.Site {
		return refuse("not a topic of this site")
	}
	types, ok := downRoutes[routeKey(parts)]
	if !ok {
		return refuse("the cloud may not publish there")
	}
	env, err := contract.ParseEnvelope(c.Payload)
	switch {
	case err != nil:
		return refuse("invalid message: " + err.Error())
	case env.Site != r.cfg.Site:
		return refuse("message for another site")
	case !slices.Contains(types, env.Type):
		return refuse("wrong type for the topic: " + env.Type)
	case slices.Contains(r.s.Seen, env.ID):
		return nil // a duplicate: already passed on
	case !r.allow(c.Now):
		return refuse("too many messages from the cloud")
	}
	if reason := r.check(c.Now, env); reason != "" {
		return refuse(reason)
	}
	r.s.Seen = append(r.s.Seen, env.ID)
	if over := len(r.s.Seen) - r.cfg.SeenSize; over > 0 {
		r.s.Seen = slices.Delete(r.s.Seen, 0, over)
	}
	switch env.Type {
	case contract.TypePhotoUpload:
		return nil // for the agent itself: its address never reaches the hub
	case contract.TypeAgentReplay:
		d, _ := contract.DecodeData[contract.ReplayData](env) // checked above
		return r.replayRange(d)
	}
	return []Action{PublishLocal{Topic: c.Topic, Payload: c.Payload}}
}

// check applies the rules of each type. It returns why the message is refused, or
// "".
func (r *Relay) check(now time.Time, env contract.Envelope) string {
	expired := func(exp *contract.Timestamp) bool { return exp != nil && !now.Before(exp.Time) }
	switch env.Type {
	case contract.TypeAlarmCommand:
		d, err := contract.DecodeData[contract.AlarmCommandData](env)
		switch {
		case err != nil:
			return err.Error()
		case d.Origin != contract.OriginApp:
			return "an order from the cloud must come from the app, not " + d.Origin
		case expired(d.Exp):
			return "expired order"
		}
	case contract.TypeVideoRequest:
		d, err := contract.DecodeData[contract.VideoRequestData](env)
		switch {
		case err != nil:
			return err.Error()
		case d.Validate() != nil:
			return d.Validate().Error()
		case expired(&d.Exp):
			return "expired order"
		}
	case contract.TypePhotoUpload:
		return r.photoUpload(now, env)
	case contract.TypeMaintOpen:
		d, err := contract.DecodeData[contract.MaintOpenData](env)
		switch {
		case err != nil:
			return err.Error()
		case d.Validate() != nil:
			return d.Validate().Error()
		case expired(&d.Exp):
			return "expired order"
		}
	case contract.TypeMaintClose:
		d, err := contract.DecodeData[contract.MaintCloseData](env)
		switch {
		case err != nil:
			return err.Error()
		case d.Validate() != nil:
			return d.Validate().Error()
		}
	case contract.TypeAgentReplay:
		d, err := contract.DecodeData[contract.ReplayData](env)
		switch {
		case err != nil:
			return err.Error()
		case d.Validate() != nil:
			return d.Validate().Error()
		}
	case contract.TypeConfigSet:
		d, err := contract.DecodeData[contract.ConfigSetData](env)
		switch {
		case err != nil:
			return err.Error()
		case d.Validate() != nil:
			return d.Validate().Error()
		}
	case contract.TypeAlertResult:
		d, err := contract.DecodeData[contract.AlertResultData](env)
		if err != nil {
			return err.Error()
		}
		p, ok := r.s.Pending[d.Ref]
		if !ok {
			return "result for an alert the agent did not send to the cloud"
		}
		switch d.Status {
		case contract.ResultQueued, contract.ResultSent:
			p.Confirmed, p.Deadline = true, time.Time{}
			r.s.Pending[d.Ref] = p
		case contract.ResultDelivered, contract.ResultFailed:
			delete(r.s.Pending, d.Ref) // the last word on it
		default:
			return "unknown status " + d.Status
		}
	case contract.TypeAlertAck:
		d, err := contract.DecodeData[contract.AlertAckData](env)
		switch {
		case err != nil:
			return err.Error()
		case d.Via != contract.ChannelApp:
			return "an acknowledgement from the cloud comes from the app, not " + d.Via
		case d.Incident == "" || r.s.Incidents[d.Incident].IsZero():
			return "acknowledgement of an incident the cloud was not told about"
		}
	}
	return ""
}

// allow takes one message from the cloud's allowance, refilled DownPerMinute per
// minute up to DownBurst.
func (r *Relay) allow(now time.Time) bool {
	if !r.s.TokensAt.IsZero() && now.After(r.s.TokensAt) {
		r.s.Tokens += now.Sub(r.s.TokensAt).Minutes() * float64(r.cfg.DownPerMinute)
	}
	r.s.Tokens = min(r.s.Tokens, float64(r.cfg.DownBurst))
	if r.s.TokensAt.IsZero() || now.After(r.s.TokensAt) {
		r.s.TokensAt = now
	}
	if r.s.Tokens < 1 {
		return false
	}
	r.s.Tokens--
	return true
}
