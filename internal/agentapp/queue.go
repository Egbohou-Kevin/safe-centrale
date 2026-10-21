package agentapp

import (
	"encoding/json"
	"fmt"

	"safehome/internal/agent"
)

// QueueStore keeps the core's queue for the cloud on disk, one document per item
// (see internal/agent/queue.go): draining the queue writes what moves, not the whole
// queue. See internal/adapter/queuedir.
type QueueStore interface {
	// Put writes the item name (valid JSON), replacing it if it exists.
	Put(name string, data []byte) error
	// Remove removes the items named; one that does not exist is no error.
	Remove(names ...string) error
	// Load returns every item, by name.
	Load() (map[string][]byte, error)
}

// restoreQueue puts the saved queue back into the core, after its snapshot, and makes
// the store agree with it. It runs before the loop starts. A failed save stops the
// start: it may be a queue that only the snapshot holds (saved by an older version),
// which the next snapshot would no longer carry.
func (a *App) restoreQueue() error {
	docs, err := a.deps.Queue.Load()
	if err != nil {
		return fmt.Errorf("agentapp: loading the queue: %w", err)
	}
	items := make([]agent.Item, 0, len(docs))
	for name, data := range docs {
		var it agent.Item
		if err := json.Unmarshal(data, &it); err != nil || it.ID != name {
			a.log.Warn("ignoring an unreadable item of the queue", "name", name, "err", err)
			continue
		}
		items = append(items, it)
	}
	for _, act := range a.core.RestoreQueue(items) {
		if err := a.storeQueue(act); err != nil {
			return err
		}
	}
	if len(items) > 0 {
		a.log.Info("queue for the cloud restored", "items", len(items))
	}
	return nil
}

// storeQueue carries out a queue action of the core on the store.
func (a *App) storeQueue(act agent.Action) error {
	switch v := act.(type) {
	case agent.QueueSave:
		data, err := json.Marshal(v.Item)
		if err == nil {
			err = a.deps.Queue.Put(v.Item.ID, data)
		}
		if err != nil {
			return fmt.Errorf("agentapp: saving an item of the queue: %w", err)
		}
	case agent.QueueDrop:
		if err := a.deps.Queue.Remove(v.IDs...); err != nil {
			return fmt.Errorf("agentapp: removing items of the queue: %w", err)
		}
	}
	return nil
}
