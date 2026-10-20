package agent

import (
	"maps"
	"slices"
	"time"

	"safehome/internal/contract"
)

// Photos (see docs/agent.md): the video service announces each photo it took at an
// alarm (photo.taken), which goes up like any message. The cloud answers with where
// to send it (photo.upload); the relay keeps that address for the agent alone (it is
// never published on the hub) and has the application send the file, one at a time,
// once nothing of an alarm waits in the queue: the photos follow the alarm's
// messages, never ahead of them. It says how it went (photo.sent).

// photo is a photo waiting for its upload.
type photo struct {
	Taken   time.Time `json:"taken"`              // when it was announced
	URL     string    `json:"url,omitempty"`      // where to send it, once the cloud said
	Token   string    `json:"token,omitempty"`    // for URL; never logged
	Exp     time.Time `json:"exp,omitempty"`      // URL is no good after Exp
	RetryAt time.Time `json:"retry_at,omitempty"` // after a failed attempt
	Tries   int       `json:"tries,omitempty"`
}

const (
	photoAge   = contract.PhotoMaxAge * time.Second
	maxPhotos  = 100              // photos followed at once: the video service keeps fewer files
	retryFirst = 30 * time.Second // after a failed upload, then twice longer each time
	retryMost  = 10 * time.Minute
)

// UploadResult says how an upload went.
type UploadResult int

const (
	UploadOK      UploadResult = iota
	UploadRetry                // the network or the cloud failed: try again later
	UploadExpired              // the address or its token were refused: the cloud may give another
	UploadRefused              // the cloud refused the file itself
	UploadMissing              // the file is no longer on the site
)

// Uploaded says how the application's upload of a photo went.
type Uploaded struct {
	Now    time.Time
	Photo  string
	Result UploadResult
}

func (i Uploaded) at() time.Time { return i.Now }

// Upload: send the photo's file to URL with the token (never log it).
type Upload struct {
	Photo string
	URL   string
	Token string
}

// RemovePhoto: the photo's file is no longer needed.
type RemovePhoto struct{ Photo string }

// PhotoReport: say how the photo went (photo.sent).
type PhotoReport struct {
	Photo  string
	Status string // contract.PhotoSent, contract.PhotoFailed
	Reason string
}

func (Upload) action()      {}
func (RemovePhoto) action() {}
func (PhotoReport) action() {}

// photoTaken follows a photo announced by the video service.
func (r *Relay) photoTaken(now time.Time, env contract.Envelope) []Action {
	d, err := contract.DecodeData[contract.PhotoTakenData](env)
	if err != nil || d.Validate() != nil {
		return nil
	}
	if _, ok := r.s.Photos[d.Photo]; ok {
		return nil
	}
	r.s.Photos[d.Photo] = photo{Taken: now}
	var out []Action
	for len(r.s.Photos) > maxPhotos {
		oldest := r.oldestPhoto(func(string, photo) bool { return true })
		out = append(out, r.dropPhoto(oldest, contract.PhotoMissing)...)
	}
	return out
}

// photoUpload takes the cloud's address for a photo. It returns why it is refused,
// or "".
func (r *Relay) photoUpload(now time.Time, env contract.Envelope) string {
	d, err := contract.DecodeData[contract.PhotoUploadData](env)
	switch {
	case err != nil:
		return err.Error()
	case d.Validate() != nil:
		return d.Validate().Error()
	case !now.Before(d.Exp.Time):
		return "expired order"
	}
	p, ok := r.s.Photos[d.Photo]
	if !ok {
		return "upload of a photo the agent does not know"
	}
	p.URL, p.Token, p.Exp, p.RetryAt, p.Tries = d.URL, d.Token, d.Exp.Time, time.Time{}, 0
	r.s.Photos[d.Photo] = p
	return ""
}

// uploaded takes the application's word on an upload.
func (r *Relay) uploaded(u Uploaded) []Action {
	if r.sending == u.Photo {
		r.sending = ""
	}
	p, ok := r.s.Photos[u.Photo]
	if !ok {
		return nil
	}
	switch u.Result {
	case UploadOK:
		delete(r.s.Photos, u.Photo)
		return []Action{RemovePhoto{Photo: u.Photo}, PhotoReport{Photo: u.Photo, Status: contract.PhotoSent}}
	case UploadExpired:
		return r.lostAddress(u.Photo)
	case UploadRefused:
		return r.dropPhoto(u.Photo, contract.PhotoRefused)
	case UploadMissing:
		return r.dropPhoto(u.Photo, contract.PhotoMissing)
	}
	wait := retryFirst << min(p.Tries, 5)
	p.Tries++
	p.RetryAt = u.Now.Add(min(wait, retryMost))
	r.s.Photos[u.Photo] = p
	return nil
}

// lostAddress forgets a photo's address and says so: the cloud may send another.
func (r *Relay) lostAddress(id string) []Action {
	p := r.s.Photos[id]
	p.URL, p.Token, p.Exp, p.RetryAt, p.Tries = "", "", time.Time{}, time.Time{}, 0
	r.s.Photos[id] = p
	return []Action{PhotoReport{Photo: id, Status: contract.PhotoFailed, Reason: contract.PhotoURLExpired}}
}

func (r *Relay) dropPhoto(id, why string) []Action {
	delete(r.s.Photos, id)
	if r.sending == id {
		r.sending = ""
	}
	return []Action{RemovePhoto{Photo: id}, PhotoReport{Photo: id, Status: contract.PhotoFailed, Reason: why}}
}

// expirePhotos drops the photos that waited too long, and forgets the addresses that
// are no good any more.
func (r *Relay) expirePhotos(now time.Time) []Action {
	var out []Action
	for _, id := range slices.Sorted(maps.Keys(r.s.Photos)) {
		p := r.s.Photos[id]
		switch {
		case !now.Before(p.Taken.Add(photoAge)):
			out = append(out, r.dropPhoto(id, contract.PhotoTooOld)...)
		case p.URL != "" && !now.Before(p.Exp) && r.sending != id:
			out = append(out, r.lostAddress(id)...)
		case !p.RetryAt.IsZero() && !now.Before(p.RetryAt):
			p.RetryAt = time.Time{} // ready again: no wake-up left in the past
			r.s.Photos[id] = p
		}
	}
	return out
}

// uploads starts the next upload, if the time is right: the link is up, nothing is
// being sent, and nothing of an alarm waits in the queue.
func (r *Relay) uploads(now time.Time) []Action {
	if !r.up || r.sending != "" || slices.ContainsFunc(r.s.Queue, func(it Item) bool { return it.Prio == PriorityAlarm }) {
		return nil
	}
	id := r.oldestPhoto(func(_ string, p photo) bool { return p.URL != "" && now.Before(p.Exp) && !now.Before(p.RetryAt) })
	if id == "" {
		return nil
	}
	r.sending = id
	p := r.s.Photos[id]
	return []Action{Upload{Photo: id, URL: p.URL, Token: p.Token}}
}

// oldestPhoto returns the photo taken first among those that match, "" if none.
func (r *Relay) oldestPhoto(match func(string, photo) bool) string {
	best := ""
	for id, p := range r.s.Photos {
		if !match(id, p) {
			continue
		}
		if b, ok := r.s.Photos[best]; best == "" || !ok || p.Taken.Before(b.Taken) || (p.Taken.Equal(b.Taken) && id < best) {
			best = id
		}
	}
	return best
}

// photoWakes lists when the photos need the relay's attention.
func (r *Relay) photoWakes() []time.Time {
	var out []time.Time
	for _, p := range r.s.Photos {
		out = append(out, p.Taken.Add(photoAge))
		if p.URL != "" {
			out = append(out, p.Exp)
			if !p.RetryAt.IsZero() {
				out = append(out, p.RetryAt)
			}
		}
	}
	return out
}
