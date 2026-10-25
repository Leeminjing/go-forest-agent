// Package scriptedmodel provides an offline deterministic model for examples
// and tests. It is not a hosted language model or a model-provider SDK.
package scriptedmodel

import (
	"context"
	"encoding/json"

	"github.com/Leeminjing/go-forest-agent/agent"
	"github.com/Leeminjing/go-forest/forest"
)

type Generator interface {
	Generate(context.Context, agent.ModelRequest, func(string) error) (agent.ModelResult, error)
}
type model struct{ version string }

func (m model) Generate(ctx context.Context, request agent.ModelRequest, emit func(string) error) (agent.ModelResult, error) {
	if err := ctx.Err(); err != nil {
		return agent.ModelResult{}, err
	}
	if len(request.Messages) == 0 {
		return agent.ModelResult{}, agent.ErrInvalid
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role == "tool" {
		result := "model " + m.version + ": " + last.Content
		for _, part := range []string{"model ", m.version, ": ", last.Content} {
			if err := emit(part); err != nil {
				return agent.ModelResult{}, err
			}
		}
		return agent.ModelResult{Content: result}, nil
	}
	return agent.ModelResult{Calls: []agent.ToolCall{{ID: "addition-1", Name: "add", Arguments: json.RawMessage(`{"a":2,"b":3}`)}}}, nil
}
func Definition(version string) forest.Definition {
	return forest.Definition{TypeID: "go-forest-agent.model.scripted", Version: version, HotSwappable: true, Capabilities: []forest.CapabilityDescriptor{forest.Capability[Generator]("model", forest.Public)}, Build: func(ctx context.Context, env *forest.BuildEnv) (forest.Lifecycle, error) {
		if version == "" {
			return nil, agent.ErrInvalid
		}
		return forest.Hooks{}, forest.Provide(env, "model", model{version: version})
	}}
}
