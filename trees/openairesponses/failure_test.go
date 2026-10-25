package openairesponses_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Leeminjing/go-forest-agent/agent"
	"github.com/Leeminjing/go-forest-agent/trees/openairesponses"
	"github.com/Leeminjing/go-forest-agent/trees/responsejournal"
)

func TestIncompleteResponsesNeverBecomeAnExecutablePlan(t *testing.T) {
	for name, stream := range map[string]string{
		"eof":            `data: {"type":"response.output_text.delta","delta":"unfinished"}` + "\n\n",
		"incomplete":     `data: {"type":"response.incomplete","response":{"id":"r","status":"incomplete","output":[]}}` + "\n\n",
		"commentary":     `data: {"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"message","id":"m","status":"completed","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Working"}]}]}}` + "\n\n",
		"refusal":        `data: {"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"message","id":"m","status":"completed","role":"assistant","content":[{"type":"refusal","refusal":"No"}]}]}}` + "\n\n",
		"unknown-output": `data: {"type":"response.completed","response":{"id":"r","status":"completed","output":[{"type":"web_search_call","id":"m"}]}}` + "\n\n",
		"duplicate-key":  `data: {"type":"response.completed","type":"response.in_progress"}` + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, stream)
			}))
			defer server.Close()
			journal, err := responsejournal.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			model, err := openairesponses.New(openairesponses.Config{Endpoint: server.URL + "/responses", APIKey: "secret", Model: "m"}, journal)
			if err != nil {
				t.Fatal(err)
			}
			result, err := model.Generate(context.Background(), agent.ModelRequest{RunID: "r", Messages: []agent.Message{{Role: "user", Content: "task"}}}, nil)
			if err == nil || len(result.Calls) != 0 {
				t.Fatal("invalid response accepted", result, err)
			}
			records, err := journal.Load(context.Background(), "r")
			if err != nil || len(records) != 0 {
				t.Fatal("failed response persisted", records, err)
			}
		})
	}
}

func TestResponsesCancellationStopsBlockedStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	journal, _ := responsejournal.New(t.TempDir())
	model, err := openairesponses.New(openairesponses.Config{Endpoint: server.URL + "/responses", APIKey: "secret", Model: "m"}, journal)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = model.Generate(ctx, agent.ModelRequest{RunID: "r", Messages: []agent.Message{{Role: "user", Content: "task"}}}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
