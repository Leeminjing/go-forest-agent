// Package events offers a bounded, in-memory, idempotent event receiver.
// Checkpoints remain authoritative across process restarts.
package events

import (
	"context"
	"errors"
	"sync"

	"github.com/Leeminjing/go-forest-agent/agent"
	"github.com/Leeminjing/go-forest/forest"
)

type key struct {
	run      string
	sequence uint64
}
type Memory struct {
	mu      sync.Mutex
	limit   int
	durable map[key]agent.Event
	streams map[uint64]chan agent.Event
	gaps    map[uint64]chan struct{}
	next    uint64
}

func New(limit int) *Memory {
	if limit < 1 {
		limit = 4096
	}
	return &Memory{limit: limit, durable: map[key]agent.Event{}, streams: map[uint64]chan agent.Event{}}
}

var ErrGap = errors.New("event subscriber fell behind; read the run checkpoint")

// Subscription events can be dropped if the buffer fills. Err reports a gap;
// callers then reload the authoritative checkpoint and subscribe again.
type Subscription struct {
	C     <-chan agent.Event
	gaps  <-chan struct{}
	close func()
}

func (s *Subscription) Close() { s.close() }
func (s *Subscription) Err() error {
	select {
	case <-s.gaps:
		return ErrGap
	default:
		return nil
	}
}
func (m *Memory) Subscribe(buffer int) *Subscription {
	if buffer < 1 {
		buffer = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	id := m.next
	ch := make(chan agent.Event, buffer)
	m.streams[id] = ch
	gap := make(chan struct{}, 1)
	// The stream's gap channel is kept beside its channel under the same lock.
	if m.gaps == nil {
		m.gaps = map[uint64]chan struct{}{}
	}
	m.gaps[id] = gap
	var once sync.Once
	return &Subscription{C: ch, gaps: gap, close: func() {
		once.Do(func() { m.mu.Lock(); delete(m.streams, id); delete(m.gaps, id); close(ch); m.mu.Unlock() })
	}}
}
func (m *Memory) Publish(ctx context.Context, event agent.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !event.Temporary {
		if event.Sequence == 0 {
			return agent.ErrInvalid
		}
		k := key{event.RunID, event.Sequence}
		if prior, ok := m.durable[k]; ok {
			if prior != event {
				return agent.ErrConflict
			}
			return nil
		}
		if len(m.durable) >= m.limit {
			return agent.ErrEvents
		}
		m.durable[k] = event
	}
	for id, ch := range m.streams {
		select {
		case ch <- event:
		default:
			select {
			case m.gaps[id] <- struct{}{}:
			default:
			}
		}
	}
	return nil
}
func Definition(memory *Memory) forest.Definition {
	return forest.Definition{TypeID: "go-forest-agent.events.memory", Version: "1", Capabilities: []forest.CapabilityDescriptor{forest.Capability[agent.Events]("events", forest.Public)}, Build: func(ctx context.Context, env *forest.BuildEnv) (forest.Lifecycle, error) {
		if memory == nil {
			return nil, agent.ErrInvalid
		}
		return forest.Hooks{}, forest.Provide(env, "events", memory)
	}}
}
