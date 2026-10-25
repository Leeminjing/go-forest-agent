package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/Leeminjing/go-forest-agent/agent"
	"github.com/Leeminjing/go-forest-agent/trees/calculator"
	"github.com/Leeminjing/go-forest-agent/trees/events"
	"github.com/Leeminjing/go-forest-agent/trees/filecheckpoint"
	"github.com/Leeminjing/go-forest-agent/trees/manualapproval"
	"github.com/Leeminjing/go-forest-agent/trees/scriptedmodel"
	"github.com/Leeminjing/go-forest/forest"
)

func main() {
	directory := flag.String("directory", "checkpoints", "checkpoint directory")
	id := flag.String("id", "demo", "run ID")
	approve := flag.Bool("approve", false, "explicitly approve the currently paused call")
	version := flag.String("model-version", "1", "deterministic model version")
	flag.Parse()
	if err := run(*directory, *id, *approve, *version); err != nil {
		log.Fatal(err)
	}
}
func run(directory, id string, approve bool, version string) (result error) {
	f := forest.New(forest.Options{})
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := f.Close(ctx); result == nil {
			result = err
		}
	}()
	memory := events.New(4096)
	sub := memory.Subscribe(1024)
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		for event := range sub.C {
			if event.Temporary {
				fmt.Printf("delta: %s\n", event.Text)
			}
		}
	}()
	defer func() { sub.Close(); <-streamDone }()
	definitions := []struct {
		id         forest.TreeID
		definition forest.Definition
	}{
		{"agent", agent.Definition(agent.Options{})}, {"model", scriptedmodel.Definition(version)}, {"tool", calculator.Definition(true)}, {"checkpoint", filecheckpoint.Definition(directory)}, {"events", events.Definition(memory)}, {"approval", manualapproval.Definition()},
	}
	var operations []forest.Operation
	for _, item := range definitions {
		if err := f.Register(item.definition); err != nil {
			return err
		}
		operations = append(operations, forest.AddRoot(item.id, item.definition.Ref()))
	}
	for _, binding := range []struct {
		port       forest.PortID
		provider   forest.TreeID
		capability forest.CapabilityID
	}{{"model", "model", "model"}, {"tools", "tool", "tool"}, {"checkpoint", "checkpoint", "checkpoint"}, {"events", "events", "events"}, {"approval", "approval", "approval"}} {
		operations = append(operations, forest.Connect(forest.Link{ID: string(binding.port), Consumer: "agent", Port: binding.port, Provider: binding.provider, Capability: binding.capability}))
	}
	if _, err := f.Apply(context.Background(), forest.ChangePlan{Operations: operations}); err != nil {
		return err
	}
	var state agent.Run
	if err := f.Invoke(context.Background(), func(ctx context.Context) error {
		return forest.Use[agent.Executor](ctx, f, "agent", "executor", func(executor agent.Executor) error {
			var err error
			state, err = executor.Read(ctx, id)
			if errors.Is(err, agent.ErrNotFound) {
				state, err = executor.Create(ctx, id, "Calculate 2 + 3")
			}
			if err != nil {
				return err
			}
			if approve && state.Status == agent.AwaitingApproval {
				state, err = executor.Decide(ctx, id, state.Revision, state.Calls[state.Current].Call.ID, agent.Granted)
			}
			return err
		})
	}); err != nil {
		return err
	}
	state, err := agent.Continue(context.Background(), f, "agent", id)
	if err != nil {
		return err
	}
	fmt.Printf("run=%s status=%s revision=%d\n", state.ID, state.Status, state.Revision)
	if state.Status == agent.AwaitingApproval {
		fmt.Println("Paused. Re-run with -approve to authorize this specific call.")
	}
	if state.Status == agent.Completed {
		fmt.Println(state.Messages[len(state.Messages)-1].Content)
	}
	if err := sub.Err(); err != nil {
		return err
	}
	return nil
}
