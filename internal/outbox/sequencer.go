package outbox

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"safehome/internal/driver"
)

// Sequencer hands out the contract's seq: a counter that only goes up, per site,
// and survives restarts, so the cloud can spot holes after a power cut.
type Sequencer struct {
	mu    sync.Mutex
	store driver.Store
	name  string
	log   *slog.Logger
	n     uint64
}

type seqDoc struct {
	Seq uint64 `json:"seq"`
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
		s.n = d.Seq
	}
	return s, nil
}

// Next returns the next number and saves it before returning, so a number is never
// handed out twice, even across a crash. If the disk refuses, the number is still
// used: an alarm message must leave, and a repeated number after a restart is a
// lesser evil than a message that never goes out.
func (s *Sequencer) Next() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	b, _ := json.Marshal(seqDoc{Seq: s.n})
	if err := s.store.Save(s.name, b); err != nil {
		s.log.Error("saving the message counter failed", "err", err)
	}
	return s.n
}
