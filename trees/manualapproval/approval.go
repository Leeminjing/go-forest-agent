// Package manualapproval pauses tools requiring explicit host authorization.
package manualapproval

import (
	"context"
	"github.com/Leeminjing/go-forest-agent/agent"
	"github.com/Leeminjing/go-forest/forest"
)

type Policy struct{}

func (Policy) Check(ctx context.Context, request agent.ApprovalRequest) (agent.Decision, error) {
	if err := ctx.Err(); err != nil {
		return agent.Pending, err
	}
	if request.Tool.NeedsApproval {
		return agent.Pending, nil
	}
	return agent.Granted, nil
}
func Definition() forest.Definition {
	return forest.Definition{TypeID: "go-forest-agent.approval.manual", Version: "1", Capabilities: []forest.CapabilityDescriptor{forest.Capability[agent.Approval]("approval", forest.Public)}, Build: func(ctx context.Context, env *forest.BuildEnv) (forest.Lifecycle, error) {
		return forest.Hooks{}, forest.Provide(env, "approval", Policy{})
	}}
}
