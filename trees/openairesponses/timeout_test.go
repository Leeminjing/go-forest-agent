package openairesponses_test

import (
	"context"
	"errors"
	"github.com/Leeminjing/go-forest-agent/agent"
	"github.com/Leeminjing/go-forest-agent/trees/openairesponses"
	"github.com/Leeminjing/go-forest-agent/trees/responsejournal"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConfiguredTimeoutIsPreservedAfterSSEHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	journal, _ := responsejournal.New(t.TempDir())
	model, err := openairesponses.New(openairesponses.Config{Endpoint: server.URL + "/responses", Model: "fixture", APIKey: "fixture", Timeout: 25 * time.Millisecond}, journal)
	if err != nil {
		t.Fatal(err)
	}
	_, err = model.Generate(context.Background(), agent.ModelRequest{RunID: "timeout", Messages: []agent.Message{{Role: "user", Content: "task"}}}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout category lost: %v", err)
	}
}
