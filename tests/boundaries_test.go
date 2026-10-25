package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Leeminjing/go-forest-agent/agent"
)

func TestNoToolsAndConcurrentStreamingCallbacks(t *testing.T) {
	var late func(string) error
	model := modelFunc(func(ctx context.Context, r agent.ModelRequest, emit func(string) error) (agent.ModelResult, error) {
		if len(r.Tools) != 0 {
			return agent.ModelResult{}, agent.ErrInvalid
		}
		late = emit
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := emit("delta"); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		return agent.ModelResult{Content: "finished"}, nil
	})
	x := setup(t, t.TempDir(), model, nil, nil, agent.Options{})
	x.create(t, "no-tools")
	if run, err := agent.Continue(context.Background(), x.f, "agent", "no-tools"); err != nil || run.Status != agent.Completed {
		t.Fatal(run, err)
	}
	if err := late("escaped callback"); !errors.Is(err, agent.ErrInvalid) {
		t.Fatal(err)
	}
}

func TestModelTurnLimitAndUnknownOnToolPanic(t *testing.T) {
	var calls atomic.Int32
	loop := modelFunc(func(ctx context.Context, r agent.ModelRequest, emit func(string) error) (agent.ModelResult, error) {
		return agent.ModelResult{Calls: []agent.ToolCall{{ID: fmt.Sprintf("call-%d", len(r.Messages)), Name: "add", Arguments: json.RawMessage(`{}`)}}}, nil
	})
	x := setup(t, t.TempDir(), loop, &testTool{version: "1", calls: &calls}, nil, agent.Options{MaxModelTurns: 2})
	x.create(t, "loop")
	if run, err := agent.Continue(context.Background(), x.f, "agent", "loop"); !errors.Is(err, agent.ErrStepLimit) || run.Status != agent.Failed || calls.Load() != 2 {
		t.Fatal(run, err, calls.Load())
	}
	x2 := setup(t, t.TempDir(), toolModel("done"), &testTool{version: "1", calls: &calls, execute: func(context.Context, agent.ToolRequest) (string, error) { panic("tool panic") }}, nil, agent.Options{})
	x2.create(t, "panic")
	if run, err := agent.Continue(context.Background(), x2.f, "agent", "panic"); !errors.Is(err, agent.ErrUnknown) || run.Status != agent.OutcomeUnknown {
		t.Fatal(run, err)
	}
}

type deliveryFault struct {
	agent.Checkpoint
	fail atomic.Bool
}

func (s *deliveryFault) CompareAndSwap(ctx context.Context, id string, expected uint64, next agent.Run) error {
	prior, err := s.Checkpoint.Load(ctx, id)
	if err == nil && next.Delivered > prior.Delivered && s.fail.CompareAndSwap(true, false) {
		return errors.New("event acknowledgement commit failed")
	}
	return s.Checkpoint.CompareAndSwap(ctx, id, expected, next)
}
func TestDurableEventsReplayAfterAcknowledgementFailure(t *testing.T) {
	var fault *deliveryFault
	x := setup(t, t.TempDir(), modelFunc(func(context.Context, agent.ModelRequest, func(string) error) (agent.ModelResult, error) {
		return agent.ModelResult{Content: "done"}, nil
	}), nil, func(c agent.Checkpoint) agent.Checkpoint { fault = &deliveryFault{Checkpoint: c}; return fault }, agent.Options{})
	sub := x.events.Subscribe(32)
	defer sub.Close()
	fault.fail.Store(true)
	if _, err := x.use(context.Background(), func(ctx context.Context, e agent.Executor) (agent.Run, error) {
		return e.Create(ctx, "outbox", "prompt")
	}); !errors.Is(err, agent.ErrCheckpoint) {
		t.Fatal(err)
	}
	persisted, err := x.store.Load(context.Background(), "outbox")
	if err != nil || persisted.Delivered != 0 {
		t.Fatal(persisted, err)
	}
	if run, err := agent.Continue(context.Background(), x.f, "agent", "outbox"); err != nil || run.Status != agent.Completed {
		t.Fatal(run, err)
	}
	created := 0
	for len(sub.C) > 0 {
		if event := <-sub.C; event.Kind == "run.created" {
			created++
		}
	}
	if created != 1 {
		t.Fatal("event receiver did not deduplicate", created)
	}
}
