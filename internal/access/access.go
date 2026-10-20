// Package access holds the access codes of a site and checks them (see
// docs/security-install.md). It is a third domain core next to the alarm engine and
// the health monitor, built on the same rules: it depends on the standard library
// only, does no I/O of its own, and never reads the clock.
//
// Each authorised person has one code, at most five per site. A code is stored
// only as a salted, deliberately slow hash (PBKDF2-HMAC-SHA256), on the hub, and is
// never written anywhere in clear: not in an error, not in a log, not in a
// snapshot. Verifying a code finds out who it belongs to, so the engine can say who
// armed or disarmed.
//
// The package does not count wrong attempts or lock anything: that is the alarm
// engine's rule (three wrong codes lock disarming for ten minutes), because it is
// about the alarm and not about codes.
//
// Saving is injected as a function, so the package neither touches the disk nor
// imports JSON. A Registry is safe for concurrent use.
package access

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
)

// Errors returned by Set and Remove. None of them ever contains a code.
var (
	ErrInvalidUser  = errors.New("access: invalid user identifier")
	ErrInvalidCode  = errors.New("access: a code is made of digits only, within the allowed length")
	ErrWeakCode     = errors.New("access: code too easy to guess")
	ErrCodeTaken    = errors.New("access: this code belongs to someone else")
	ErrTooManyUsers = errors.New("access: the site already has the maximum number of codes")
	ErrNoSuchUser   = errors.New("access: no such user")
)

const (
	algoPBKDF2 = "pbkdf2-sha256"
	saltBytes  = 16
	hashBytes  = 32
	maxUserLen = 64
	// minIterations is the lowest cost the registry accepts: below it, a stolen file
	// would give up the codes in seconds.
	minIterations = 10_000
)

// Record is what is stored for one person. It holds no code, only what is needed to
// check one. The fields are meant to be saved as JSON by the caller.
type Record struct {
	User       string `json:"user"`
	Algo       string `json:"algo"`
	Iterations int    `json:"iterations"`
	Salt       []byte `json:"salt"`
	Hash       []byte `json:"hash"`
}

// Config holds the registry settings.
type Config struct {
	MaxUsers  int // codes per site (5)
	MinDigits int // shortest code (4)
	MaxDigits int // longest code (8)
	// Iterations is the cost of the hash for new codes. It is recorded with each code,
	// so it can be raised later without invalidating the existing ones. It must be
	// tuned on the real hub: a check should take a fraction of a second there.
	Iterations int
	// Rand supplies the salts; nil means the system's secure random source.
	Rand io.Reader
}

// DefaultConfig returns the defaults decided in docs/security-install.md.
func DefaultConfig() Config {
	return Config{MaxUsers: 5, MinDigits: 4, MaxDigits: 8, Iterations: 200_000}
}

// Validate checks that the configuration is usable.
func (c Config) Validate() error {
	switch {
	case c.MaxUsers < 1:
		return fmt.Errorf("access: MaxUsers must be at least 1, got %d", c.MaxUsers)
	case c.MinDigits < 4:
		return fmt.Errorf("access: MinDigits must be at least 4, got %d", c.MinDigits)
	case c.MaxDigits < c.MinDigits:
		return fmt.Errorf("access: MaxDigits (%d) must not be below MinDigits (%d)", c.MaxDigits, c.MinDigits)
	case c.Iterations < minIterations:
		return fmt.Errorf("access: Iterations must be at least %d, got %d", minIterations, c.Iterations)
	}
	return nil
}

// Registry is the set of codes of a site.
type Registry struct {
	cfg     Config
	persist func([]Record) error

	mu      sync.Mutex
	records []Record
}

// New creates a registry from the records saved earlier (nil for a new site).
// persist is called with the complete list of records before any change takes
// effect: if it fails, the change is dropped, so a code that could not be saved is
// never silently accepted for the session and lost at the next restart.
func New(cfg Config, records []Record, persist func([]Record) error) (*Registry, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if persist == nil {
		return nil, errors.New("access: persist is required")
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	if len(records) > cfg.MaxUsers {
		return nil, fmt.Errorf("access: %d saved codes, the maximum is %d", len(records), cfg.MaxUsers)
	}
	seen := map[string]bool{}
	for _, rec := range records {
		if err := rec.validate(); err != nil {
			return nil, err
		}
		if seen[rec.User] {
			return nil, fmt.Errorf("access: user %q appears twice in the saved codes", rec.User)
		}
		seen[rec.User] = true
	}
	return &Registry{cfg: cfg, persist: persist, records: cloneRecords(records)}, nil
}

func (r Record) validate() error {
	switch {
	case !validUser(r.User):
		return fmt.Errorf("access: saved code with an invalid user identifier %q", r.User)
	case r.Algo != algoPBKDF2:
		return fmt.Errorf("access: user %q: unknown algorithm %q", r.User, r.Algo)
	case r.Iterations < minIterations:
		return fmt.Errorf("access: user %q: %d iterations is too few", r.User, r.Iterations)
	case len(r.Salt) < saltBytes || len(r.Hash) != hashBytes:
		return fmt.Errorf("access: user %q: malformed salt or hash", r.User)
	}
	return nil
}

func cloneRecords(in []Record) []Record {
	out := make([]Record, len(in))
	for i, r := range in {
		r.Salt, r.Hash = slices.Clone(r.Salt), slices.Clone(r.Hash)
		out[i] = r
	}
	return out
}

func validUser(u string) bool { return u != "" && len(u) <= maxUserLen }

// Users returns the identifiers of the people who have a code, sorted.
func (r *Registry) Users() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	users := make([]string, len(r.records))
	for i, rec := range r.records {
		users[i] = rec.User
	}
	slices.Sort(users)
	return users
}

// Records returns a copy of what is stored, for saving or inspection. It holds
// hashes, never codes.
func (r *Registry) Records() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneRecords(r.records)
}

// Set gives user a code, or replaces the one they have. It refuses a code that is
// badly formed, too easy to guess, or already used by someone else (a code must
// identify one person).
func (r *Registry) Set(user, code string) error {
	if !validUser(user) {
		return ErrInvalidUser
	}
	if !r.wellFormed(code) {
		return ErrInvalidCode
	}
	if weak(code) {
		return ErrWeakCode
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	idx := slices.IndexFunc(r.records, func(rec Record) bool { return rec.User == user })
	if idx < 0 && len(r.records) >= r.cfg.MaxUsers {
		return ErrTooManyUsers
	}
	for i, rec := range r.records {
		if i != idx && matches(rec, code) {
			return ErrCodeTaken
		}
	}

	rec, err := r.newRecord(user, code)
	if err != nil {
		return err
	}
	next := cloneRecords(r.records)
	if idx < 0 {
		next = append(next, rec)
	} else {
		next[idx] = rec
	}
	if err := r.persist(cloneRecords(next)); err != nil {
		return fmt.Errorf("access: the code could not be saved: %w", err)
	}
	r.records = next
	return nil
}

// Reload replaces every code with the given records, which come from the saved
// copy: it is how a running hub picks up the codes an installer has just set with
// the command-line tool. The records are checked like those given to New, and
// nothing is saved (they are already on disk).
func (r *Registry) Reload(records []Record) error {
	if len(records) > r.cfg.MaxUsers {
		return fmt.Errorf("access: %d saved codes, the maximum is %d", len(records), r.cfg.MaxUsers)
	}
	seen := map[string]bool{}
	for _, rec := range records {
		if err := rec.validate(); err != nil {
			return err
		}
		if seen[rec.User] {
			return fmt.Errorf("access: user %q appears twice in the saved codes", rec.User)
		}
		seen[rec.User] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = cloneRecords(records)
	return nil
}

// Remove deletes the code of user: that person can no longer arm or disarm.
func (r *Registry) Remove(user string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := slices.IndexFunc(r.records, func(rec Record) bool { return rec.User == user })
	if idx < 0 {
		return ErrNoSuchUser
	}
	next := slices.Delete(cloneRecords(r.records), idx, idx+1)
	if err := r.persist(cloneRecords(next)); err != nil {
		return fmt.Errorf("access: the removal could not be saved: %w", err)
	}
	r.records = next
	return nil
}

// Verify tells whose code this is. It checks every stored code, with no early exit,
// so the time taken does not reveal which person matched. It does not count wrong
// attempts: the alarm engine does.
func (r *Registry) Verify(code string) (user string, ok bool) {
	if !r.wellFormed(code) {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.records {
		if matches(rec, code) && !ok {
			user, ok = rec.User, true
		}
	}
	return user, ok
}

func (r *Registry) wellFormed(code string) bool {
	if len(code) < r.cfg.MinDigits || len(code) > r.cfg.MaxDigits {
		return false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (r *Registry) newRecord(user, code string) (Record, error) {
	salt := make([]byte, saltBytes)
	if _, err := io.ReadFull(r.cfg.Rand, salt); err != nil {
		return Record{}, fmt.Errorf("access: no random salt available: %w", err)
	}
	hash, err := derive(code, salt, r.cfg.Iterations)
	if err != nil {
		return Record{}, err
	}
	return Record{User: user, Algo: algoPBKDF2, Iterations: r.cfg.Iterations, Salt: salt, Hash: hash}, nil
}

func derive(code string, salt []byte, iterations int) ([]byte, error) {
	h, err := pbkdf2.Key(sha256.New, code, salt, iterations, hashBytes)
	if err != nil {
		return nil, fmt.Errorf("access: %w", err)
	}
	return h, nil
}

// matches reports, in constant time, whether code is the one behind rec.
func matches(rec Record, code string) bool {
	h, err := derive(code, rec.Salt, rec.Iterations)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(h, rec.Hash) == 1
}

// weak reports codes that anyone tries first: one digit repeated (0000, 777777) and
// straight runs up or down (1234, 987654, 7890).
func weak(code string) bool {
	same, up, down := true, true, true
	for i := 1; i < len(code); i++ {
		d := int(code[i]) - int(code[i-1])
		if d != 0 {
			same = false
		}
		if d != 1 && d != -9 { // +1, with 9 followed by 0
			up = false
		}
		if d != -1 && d != 9 { // -1, with 0 followed by 9
			down = false
		}
	}
	return same || up || down
}
