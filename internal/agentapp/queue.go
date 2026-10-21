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

// uplinkDoc is the document of the core's numbering for the cloud (see
// internal/agent/queue.go): a few bytes, written at each number.
type uplinkDoc struct {
	Last uint64 `json:"last"`
}

// restoreQueue puts the saved queue and numbering back into the core, after its
// snapshot, and makes the stores agree with it. It runs before the loop starts. A
// failed save stops the start: it may be a number given before a crash, which must
// never be given again.
func (a *App) restoreQueue() error {
	docs, err := a.deps.Queue.Load()
	if err != nil {
		return fmt.Errorf("agentapp: loading the queue: %w", err)
	}
	var uplink uplinkDoc
	if data, found, err := a.deps.Store.Load(keyUplink); err != nil {
		return fmt.Errorf("agentapp: loading the numbering for the cloud: %w", err)
	} else if found {
		if err := json.Unmarshal(data, &uplink); err != nil {
			return fmt.Errorf("agentapp: reading the numbering for the cloud: %w", err)
		}
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
	for _, act := range a.core.RestoreQueue(items, uplink.Last) {
		if err := a.save(act); err != nil {
			return err
		}
	}
	if len(items) > 0 {
		a.log.Info("queue for the cloud restored", "items", len(items))
	}
	return nil
}

// save carries out an action of the core on the queue's store or the counter's.
func (a *App) save(act agent.Action) error {
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
	case agent.UplinkSave:
		data, _ := json.Marshal(uplinkDoc{Last: v.Last}) // a number: cannot fail
		if err := a.deps.Store.Save(keyUplink, data); err != nil {
			return fmt.Errorf("agentapp: saving the numbering for the cloud: %w", err)
		}
	}
	return nil
}
