package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Leeminjing/go-forest-agent/agent"
	"github.com/Leeminjing/go-forest-agent/trees/events"
	"github.com/Leeminjing/go-forest-agent/trees/filecheckpoint"
	"github.com/Leeminjing/go-forest-agent/trees/manualapproval"
	"github.com/Leeminjing/go-forest/forest"
)

type modelFunc func(context.Context, agent.ModelRequest, func(string) error) (agent.ModelResult, error)

func (f modelFunc) Generate(ctx context.Context, r agent.ModelRequest, emit func(string) error) (agent.ModelResult, error) {
	return f(ctx, r, emit)
}

type testTool struct {
	version  string
	approval bool
	calls    *atomic.Int32
	execute  func(context.Context, agent.ToolRequest) (string, error)
}

func (t *testTool) Describe(context.Context) (agent.ToolSpec, error) {
	return agent.ToolSpec{Name: "add", Version: t.version, Parameters: json.RawMessage(`{"type":"object"}`), NeedsApproval: t.approval}, nil
}
func (t *testTool) Execute(ctx context.Context, r agent.ToolRequest) (string, error) {
	t.calls.Add(1)
	if t.execute != nil {
		return t.execute(ctx, r)
	}
	return "5", nil
}
func toolModel(final string) modelFunc {
	return func(ctx context.Context, r agent.ModelRequest, emit func(string) error) (agent.ModelResult, error) {
		if r.Messages[len(r.Messages)-1].Role == "tool" {
			if err := emit(final); err != nil {
				return agent.ModelResult{}, err
			}
			return agent.ModelResult{Content: final}, nil
		}
		return agent.ModelResult{Calls: []agent.ToolCall{{ID: "call-1", Name: "add", Arguments: json.RawMessage(`{"a":2,"b":3}`)}}}, nil
	}
}
func modelDefinition(version string, model agent.Model, stop func(context.Context) error) forest.Definition {
	return forest.Definition{TypeID: "test.model", Version: version, HotSwappable: true, Capabilities: []forest.CapabilityDescriptor{forest.Capability[agent.Model]("model", forest.Public)}, Build: func(ctx context.Context, env *forest.BuildEnv) (forest.Lifecycle, error) {
		return forest.Hooks{OnStop: stop}, forest.Provide(env, "model", model)
	}}
}
func toolDefinition(tool *testTool) forest.Definition {
	return forest.Definition{TypeID: "test.tool", Version: tool.version, HotSwappable: true, Capabilities: []forest.CapabilityDescriptor{forest.Capability[agent.Tool]("tool", forest.Public)}, Build: func(ctx context.Context, env *forest.BuildEnv) (forest.Lifecycle, error) {
		return forest.Hooks{}, forest.Provide(env, "tool", tool)
	}}
}

type fixture struct {
	f      *forest.Forest
	store  *filecheckpoint.Store
	events *events.Memory
}

func setup(t *testing.T, directory string, model agent.Model, tool *testTool, wrap func(agent.Checkpoint) agent.Checkpoint, options agent.Options) *fixture {
	t.Helper()
	store, err := filecheckpoint.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint agent.Checkpoint = store
	if wrap != nil {
		checkpoint = wrap(store)
	}
	checkpointDef := forest.Definition{TypeID: "test.checkpoint", Version: "1", Capabilities: []forest.CapabilityDescriptor{forest.Capability[agent.Checkpoint]("checkpoint", forest.Public)}, Build: func(ctx context.Context, env *forest.BuildEnv) (forest.Lifecycle, error) {
		return forest.Hooks{}, forest.Provide(env, "checkpoint", checkpoint)
	}}
	memory := events.New(20000)
	f := forest.New(forest.Options{})
	defs := map[forest.TreeID]forest.Definition{"agent": agent.Definition(options), "model": modelDefinition("1", model, nil), "checkpoint": checkpointDef, "events": events.Definition(memory), "approval": manualapproval.Definition()}
	if tool != nil {
		defs["tool"] = toolDefinition(tool)
	}
	var ops []forest.Operation
	for id, def := range defs {
		if err := f.Register(def); err != nil {
			t.Fatal(err)
		}
		ops = append(ops, forest.AddRoot(id, def.Ref()))
	}
	for _, binding := range []struct {
		port     forest.PortID
		provider forest.TreeID
		cap      forest.CapabilityID
	}{{"model", "model", "model"}, {"checkpoint", "checkpoint", "checkpoint"}, {"events", "events", "events"}, {"approval", "approval", "approval"}} {
		ops = append(ops, forest.Connect(forest.Link{ID: string(binding.port), Consumer: "agent", Port: binding.port, Provider: binding.provider, Capability: binding.cap}))
	}
	if tool != nil {
		ops = append(ops, forest.Connect(forest.Link{ID: "tool", Consumer: "agent", Port: "tools", Provider: "tool", Capability: "tool"}))
	}
	if _, err := f.Apply(context.Background(), forest.ChangePlan{Operations: ops}); err != nil {
		t.Fatal(err)
	}
	x := &fixture{f: f, store: store, events: memory}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := f.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return x
}
func (x *fixture) use(ctx context.Context, fn func(context.Context, agent.Executor) (agent.Run, error)) (agent.Run, error) {
	var run agent.Run
	err := x.f.Invoke(ctx, func(ctx context.Context) error {
		return forest.Use[agent.Executor](ctx, x.f, "agent", "executor", func(e agent.Executor) error { var err error; run, err = fn(ctx, e); return err })
	})
	return run, err
}
func (x *fixture) create(t *testing.T, id string) agent.Run {
	t.Helper()
	run, err := x.use(context.Background(), func(ctx context.Context, e agent.Executor) (agent.Run, error) { return e.Create(ctx, id, "calculate") })
	if err != nil {
		t.Fatal(err)
	}
	return run
}
func (x *fixture) advance(id string) (agent.Run, error) {
	return x.use(context.Background(), func(ctx context.Context, e agent.Executor) (agent.Run, error) { return e.Advance(ctx, id) })
}
func (x *fixture) manage(fn func(context.Context, agent.Executor) (agent.Run, error)) (agent.Run, error) {
	return x.use(context.Background(), fn)
}
func signal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("step synchronization timeout")
	}
}

func TestModelToolModelDurableEventsAndNoReplay(t *testing.T) {
	var calls atomic.Int32
	directory := t.TempDir()
	x := setup(t, directory, toolModel("answer 5"), &testTool{version: "1", calls: &calls}, nil, agent.Options{})
	sub := x.events.Subscribe(128)
	defer sub.Close()
	x.create(t, "run")
	run, err := agent.Continue(context.Background(), x.f, "agent", "run")
	if err != nil || run.Status != agent.Completed || calls.Load() != 1 {
		t.Fatal(run, err, calls.Load())
	}
	loaded, err := x.store.Load(context.Background(), "run")
	if err != nil || loaded.Revision != run.Revision || loaded.Delivered != uint64(len(loaded.Events)) {
		t.Fatal(loaded, run, err)
	}
	foundDelta, foundFinal := false, false
	for len(sub.C) > 0 {
		event := <-sub.C
		if event.Temporary && event.Kind == "model.delta" {
			foundDelta = true
		}
		if !event.Temporary && event.Kind == "model.completed" && event.Text == "answer 5" {
			foundFinal = true
		}
	}
	if !foundDelta || !foundFinal || sub.Err() != nil {
		t.Fatal("stream/final events missing")
	}
	x2 := setup(t, directory, toolModel("should not execute"), &testTool{version: "1", calls: &calls}, nil, agent.Options{})
	if run, err := agent.Continue(context.Background(), x2.f, "agent", "run"); err != nil || run.Status != agent.Completed || calls.Load() != 1 {
		t.Fatal(run, err, calls.Load())
	}
}

func TestEachStepSeesNewModelAndOldScopeDrains(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls, stops atomic.Int32
	old := modelFunc(func(ctx context.Context, r agent.ModelRequest, emit func(string) error) (agent.ModelResult, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return agent.ModelResult{}, ctx.Err()
		}
		return toolModel("old").Generate(ctx, r, emit)
	})
	x := setup(t, t.TempDir(), old, &testTool{version: "1", calls: &calls}, nil, agent.Options{})
	// Replace the initial model with an observable equivalent before starting.
	observable := modelDefinition("observable", old, func(context.Context) error { stops.Add(1); return nil })
	next := modelDefinition("next", toolModel("new"), nil)
	for _, d := range []forest.Definition{observable, next} {
		if err := x.f.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := x.f.Apply(context.Background(), forest.ChangePlan{BaseGeneration: 1, Operations: []forest.Operation{forest.Replace("model", observable.Ref(), false)}}); err != nil {
		t.Fatal(err)
	}
	x.create(t, "hot")
	done := make(chan error, 1)
	go func() {
		run, err := agent.Continue(context.Background(), x.f, "agent", "hot")
		if err == nil && run.Messages[len(run.Messages)-1].Content != "new" {
			err = fmt.Errorf("new model not used: %+v", run)
		}
		done <- err
	}()
	signal(t, entered)
	if _, err := x.f.Apply(context.Background(), forest.ChangePlan{BaseGeneration: 2, Operations: []forest.Operation{forest.Replace("model", next.Ref(), false)}}); err != nil {
		t.Fatal(err)
	}
	if stops.Load() != 0 {
		t.Fatal("active model released")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := x.f.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if stops.Load() != 1 {
		t.Fatal(stops.Load())
	}
	if err := x.f.Invoke(context.Background(), func(ctx context.Context) error { _, err := agent.Continue(ctx, x.f, "agent", "hot"); return err }); !errors.Is(err, agent.ErrNestedScope) {
		t.Fatal(err)
	}
}

func TestApprovalRestartDenialAndChangedToolIdentity(t *testing.T) {
	var calls atomic.Int32
	directory := t.TempDir()
	tool := &testTool{version: "1", calls: &calls, approval: true}
	x := setup(t, directory, toolModel("done"), tool, nil, agent.Options{})
	x.create(t, "approval")
	run, err := agent.Continue(context.Background(), x.f, "agent", "approval")
	if err != nil || run.Status != agent.AwaitingApproval || calls.Load() != 0 {
		t.Fatal(run, err)
	}
	x2 := setup(t, directory, toolModel("done"), tool, nil, agent.Options{})
	if _, err := x2.manage(func(ctx context.Context, e agent.Executor) (agent.Run, error) {
		return e.Decide(ctx, "approval", "wrong", agent.Granted)
	}); !errors.Is(err, agent.ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := x2.manage(func(ctx context.Context, e agent.Executor) (agent.Run, error) {
		return e.Decide(ctx, "approval", "call-1", agent.Granted)
	}); err != nil {
		t.Fatal(err)
	}
	newTool := toolDefinition(&testTool{version: "2", calls: &calls, approval: true})
	if err := x2.f.Register(newTool); err != nil {
		t.Fatal(err)
	}
	if _, err := x2.f.Apply(context.Background(), forest.ChangePlan{BaseGeneration: 1, Operations: []forest.Operation{forest.Replace("tool", newTool.Ref(), false)}}); err != nil {
		t.Fatal(err)
	}
	if run, err := agent.Continue(context.Background(), x2.f, "agent", "approval"); err != nil || run.Status != agent.AwaitingApproval || calls.Load() != 0 {
		t.Fatal("old approval reused", run, err)
	}
	if _, err := x2.manage(func(ctx context.Context, e agent.Executor) (agent.Run, error) {
		return e.Decide(ctx, "approval", "call-1", agent.Granted)
	}); err != nil {
		t.Fatal(err)
	}
	if run, err := agent.Continue(context.Background(), x2.f, "agent", "approval"); err != nil || run.Status != agent.Completed || calls.Load() != 1 {
		t.Fatal(run, err)
	}
	x2.create(t, "denied")
	if _, err := agent.Continue(context.Background(), x2.f, "agent", "denied"); err != nil {
		t.Fatal(err)
	}
	if run, err := x2.manage(func(ctx context.Context, e agent.Executor) (agent.Run, error) {
		return e.Decide(ctx, "denied", "call-1", agent.Denied)
	}); err != nil || run.Status != agent.Failed {
		t.Fatal(run, err)
	}
	if _, err := agent.Continue(context.Background(), x2.f, "agent", "denied"); err != nil || calls.Load() != 1 {
		t.Fatal(err, calls.Load())
	}
}

type failingStore struct {
	agent.Checkpoint
	fail atomic.Bool
}

func (s *failingStore) CompareAndSwap(ctx context.Context, id string, expected uint64, next agent.Run) error {
	if len(next.Messages) > 0 && next.Messages[len(next.Messages)-1].Role == "tool" && s.fail.CompareAndSwap(true, false) {
		return errors.New("injected result persistence failure")
	}
	return s.Checkpoint.CompareAndSwap(ctx, id, expected, next)
}
func TestToolResultWriteFailureRequiresExplicitResolution(t *testing.T) {
	var calls atomic.Int32
	var fault *failingStore
	x := setup(t, t.TempDir(), toolModel("resolved"), &testTool{version: "1", calls: &calls}, func(c agent.Checkpoint) agent.Checkpoint {
		fault = &failingStore{Checkpoint: c}
		fault.fail.Store(true)
		return fault
	}, agent.Options{})
	x.create(t, "unknown")
	if _, err := agent.Continue(context.Background(), x.f, "agent", "unknown"); !errors.Is(err, agent.ErrUnknown) || !errors.Is(err, agent.ErrCheckpoint) {
		t.Fatal(err)
	}
	persisted, err := x.store.Load(context.Background(), "unknown")
	if err != nil || persisted.Status != agent.ToolRunning {
		t.Fatal(persisted, err)
	}
	run, err := x.manage(func(ctx context.Context, e agent.Executor) (agent.Run, error) { return e.Recover(ctx, "unknown") })
	if err != nil || run.Status != agent.OutcomeUnknown {
		t.Fatal(run, err)
	}
	if _, err := agent.Continue(context.Background(), x.f, "agent", "unknown"); !errors.Is(err, agent.ErrUnknown) || calls.Load() != 1 {
		t.Fatal(err, calls.Load())
	}
	if _, err := x.manage(func(ctx context.Context, e agent.Executor) (agent.Run, error) {
		return e.Resolve(ctx, "unknown", "call-1", "verified 5")
	}); err != nil {
		t.Fatal(err)
	}
	if run, err := agent.Continue(context.Background(), x.f, "agent", "unknown"); err != nil || run.Status != agent.Completed || calls.Load() != 1 {
		t.Fatal(run, err, calls.Load())
	}
}

func TestCancellationPropagatesAndToolUncertaintyIsPreserved(t *testing.T) {
	for _, at := range []string{"model", "tool"} {
		t.Run(at, func(t *testing.T) {
			entered := make(chan struct{})
			var calls atomic.Int32
			model := agent.Model(toolModel("done"))
			tool := &testTool{version: "1", calls: &calls}
			if at == "model" {
				model = modelFunc(func(ctx context.Context, _ agent.ModelRequest, _ func(string) error) (agent.ModelResult, error) {
					close(entered)
					<-ctx.Done()
					return agent.ModelResult{}, ctx.Err()
				})
			} else {
				tool.execute = func(ctx context.Context, _ agent.ToolRequest) (string, error) {
					close(entered)
					<-ctx.Done()
					return "", ctx.Err()
				}
			}
			x := setup(t, t.TempDir(), model, tool, nil, agent.Options{})
			x.create(t, "cancel")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := agent.Continue(ctx, x.f, "agent", "cancel"); done <- err }()
			signal(t, entered)
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			run, err := x.store.Load(context.Background(), "cancel")
			if err != nil {
				t.Fatal(err)
			}
			want := agent.Canceled
			if at == "tool" {
				want = agent.OutcomeUnknown
			}
			if run.Status != want {
				t.Fatal(run.Status, want)
			}
		})
	}
}

func TestConcurrentRunIsolationAndSingleRunClaims(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	model := modelFunc(func(ctx context.Context, r agent.ModelRequest, emit func(string) error) (agent.ModelResult, error) {
		if r.RunID == "busy" && first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return toolModel("done").Generate(ctx, r, emit)
	})
	x := setup(t, t.TempDir(), model, &testTool{version: "1", calls: &calls}, nil, agent.Options{})
	x.create(t, "busy")
	done := make(chan error, 1)
	go func() { _, err := x.advance("busy"); done <- err }()
	signal(t, entered)
	if _, err := x.advance("busy"); !errors.Is(err, agent.ErrBusy) {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 12)
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("run-%d", i)
		x.create(t, id)
		wg.Add(1)
		go func() {
			defer wg.Done()
			run, err := agent.Continue(context.Background(), x.f, "agent", id)
			if err == nil && run.Status != agent.Completed {
				err = fmt.Errorf("status %s", run.Status)
			}
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 12 {
		t.Fatal(calls.Load())
	}
}

func TestInvalidModelAndUnconnectedToolsFailWithoutPanic(t *testing.T) {
	for _, calls := range [][]agent.ToolCall{{{ID: "x", Name: "add", Arguments: json.RawMessage(`{`)}}, {{ID: "x", Name: "hidden", Arguments: json.RawMessage(`{}`)}}, {{ID: "x", Name: "add", Arguments: json.RawMessage(`{}`)}, {ID: "x", Name: "add", Arguments: json.RawMessage(`{}`)}}} {
		var toolCalls atomic.Int32
		model := modelFunc(func(context.Context, agent.ModelRequest, func(string) error) (agent.ModelResult, error) {
			return agent.ModelResult{Calls: calls}, nil
		})
		x := setup(t, t.TempDir(), model, &testTool{version: "1", calls: &toolCalls}, nil, agent.Options{})
		x.create(t, "invalid")
		if _, err := agent.Continue(context.Background(), x.f, "agent", "invalid"); !errors.Is(err, agent.ErrInvalid) || toolCalls.Load() != 0 {
			t.Fatal(err, toolCalls.Load())
		}
	}
}
