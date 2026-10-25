// Package agent composes a durable, step-based agent executor from ordinary
// Forest trees. These interfaces belong to the executor that consumes them.
package agent

import (
	"context"
	"encoding/json"
	"errors"
)

var (
	ErrNotFound    = errors.New("agent run not found")
	ErrConflict    = errors.New("checkpoint revision conflict")
	ErrBusy        = errors.New("agent step already running")
	ErrInvalid     = errors.New("invalid agent request or model output")
	ErrUnknown     = errors.New("tool outcome unknown; explicit resolution required")
	ErrDenied      = errors.New("tool approval denied")
	ErrCheckpoint  = errors.New("checkpoint operation failed")
	ErrEvents      = errors.New("event delivery failed")
	ErrCorrupt     = errors.New("checkpoint corrupt")
	ErrStepLimit   = errors.New("agent model turn limit reached")
	ErrNestedScope = errors.New("agent driver requires a context outside a Forest Scope")
)

type Status string

const (
	ModelReady       Status = "model-ready"
	ModelRunning     Status = "model-running"
	ToolsReady       Status = "tools-ready"
	ToolRunning      Status = "tool-running"
	AwaitingApproval Status = "awaiting-approval"
	OutcomeUnknown   Status = "outcome-unknown"
	Completed        Status = "completed"
	Canceled         Status = "canceled"
	Failed           Status = "failed"
)

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
type Message struct {
	Role    string     `json:"role"`
	Content string     `json:"content"`
	CallID  string     `json:"call_id,omitempty"`
	Calls   []ToolCall `json:"calls,omitempty"`
}
type ToolSpec struct {
	Name          string          `json:"name"`
	Version       string          `json:"version"`
	Description   string          `json:"description"`
	Parameters    json.RawMessage `json:"parameters"`
	NeedsApproval bool            `json:"needs_approval"`
}
type ModelRequest struct {
	RunID    string
	Messages []Message
	Tools    []ToolSpec
}
type ModelResult struct {
	Content string
	Calls   []ToolCall
}
type ToolRequest struct {
	RunID string
	Call  ToolCall
}
type ApprovalRequest struct {
	RunID string
	Call  ToolCall
	Tool  ToolSpec
}
type Decision string

const (
	Pending Decision = "pending"
	Granted Decision = "granted"
	Denied  Decision = "denied"
)

type Event struct {
	RunID     string `json:"run_id"`
	Sequence  uint64 `json:"sequence"`
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Temporary bool   `json:"temporary,omitempty"`
}
type PendingCall struct {
	Call     ToolCall `json:"call"`
	Tool     ToolSpec `json:"tool"`
	Approved bool     `json:"approved"`
	Done     bool     `json:"done"`
	Result   string   `json:"result,omitempty"`
}
type Run struct {
	ID         string        `json:"id"`
	Revision   uint64        `json:"revision"`
	Status     Status        `json:"status"`
	Messages   []Message     `json:"messages"`
	Calls      []PendingCall `json:"calls,omitempty"`
	Current    int           `json:"current"`
	ModelTurns int           `json:"model_turns"`
	Events     []Event       `json:"events"`
	Delivered  uint64        `json:"delivered"`
}

// Validate checks the serialized state invariants required to safely resume.
func (r Run) Validate() error {
	if r.ID == "" || len(r.ID) > 256 || r.Revision == 0 || r.Current < 0 || r.Current > len(r.Calls) || r.Delivered > uint64(len(r.Events)) || len(r.Events) > 16384 {
		return ErrCorrupt
	}
	switch r.Status {
	case ModelReady, ModelRunning:
		if r.Current != len(r.Calls) {
			return ErrCorrupt
		}
	case Completed:
		if len(r.Calls) != 0 {
			return ErrCorrupt
		}
	case Canceled, Failed:
	case ToolsReady, ToolRunning, AwaitingApproval, OutcomeUnknown:
		if r.Current >= len(r.Calls) || r.Calls[r.Current].Done {
			return ErrCorrupt
		}
	default:
		return ErrCorrupt
	}
	for i, e := range r.Events {
		if e.RunID != r.ID || e.Sequence != uint64(i+1) || e.Temporary {
			return ErrCorrupt
		}
	}
	for i, call := range r.Calls {
		if call.Call.ID == "" || call.Call.Name == "" || call.Tool.Name != call.Call.Name || call.Tool.Version == "" || call.Done != (i < r.Current) || !json.Valid(call.Call.Arguments) || !json.Valid(call.Tool.Parameters) {
			return ErrCorrupt
		}
	}
	for _, message := range r.Messages {
		for _, call := range message.Calls {
			if !json.Valid(call.Arguments) {
				return ErrCorrupt
			}
		}
	}
	return nil
}

type Model interface {
	Generate(context.Context, ModelRequest, func(string) error) (ModelResult, error)
}
type Tool interface {
	Describe(context.Context) (ToolSpec, error)
	Execute(context.Context, ToolRequest) (string, error)
}

// Checkpoint must atomically append expected+1, or return ErrConflict. Load
// returns detached data and must never silently fall back from corrupt data.
type Checkpoint interface {
	Load(context.Context, string) (Run, error)
	CompareAndSwap(context.Context, string, uint64, Run) error
}

// Events must deduplicate durable events by (RunID, Sequence). Temporary
// deltas are provisional and may be discarded, repeated, or interrupted.
type Events interface {
	Publish(context.Context, Event) error
}
type Approval interface {
	Check(context.Context, ApprovalRequest) (Decision, error)
}

// Executor methods execute within one caller-owned Forest Scope. Recover and
// Resolve are privileged management operations: the host must authenticate the
// caller and ensure a previous executor is stopped before recovering a run.
type Executor interface {
	Create(context.Context, string, string) (Run, error)
	Read(context.Context, string) (Run, error)
	Advance(context.Context, string) (Run, error)
	Decide(context.Context, string, string, Decision) (Run, error)
	Recover(context.Context, string) (Run, error)
	Resolve(context.Context, string, string, string) (Run, error)
}
