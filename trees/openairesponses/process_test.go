package openairesponses_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Leeminjing/go-forest-agent/agent"
	"github.com/Leeminjing/go-forest-agent/trees/openairesponses"
	"github.com/Leeminjing/go-forest-agent/trees/responsejournal"
)

func TestResponseProcessFixture(t *testing.T) {
	mode := os.Getenv("RESPONSE_PROCESS_MODE")
	if mode == "" {
		t.Skip("subprocess fixture")
	}
	journal, err := responsejournal.New(os.Getenv("RESPONSE_PROCESS_DIRECTORY"))
	if err != nil {
		t.Fatal(err)
	}
	model, err := openairesponses.New(openairesponses.Config{Endpoint: os.Getenv("RESPONSE_PROCESS_ENDPOINT"), APIKey: "fixture", Model: "fixture"}, journal)
	if err != nil {
		t.Fatal(err)
	}
	result, err := model.Generate(context.Background(), agent.ModelRequest{RunID: "durable", Messages: []agent.Message{{Role: "user", Content: "task"}}}, nil)
	if err != nil || result.Content != "persisted" {
		t.Fatal(result, err)
	}
	if mode == "execute" {
		fmt.Println("journal-saved-before-agent")
		<-time.After(30 * time.Second)
	} else {
		fmt.Println("response-reused")
	}
}
func TestRealProcessReusesModelResultBeforeAgentCheckpointCommit(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"response","status":"completed","output":[{"type":"message","id":"message","status":"completed","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"persisted"}]}]}}`+"\n\n")
	}))
	defer server.Close()
	directory := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "RESPONSE_PROCESS_DIRECTORY="+directory, "RESPONSE_PROCESS_ENDPOINT="+server.URL+"/responses")
	cmd := exec.Command(binary, "-test.run=^TestResponseProcessFixture$", "-test.timeout=35s")
	cmd.Env = append(env, "RESPONSE_PROCESS_MODE=execute")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
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
		if line != "journal-saved-before-agent" {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal(line, stderr.String())
		}
	case <-time.After(8 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("missing durable boundary", stderr.String())
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if strings.Contains(stderr.String(), "WARNING: DATA RACE") {
		t.Fatal(stderr.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	recovery := exec.CommandContext(ctx, binary, "-test.run=^TestResponseProcessFixture$", "-test.timeout=9s")
	recovery.Env = append(env, "RESPONSE_PROCESS_MODE=recover")
	output, err := recovery.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "response-reused") || requests.Load() != 1 {
		t.Fatal(err, string(output), requests.Load())
	}
}
