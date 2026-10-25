package tests

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/Leeminjing/go-forest-agent/agent"
)

func TestMultiplePlannedToolsResumeInOrderWithoutReplayingSavedPrefix(t *testing.T) {
	var count atomic.Int32
	var order []string
	model := modelFunc(func(ctx context.Context, request agent.ModelRequest, emit func(string) error) (agent.ModelResult, error) {
		if request.Messages[len(request.Messages)-1].Role == "tool" {
			return agent.ModelResult{Content: "both results saved"}, nil
		}
		return agent.ModelResult{Calls: []agent.ToolCall{
			{ID: "first", Name: "add", Arguments: json.RawMessage(`{"a":1,"b":2}`)},
			{ID: "second", Name: "add", Arguments: json.RawMessage(`{"a":3,"b":4}`)},
		}}, nil
	})
	tool := &testTool{version: "1", calls: &count, execute: func(ctx context.Context, request agent.ToolRequest) (string, error) {
		order = append(order, request.Call.ID)
		return "result for " + request.Call.ID, nil
	}}
	directory := t.TempDir()
	first := setup(t, directory, model, tool, nil, agent.Options{})
	first.create(t, "multi-tool")
	if run, err := first.advance("multi-tool"); err != nil || run.Status != agent.ToolsReady || len(run.Calls) != 2 {
		t.Fatal(run, err)
	}
	if run, err := first.advance("multi-tool"); err != nil || run.Status != agent.ToolsReady || run.Current != 1 || !run.Calls[0].Done {
		t.Fatal(run, err)
	}
	second := setup(t, directory, model, tool, nil, agent.Options{})
	run, err := agent.Continue(context.Background(), second.f, "agent", "multi-tool")
	if err != nil || run.Status != agent.Completed || count.Load() != 2 || !reflect.DeepEqual(order, []string{"first", "second"}) {
		t.Fatal(run, err, count.Load(), order)
	}
}
