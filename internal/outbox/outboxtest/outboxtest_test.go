package outboxtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/outbox"
	"safehome/internal/outbox/outboxtest"
)

var _ outbox.Publisher = (*outboxtest.Broker)(nil)

func envelope(t *testing.T, typ string) []byte {
	t.Helper()
	env, err := contract.NewEnvelope("tg-lom-0001", "test", 1, contract.NewIDGenerator().New(), time.Now(), typ, map[string]int{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	return b
}

func TestTheBrokerRecordsAndAnswersAsTold(t *testing.T) {
	ctx := context.Background()
	b := &outboxtest.Broker{}
	b.Publish(ctx, "a", envelope(t, contract.TypeHeartbeat), 1, true)
	b.Publish(ctx, "zigbee2mqtt/x/set", []byte(`{"state":"ON"}`), 1, false)
	if len(b.All()) != 2 || len(b.On("a")) != 1 || !b.On("a")[0].Retain || len(b.OfType(contract.TypeHeartbeat)) != 1 {
		t.Fatalf("%+v", b.All())
	}
	if raw := b.On("zigbee2mqtt/x/set"); len(raw) != 1 || raw[0].IsEnv || string(raw[0].Payload) != `{"state":"ON"}` {
		t.Fatalf("%+v", raw)
	}

	b.FailNext(1)
	if err := b.Publish(ctx, "a", nil, 1, false); !errors.Is(err, outbox.ErrUnavailable) {
		t.Fatalf("FailNext: %v", err)
	}
	b.SetDown(true)
	if err := b.Publish(ctx, "a", nil, 1, false); !errors.Is(err, outbox.ErrUnavailable) {
		t.Fatalf("SetDown: %v", err)
	}
	b.SetDown(false)
	refused := errors.New("refused")
	b.Refuse(func(m outboxtest.Message) error {
		if m.Topic == "bad" {
			return refused
		}
		return nil
	})
	if err := b.Publish(ctx, "bad", nil, 1, false); err != refused || len(b.All()) != 2 {
		t.Fatalf("Refuse: %v", err)
	}

	release := b.Hold()
	short, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	if err := b.Publish(short, "a", nil, 1, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Hold: %v", err)
	}
	release()
	release() // twice does no harm
	if err := b.Publish(ctx, "a", nil, 1, false); err != nil || len(b.All()) != 3 {
		t.Fatalf("after the release: %v", err)
	}
}
