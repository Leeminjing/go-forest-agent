package events_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Leeminjing/go-forest-agent/agent"
	"github.com/Leeminjing/go-forest-agent/trees/events"
)

func TestDurableDeduplicationBoundAndSubscriberGap(t *testing.T) {
	m := events.New(2)
	s := m.Subscribe(1)
	defer s.Close()
	ctx := context.Background()
	one := agent.Event{RunID: "run", Sequence: 1, Kind: "saved"}
	if err := m.Publish(ctx, one); err != nil {
		t.Fatal(err)
	}
	if err := m.Publish(ctx, one); err != nil {
		t.Fatal(err)
	}
	if len(s.C) != 1 {
		t.Fatal("duplicate delivery")
	}
	one.Text = "different"
	if err := m.Publish(ctx, one); !errors.Is(err, agent.ErrConflict) {
		t.Fatal(err)
	}
	if err := m.Publish(ctx, agent.Event{RunID: "run", Sequence: 2, Kind: "saved"}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(s.Err(), events.ErrGap) {
		t.Fatal("missing subscriber gap")
	}
	if err := m.Publish(ctx, agent.Event{RunID: "run", Sequence: 3}); !errors.Is(err, agent.ErrEvents) {
		t.Fatal(err)
	}
	s.Close()
}
