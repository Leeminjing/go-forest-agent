package tests

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Leeminjing/go-forest-agent/agent"
)

func TestAgentProcessFixture(t *testing.T) {
	mode := os.Getenv("FOREST_AGENT_FIXTURE")
	if mode == "" {
		t.Skip("subprocess fixture")
	}
	directory := os.Getenv("FOREST_AGENT_CHECKPOINTS")
	marker := filepath.Join(directory, "effects.log")
	var calls atomic.Int32
	tool := &testTool{version: "1", calls: &calls, execute: func(ctx context.Context, _ agent.ToolRequest) (string, error) {
		file, err := os.OpenFile(marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return "", err
		}
		if _, err := file.WriteString("effect\n"); err != nil {
			file.Close()
			return "", err
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return "", err
		}
		file.Close()
		if mode == "inflight" {
			fmt.Println("effect-visible")
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "5", nil
	}}
	model := toolModel("done")
	if mode == "saved" {
		model = modelFunc(func(ctx context.Context, r agent.ModelRequest, emit func(string) error) (agent.ModelResult, error) {
			if r.Messages[len(r.Messages)-1].Role == "tool" {
				fmt.Println("result-saved")
				<-ctx.Done()
				return agent.ModelResult{}, ctx.Err()
			}
			return toolModel("done").Generate(ctx, r, emit)
		})
	}
	x := setup(t, directory, model, tool, nil, agent.Options{})
	if mode == "recover" {
		run, err := x.manage(func(ctx context.Context, e agent.Executor) (agent.Run, error) { return e.Recover(ctx, "process") })
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == agent.OutcomeUnknown {
			if _, err := agent.Continue(context.Background(), x.f, "agent", "process"); !errors.Is(err, agent.ErrUnknown) {
				t.Fatal(err)
			}
			data, err := os.ReadFile(marker)
			if err != nil || strings.Count(string(data), "effect\n") != 1 {
				t.Fatal("unknown tool replayed", err, string(data))
			}
			if _, err := x.manage(func(ctx context.Context, e agent.Executor) (agent.Run, error) {
				return e.Resolve(ctx, "process", "call-1", "verified 5")
			}); err != nil {
				t.Fatal(err)
			}
		}
		if run, err := agent.Continue(context.Background(), x.f, "agent", "process"); err != nil || run.Status != agent.Completed {
			t.Fatal(run, err)
		}
		data, err := os.ReadFile(marker)
		if err != nil || strings.Count(string(data), "effect\n") != 1 {
			t.Fatal("saved result replayed", err, string(data))
		}
		fmt.Println("recovery-complete")
		return
	}
	x.create(t, "process")
	if mode == "planned" {
		if run, err := x.advance("process"); err != nil || run.Status != agent.ToolsReady {
			t.Fatal(run, err)
		}
		fmt.Println("tool-planned")
		<-time.After(30 * time.Second)
		return
	}
	if _, err := agent.Continue(context.Background(), x.f, "agent", "process"); err != nil {
		t.Fatal(err)
	}
}

func TestRealProcessCrashPreservesUnknownAndSavedToolResults(t *testing.T) {
	for _, mode := range []string{"planned", "inflight", "saved"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "-test.run=^TestAgentProcessFixture$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "FOREST_AGENT_FIXTURE="+mode, "FOREST_AGENT_CHECKPOINTS="+directory)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			// Read stderr after Wait/Kill, when the command's copier has completed.
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			})
			ready := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				if scanner.Scan() {
					ready <- scanner.Text()
				} else {
					ready <- ""
				}
			}()
			select {
			case line := <-ready:
				if line != "effect-visible" && line != "result-saved" && line != "tool-planned" {
					cmd.Process.Kill()
					cmd.Wait()
					t.Fatal("no crash point", line, stderr.String())
				}
			case <-time.After(6 * time.Second):
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("crash point timeout", stderr.String())
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			if strings.Contains(stderr.String(), "WARNING: DATA RACE") {
				t.Fatal(stderr.String())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			recovery := exec.CommandContext(ctx, executable, "-test.run=^TestAgentProcessFixture$", "-test.timeout=7s")
			recovery.Env = append(os.Environ(), "FOREST_AGENT_FIXTURE=recover", "FOREST_AGENT_CHECKPOINTS="+directory)
			output, err := recovery.CombinedOutput()
			if err != nil || !strings.Contains(string(output), "recovery-complete") {
				t.Fatal(err, string(output))
			}
		})
	}
}
