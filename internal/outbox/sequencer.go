package outbox

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"safehome/internal/driver"
)

// seqBlock is how many numbers are reserved by one write. Saving the counter for
// every message would wear the SD card out (a write, two fsyncs, a rename each); a
// restart skips at most the rest of a block, which only the local programs see (the
// agent numbers what goes to the cloud again, without holes).
const seqBlock = 1000

// Sequencer hands out the contract's seq on the site: a counter that only goes up
// and survives restarts. It may jump after a restart, never go back.
type Sequencer struct {
	mu       sync.Mutex
	store    driver.Store
	name     string
	log      *slog.Logger
	n        uint64 // the last number handed out
	reserved uint64 // the highest number saved as possibly handed out
}

type seqDoc struct {
	Seq uint64 `json:"seq"` // the end of the reserved block
}

// NewSequencer loads the counter saved under name.
func NewSequencer(store driver.Store, name string, log *slog.Logger) (*Sequencer, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Sequencer{store: store, name: name, log: log}
	data, found, err := store.Load(name)
	if err != nil {
		return nil, fmt.Errorf("outbox: loading the message counter: %w", err)
	}
	if found {
		var d seqDoc
		if err := json.Unmarshal(data, &d); err != nil {
			return nil, fmt.Errorf("outbox: reading the message counter: %w", err)
		}
		s.n, s.reserved = d.Seq, d.Seq
	}
	return s, nil
}

// Next returns the next number. A number is saved as reserved before it is handed
// out, so none is handed out twice, even across a crash. If the disk refuses, the
// number is still used (and the save tried again next time): an alarm message must
// leave, and a repeated number after a restart is a lesser evil than a message that
// never goes out.
func (s *Sequencer) Next() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	if s.n > s.reserved {
		b, _ := json.Marshal(seqDoc{Seq: s.n + seqBlock - 1})
		if err := s.store.Save(s.name, b); err != nil {
			s.log.Error("saving the message counter failed", "err", err)
		} else {
			s.reserved = s.n + seqBlock - 1
		}
	}
	return s.n
}
