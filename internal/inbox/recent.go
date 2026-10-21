package inbox

import "sync"

// RecentIDs remembers the latest message ids, so a message delivered twice (QoS 1
// is at least once) is handled once. It forgets the oldest when full.
type RecentIDs struct {
	mu   sync.Mutex
	ring []string
	set  map[string]bool
	next int
}

// NewRecentIDs remembers up to size ids, at least one.
func NewRecentIDs(size int) *RecentIDs {
	size = max(size, 1)
	return &RecentIDs{ring: make([]string, size), set: make(map[string]bool, size)}
}

// FirstTime reports whether id is new, and remembers it.
func (r *RecentIDs) FirstTime(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.set[id] {
		return false
	}
	if old := r.ring[r.next]; old != "" {
		delete(r.set, old)
	}
	r.ring[r.next] = id
	r.set[id] = true
	r.next = (r.next + 1) % len(r.ring)
	return true
}
