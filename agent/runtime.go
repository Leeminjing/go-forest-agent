package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/Leeminjing/go-forest/forest"
)

func clone[T any](value T) T {
	data, err := json.Marshal(value)
	if err != nil {
		panic("internal non-serializable agent state")
	}
	var copied T
	if err := json.Unmarshal(data, &copied); err != nil {
		panic("internal invalid agent state")
	}
	return copied
}

func (r *runtime) Read(ctx context.Context, id string) (Run, error) {
	var run Run
	err := r.checkpoint.Use(ctx, func(c Checkpoint) error { var err error; run, err = c.Load(ctx, id); return err })
	if err == nil {
		if run.ID != id {
			err = ErrCorrupt
		} else {
			err = run.Validate()
		}
	}
	return run, err
}
func (r *runtime) save(ctx context.Context, run *Run) error {
	if len(run.Events) > 16384 {
		return ErrInvalid
	}
	// Finalization retains this Scope, but gets a bounded opportunity to save a
	// known outcome even when the business caller has canceled its own Context.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.options.CommitTimeout)
	defer cancel()
	next := clone(*run)
	next.Revision++
	if err := next.Validate(); err != nil {
		return err
	}
	err := r.checkpoint.Use(commitCtx, func(c Checkpoint) error { return c.CompareAndSwap(commitCtx, run.ID, run.Revision, next) })
	if err != nil {
		return errors.Join(ErrCheckpoint, err)
	}
	*run = next
	return nil
}
func addEvent(run *Run, kind, text, call string) {
	run.Events = append(run.Events, Event{RunID: run.ID, Sequence: uint64(len(run.Events) + 1), Kind: kind, Text: text, CallID: call})
}
func (r *runtime) flush(ctx context.Context, run *Run) error {
	if run.Delivered == uint64(len(run.Events)) {
		return nil
	}
	for _, event := range run.Events[run.Delivered:] {
		if err := r.events.Use(ctx, func(e Events) error { return e.Publish(ctx, event) }); err != nil {
			return errors.Join(ErrEvents, err)
		}
	}
	next := *run
	next.Delivered = uint64(len(run.Events))
	if err := r.save(ctx, &next); err != nil {
		return err
	}
	*run = next
	return nil
}
func (r *runtime) Create(ctx context.Context, id, prompt string) (Run, error) {
	if id == "" || len(id) > 256 || prompt == "" || len(prompt) > 1<<20 {
		return Run{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Run{}, err
	}
	run := Run{ID: id, Status: ModelReady, Messages: []Message{{Role: "user", Content: prompt}}}
	addEvent(&run, "run.created", "", "")
	if err := r.save(ctx, &run); err != nil {
		return Run{}, err
	}
	err := r.flush(ctx, &run)
	return run, err
}
func (r *runtime) Advance(ctx context.Context, id string) (Run, error) {
	run, err := r.Read(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if run.Status == ModelRunning || run.Status == ToolRunning {
		return run, ErrBusy
	}
	if err := r.flush(ctx, &run); err != nil {
		return run, err
	}
	switch run.Status {
	case Completed, Canceled, Failed, AwaitingApproval:
		return run, nil
	case OutcomeUnknown:
		return run, ErrUnknown
	case ModelReady, ToolsReady:
	default:
		return run, ErrCorrupt
	}
	if err := ctx.Err(); err != nil {
		run.Status = Canceled
		addEvent(&run, "run.canceled", "", "")
		return run, errors.Join(err, r.save(ctx, &run))
	}
	if run.Status == ModelReady {
		err = r.modelStep(ctx, &run)
	} else {
		err = r.toolStep(ctx, &run)
	}
	if err != nil {
		// An unsuccessful commit may leave the old durable running state. Never
		// return an uncommitted completed tool result as authoritative state.
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.options.CommitTimeout)
		defer cancel()
		persisted, readErr := r.Read(readCtx, id)
		if readErr != nil {
			return Run{}, errors.Join(err, readErr)
		}
		if persisted.Status != ModelRunning && persisted.Status != ToolRunning {
			flushErr := r.flush(readCtx, &persisted)
			return persisted, errors.Join(err, flushErr)
		}
		return persisted, err
	}
	err = r.flush(ctx, &run)
	return run, err
}

func (r *runtime) catalog(ctx context.Context) ([]ToolSpec, error) {
	var specs []ToolSpec
	err := r.tools.UseAll(ctx, func(tools []Tool) error {
		seen := map[string]bool{}
		for _, tool := range tools {
			spec, err := tool.Describe(ctx)
			if err != nil {
				return err
			}
			if spec.Name == "" || spec.Version == "" || !json.Valid(spec.Parameters) || seen[spec.Name] || len(specs) >= 64 {
				return ErrInvalid
			}
			seen[spec.Name] = true
			specs = append(specs, clone(spec))
		}
		return nil
	})
	if errors.Is(err, forest.ErrBindingUnavailable) {
		err = nil
	}
	return specs, err
}

func safeCall[T any](fn func() (T, error)) (value T, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("provider panicked")
		}
	}()
	return fn()
}
func (r *runtime) modelStep(ctx context.Context, run *Run) error {
	if run.ModelTurns >= r.options.MaxModelTurns {
		run.Status = Failed
		addEvent(run, "run.limit", "", "")
		return errors.Join(ErrStepLimit, r.save(ctx, run))
	}
	specs, err := r.catalog(ctx)
	if err != nil {
		return err
	}
	run.Status = ModelRunning
	if err := r.save(ctx, run); err != nil {
		return err
	}
	request := ModelRequest{RunID: run.ID, Messages: clone(run.Messages), Tools: clone(specs)}
	var result ModelResult
	var streamErr error
	var streamMu sync.Mutex
	streamClosed := false
	deltas := 0
	deltaBytes := 0
	err = r.model.Use(ctx, func(model Model) error {
		var err error
		result, err = safeCall(func() (ModelResult, error) {
			return model.Generate(ctx, request, func(text string) error {
				streamMu.Lock()
				defer streamMu.Unlock()
				if streamClosed {
					return ErrInvalid
				}
				if streamErr != nil {
					return streamErr
				}
				deltas++
				deltaBytes += len(text)
				if deltas > 4096 || len(text) > 1<<20 || deltaBytes > 4<<20 {
					streamErr = ErrInvalid
					return streamErr
				}
				streamErr = r.events.Use(ctx, func(events Events) error {
					return events.Publish(ctx, Event{RunID: run.ID, Kind: "model.delta", Text: text, Temporary: true})
				})
				return streamErr
			})
		})
		streamMu.Lock()
		streamClosed = true
		streamMu.Unlock()
		if err == nil {
			err = validateResult(*run, result, specs)
		}
		if err == nil {
			result = clone(result)
		}
		return err
	})
	if err == nil {
		err = streamErr
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = validateResult(*run, result, specs)
	}
	if err != nil {
		run.Status = Failed
		if ctx.Err() != nil {
			run.Status = Canceled
		}
		addEvent(run, "model.failed", "", "")
		return errors.Join(err, r.save(ctx, run))
	}
	run.ModelTurns++
	run.Messages = append(run.Messages, Message{Role: "assistant", Content: result.Content, Calls: result.Calls})
	run.Calls = nil
	run.Current = 0
	for _, call := range result.Calls {
		for _, spec := range specs {
			if spec.Name == call.Name {
				run.Calls = append(run.Calls, PendingCall{Call: call, Tool: spec})
				break
			}
		}
	}
	run.Status = Completed
	if len(run.Calls) > 0 {
		run.Status = ToolsReady
	}
	addEvent(run, "model.completed", result.Content, "")
	return r.save(ctx, run)
}
func validateResult(run Run, result ModelResult, specs []ToolSpec) error {
	if len(result.Calls) > 16 || len(run.Messages) > 512 || len(result.Content) > 1<<20 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, message := range run.Messages {
		for _, call := range message.Calls {
			seen[call.ID] = true
		}
	}
	for _, call := range result.Calls {
		if call.ID == "" || len(call.ID) > 256 || seen[call.ID] || len(call.Arguments) > 1<<20 || !json.Valid(call.Arguments) {
			return ErrInvalid
		}
		seen[call.ID] = true
		found := false
		for _, spec := range specs {
			if spec.Name == call.Name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: unconnected tool %q", ErrInvalid, call.Name)
		}
	}
	return nil
}

func (r *runtime) toolStep(ctx context.Context, run *Run) error {
	if run.Current < 0 || run.Current >= len(run.Calls) {
		return ErrCorrupt
	}
	pending := &run.Calls[run.Current]
	return r.tools.UseAll(ctx, func(tools []Tool) error {
		var selected Tool
		var actual ToolSpec
		for _, tool := range tools {
			spec, err := tool.Describe(ctx)
			if err != nil {
				return err
			}
			if spec.Name == pending.Call.Name {
				if selected != nil {
					return ErrInvalid
				}
				selected = tool
				actual = clone(spec)
			}
		}
		if selected == nil {
			return ErrNotFound
		}
		if actual.Version == "" || !json.Valid(actual.Parameters) {
			return ErrInvalid
		}
		if !reflect.DeepEqual(actual, pending.Tool) {
			pending.Tool = actual
			pending.Approved = false
			addEvent(run, "tool.changed", "", pending.Call.ID)
			return r.save(ctx, run)
		}
		if actual.NeedsApproval && !pending.Approved {
			var decision Decision
			err := r.approval.Use(ctx, func(a Approval) error {
				var err error
				decision, err = a.Check(ctx, clone(ApprovalRequest{RunID: run.ID, Call: pending.Call, Tool: actual}))
				return err
			})
			if err != nil {
				return err
			}
			switch decision {
			case Pending:
				run.Status = AwaitingApproval
			case Granted:
				pending.Approved = true
			case Denied:
				run.Status = Failed
			default:
				return ErrInvalid
			}
			addEvent(run, "tool.approval", string(decision), pending.Call.ID)
			if err := r.save(ctx, run); err != nil {
				return err
			}
			if decision == Denied {
				return ErrDenied
			}
			return nil
		}
		run.Status = ToolRunning
		addEvent(run, "tool.started", "", pending.Call.ID)
		if err := r.save(ctx, run); err != nil {
			return err
		}
		// save detaches slices, so reacquire the pending entry after every save.
		pending = &run.Calls[run.Current]
		if err := ctx.Err(); err != nil {
			run.Status = Canceled
			addEvent(run, "run.canceled", "", pending.Call.ID)
			return errors.Join(err, r.save(ctx, run))
		}
		result, err := safeCall(func() (string, error) {
			return selected.Execute(ctx, clone(ToolRequest{RunID: run.ID, Call: pending.Call}))
		})
		if err != nil {
			run.Status = OutcomeUnknown
			addEvent(run, "tool.unknown", "", pending.Call.ID)
			return errors.Join(ErrUnknown, err, r.save(ctx, run))
		}
		pending.Done = true
		pending.Result = result
		run.Messages = append(run.Messages, Message{Role: "tool", Content: result, CallID: pending.Call.ID})
		addEvent(run, "tool.completed", result, pending.Call.ID)
		run.Current++
		run.Status = ToolsReady
		if run.Current == len(run.Calls) {
			run.Status = ModelReady
		}
		if ctx.Err() != nil {
			run.Status = Canceled
			addEvent(run, "run.canceled", "", "")
		}
		if err := r.save(ctx, run); err != nil {
			return errors.Join(ErrUnknown, err)
		}
		return nil
	})
}

func (r *runtime) Decide(ctx context.Context, id, callID string, decision Decision) (Run, error) {
	run, err := r.Read(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if run.Status != AwaitingApproval || run.Current >= len(run.Calls) || run.Current < 0 || run.Calls[run.Current].Call.ID != callID || (decision != Granted && decision != Denied) {
		return run, ErrInvalid
	}
	if decision == Granted {
		run.Calls[run.Current].Approved = true
		run.Status = ToolsReady
	} else {
		run.Status = Failed
	}
	addEvent(&run, "tool.approval", string(decision), callID)
	if err := r.save(ctx, &run); err != nil {
		return run, err
	}
	err = r.flush(ctx, &run)
	return run, err
}
func (r *runtime) Recover(ctx context.Context, id string) (Run, error) {
	run, err := r.Read(ctx, id)
	if err != nil {
		return Run{}, err
	}
	switch run.Status {
	case ModelRunning:
		run.Status = ModelReady
	case ToolRunning:
		run.Status = OutcomeUnknown
	default:
		return run, nil
	}
	addEvent(&run, "run.recovered", "", "")
	if err := r.save(ctx, &run); err != nil {
		return run, err
	}
	err = r.flush(ctx, &run)
	return run, err
}
func (r *runtime) Resolve(ctx context.Context, id, callID, result string) (Run, error) {
	run, err := r.Read(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if run.Status != OutcomeUnknown || run.Current < 0 || run.Current >= len(run.Calls) || run.Calls[run.Current].Call.ID != callID {
		return run, ErrInvalid
	}
	pending := &run.Calls[run.Current]
	pending.Done = true
	pending.Result = result
	run.Messages = append(run.Messages, Message{Role: "tool", CallID: callID, Content: result})
	addEvent(&run, "tool.resolved", result, callID)
	run.Current++
	run.Status = ToolsReady
	if run.Current == len(run.Calls) {
		run.Status = ModelReady
	}
	if err := r.save(ctx, &run); err != nil {
		return run, err
	}
	err = r.flush(ctx, &run)
	return run, err
}
