// Package inbox is the way in of every application of the site's programs: the
// message the MQTT client hands over, the subscriptions it asks for, the gate a
// message goes through before anything trusts it, and the memory of recent message
// ids that makes an at-least-once delivery handled once.
//
// It knows the contract and nothing else; the composition root turns the MQTT
// client's messages and subscriptions into these.
package inbox

import (
	"errors"
	"fmt"
	"slices"

	"safehome/internal/contract"
)

// Message is a message received from the broker.
type Message struct {
	Topic   string
	Payload []byte
	Retain  bool
}

// Subscription is a topic filter an application listens to, with the best QoS
// wanted. NoLocal: the client does not hear what it publishes itself there (the link
// agent, which republishes between two brokers).
type Subscription struct {
	Filter  string
	QoS     byte
	NoLocal bool
}

// Why the gate refused a message; a message that breaks the contract gets an error
// wrapping contract.ErrInvalid (or contract.ErrUnsupportedVersion) instead.
var (
	ErrOtherSite = errors.New("message for another site")
	ErrWrongType = errors.New("wrong type for the topic")
)

// Envelope reads the envelope of m and checks it: valid, for site, and of one of
// types. A topic that carries several types (alarm/event) refuses the others with
// ErrWrongType, which the caller may then ignore quietly.
func Envelope(m Message, site string, types ...string) (contract.Envelope, error) {
	env, err := contract.ParseEnvelope(m.Payload)
	switch {
	case err != nil:
		return contract.Envelope{}, err
	case env.Site != site:
		return contract.Envelope{}, fmt.Errorf("%w: %q", ErrOtherSite, env.Site)
	case !slices.Contains(types, env.Type):
		return contract.Envelope{}, fmt.Errorf("%w: %q", ErrWrongType, env.Type)
	}
	return env, nil
}

// Open is Envelope for one type, followed by its data, checked (contract.ValidData).
// When only the data is wrong, the envelope is returned with the error, for the log.
// Dropping a duplicate (RecentIDs) is left to the caller.
func Open[T any](m Message, site, typ string) (contract.Envelope, T, error) {
	var d T
	env, err := Envelope(m, site, typ)
	if err != nil {
		return env, d, err
	}
	d, err = contract.ValidData[T](env)
	return env, d, err
}
