package openairesponses_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Leeminjing/go-forest-agent/agent"
	"github.com/Leeminjing/go-forest-agent/trees/openairesponses"
	"github.com/Leeminjing/go-forest-agent/trees/responsejournal"
)

func TestResponsesPreservesNativeToolContinuationAndReusesSavedResult(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("wrong protocol or authentication")
		}
		var request struct {
			Model    string            `json:"model"`
			Input    []json.RawMessage `json:"input"`
			Stream   bool              `json:"stream"`
			Store    bool              `json:"store"`
			Parallel bool              `json:"parallel_tool_calls"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if !request.Stream || request.Store || request.Parallel || request.Model != "test-model" {
			t.Error("unsafe request configuration")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-request-id", "request-proof")
		if requests.Add(1) == 1 {
			fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Reading\"}\n\n")
			fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[{"id":"reason-1","type":"reasoning","summary":[],"encrypted_content":"opaque-proof"},{"id":"msg-1","type":"message","status":"completed","phase":"commentary","role":"assistant","content":[{"type":"output_text","text":"Reading","annotations":[]}]},{"id":"fc-item-1","type":"function_call","status":"completed","call_id":"call-1","name":"read","arguments":"{\"path\":\"x.go\"}"}]}}`+"\n\n")
		} else {
			encoded, _ := json.Marshal(request.Input)
			for _, preserved := range []string{"opaque-proof", "commentary", "fc-item-1", "function_call_output", "call-1", "source text"} {
				if !strings.Contains(string(encoded), preserved) {
					t.Errorf("lost continuation %s: %s", preserved, encoded)
				}
			}
			fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp-2","status":"completed","output":[{"id":"msg-2","type":"message","status":"completed","phase":"final_answer","role":"assistant","content":[{"type":"output_text","text":"Done","annotations":[]}]}]}}`+"\n\n")
		}
	}))
	defer server.Close()
	directory := t.TempDir()
	journal, err := responsejournal.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	config := openairesponses.Config{Endpoint: server.URL + "/v1/responses", APIKey: "test-key", Model: "test-model"}
	model, err := openairesponses.New(config, journal)
	if err != nil {
		t.Fatal(err)
	}
	request := agent.ModelRequest{RunID: "r1", Messages: []agent.Message{{Role: "user", Content: "Read x.go"}}, Tools: []agent.ToolSpec{{Name: "read", Version: "1", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)}}}
	var streamed string
	first, err := model.Generate(context.Background(), request, func(delta string) error { streamed += delta; return nil })
	if err != nil || len(first.Calls) != 1 || first.Calls[0].ID != "call-1" || streamed != "Reading" {
		t.Fatal(first, err, streamed)
	}
	// Reopen the actual journal: no native state may depend on the old Model.
	journal, err = responsejournal.New(directory)
	if err != nil {
		t.Fatal(err)
	}
	model, err = openairesponses.New(config, journal)
	if err != nil {
		t.Fatal(err)
	}
	again, err := model.Generate(context.Background(), request, nil)
	if err != nil || len(again.Calls) != 1 || requests.Load() != 1 {
		t.Fatal(again, err, requests.Load())
	}
	request.Messages = append(request.Messages, agent.Message{Role: "assistant", Content: first.Content, Calls: first.Calls}, agent.Message{Role: "tool", CallID: "call-1", Content: "source text"})
	final, err := model.Generate(context.Background(), request, nil)
	if err != nil || final.Content != "Done" || len(final.Calls) != 0 || requests.Load() != 2 {
		t.Fatal(final, err, requests.Load())
	}
}
