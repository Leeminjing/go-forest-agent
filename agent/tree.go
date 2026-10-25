package agent

import (
	"context"
	"errors"
	"time"

	"github.com/Leeminjing/go-forest/forest"
)

type Options struct {
	MaxModelTurns int
	CommitTimeout time.Duration
}
type runtime struct {
	model      forest.Ref[Model]
	tools      forest.Ref[Tool]
	checkpoint forest.Ref[Checkpoint]
	events     forest.Ref[Events]
	approval   forest.Ref[Approval]
	options    Options
}

func Definition(options Options) forest.Definition {
	return forest.Definition{TypeID: "go-forest-agent.runtime", Version: "1",
		Ports: []forest.PortDescriptor{
			forest.Port[Model]("model", forest.Required()),
			forest.Port[Tool]("tools", forest.Many()),
			forest.Port[Checkpoint]("checkpoint", forest.Required(), forest.During(forest.Construction)),
			forest.Port[Events]("events", forest.Required()),
			forest.Port[Approval]("approval", forest.Required()),
		},
		Capabilities: []forest.CapabilityDescriptor{forest.Capability[Executor]("executor", forest.Public)},
		Build: func(ctx context.Context, env *forest.BuildEnv) (forest.Lifecycle, error) {
			options := options
			if options.MaxModelTurns == 0 {
				options.MaxModelTurns = 32
			}
			if options.CommitTimeout == 0 {
				options.CommitTimeout = 5 * time.Second
			}
			if options.MaxModelTurns < 1 || options.MaxModelTurns > 128 || options.CommitTimeout < time.Millisecond {
				return nil, ErrInvalid
			}
			r := &runtime{options: options}
			var errs []error
			var err error
			r.model, err = forest.Reference[Model](env, "model")
			errs = append(errs, err)
			r.tools, err = forest.Reference[Tool](env, "tools")
			errs = append(errs, err)
			r.checkpoint, err = forest.Reference[Checkpoint](env, "checkpoint")
			errs = append(errs, err)
			r.events, err = forest.Reference[Events](env, "events")
			errs = append(errs, err)
			r.approval, err = forest.Reference[Approval](env, "approval")
			errs = append(errs, err)
			if err := errors.Join(errs...); err != nil {
				return nil, err
			}
			return forest.Hooks{}, forest.Provide(env, "executor", r)
		}}
}

// Continue opens a fresh Scope per Advance; no Scope survives a pause or restart.
func Continue(ctx context.Context, f *forest.Forest, tree forest.TreeID, id string) (Run, error) {
	if _, err := f.Generation(ctx); err == nil {
		return Run{}, ErrNestedScope
	}
	for {
		var run Run
		err := f.Invoke(ctx, func(stepCtx context.Context) error {
			return forest.Use[Executor](stepCtx, f, tree, "executor", func(e Executor) error {
				var err error
				run, err = e.Advance(stepCtx, id)
				return err
			})
		})
		if err != nil || run.Status == Completed || run.Status == Canceled || run.Status == Failed || run.Status == AwaitingApproval || run.Status == OutcomeUnknown {
			return run, err
		}
	}
}
