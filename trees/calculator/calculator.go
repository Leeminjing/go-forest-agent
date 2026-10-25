// Package calculator provides a deterministic, side-effect-free example tool.
package calculator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"

	"github.com/Leeminjing/go-forest-agent/agent"
	"github.com/Leeminjing/go-forest/forest"
)

type Service interface {
	Describe(context.Context) (agent.ToolSpec, error)
	Execute(context.Context, agent.ToolRequest) (string, error)
}
type tool struct{ approval bool }

func (t tool) Describe(context.Context) (agent.ToolSpec, error) {
	return agent.ToolSpec{Name: "add", Version: "1", Description: "Add two finite numbers", Parameters: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"],"additionalProperties":false}`), NeedsApproval: t.approval}, nil
}
func (t tool) Execute(ctx context.Context, request agent.ToolRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var input struct {
		A *float64 `json:"a"`
		B *float64 `json:"b"`
	}
	d := json.NewDecoder(bytes.NewReader(request.Call.Arguments))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF || input.A == nil || input.B == nil {
		return "", agent.ErrInvalid
	}
	return strconv.FormatFloat(*input.A+*input.B, 'g', -1, 64), nil
}
func Definition(requireApproval bool) forest.Definition {
	return forest.Definition{TypeID: "go-forest-agent.tool.add", Version: "1", HotSwappable: true, Capabilities: []forest.CapabilityDescriptor{forest.Capability[Service]("tool", forest.Public)}, Build: func(ctx context.Context, env *forest.BuildEnv) (forest.Lifecycle, error) {
		return forest.Hooks{}, forest.Provide(env, "tool", tool{approval: requireApproval})
	}}
}
