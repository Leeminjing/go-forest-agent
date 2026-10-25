// Package openairesponses implements the Agent's Model contract using the
// OpenAI Responses protocol. Provider continuation belongs to this adapter,
// not to the generic executor or the application's authored history.
package openairesponses

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/Leeminjing/go-forest-agent/agent"
)

var (
	ErrProtocol     = errors.New("unsupported or incomplete Responses protocol")
	ErrContinuation = errors.New("Responses continuation missing or inconsistent")
	ErrConflict     = errors.New("Responses journal conflict")
	ErrRefused      = errors.New("model refused request")
)

const MaxRecordBytes = 8 << 20

// Journal is defined by its consumer. Append must publish expected+1 without
// overwrite; Load must return detached records and never skip corrupt entries.
type Journal interface {
	Load(context.Context, string) ([]Record, error)
	Append(context.Context, string, uint64, Record) error
}

type Record struct {
	Version     string             `json:"version"`
	RunID       string             `json:"run_id"`
	Sequence    uint64             `json:"sequence"`
	Parent      string             `json:"parent"`
	Profile     string             `json:"profile"`
	RequestHash string             `json:"request_hash"`
	Request     agent.ModelRequest `json:"request"`
	Result      agent.ModelResult  `json:"result"`
	Input       []json.RawMessage  `json:"input"`
	Output      []json.RawMessage  `json:"output"`
	ResponseID  string             `json:"response_id"`
	RequestID   string             `json:"request_id"`
	Digest      string             `json:"digest"`
}

func Fingerprint(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil || len(data) > MaxRecordBytes {
		return "", ErrProtocol
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
func (r Record) Check() error {
	if r.Version != "openai-responses-journal-v1" || r.RunID == "" || r.Request.RunID != r.RunID || r.Sequence == 0 || r.Profile == "" || r.ResponseID == "" || r.Input == nil || r.Output == nil {
		return ErrContinuation
	}
	requestHash, err := Fingerprint(r.Request)
	if err != nil || requestHash != r.RequestHash {
		return ErrContinuation
	}
	digest := r.Digest
	r.Digest = ""
	expected, err := Fingerprint(r)
	if err != nil || digest != expected {
		return ErrContinuation
	}
	result, err := standardResult(r.Output, r.Request.Tools)
	if err != nil || !reflect.DeepEqual(result, r.Result) {
		return ErrContinuation
	}
	return nil
}

func CheckChain(id string, records []Record) error {
	parent := ""
	for i, record := range records {
		if record.RunID != id || record.Sequence != uint64(i+1) || record.Parent != parent || record.Check() != nil {
			return ErrContinuation
		}
		if i > 0 {
			previous := records[i-1]
			if record.Profile != previous.Profile {
				return ErrContinuation
			}
			input, err := nextInput(previous, record.Request)
			if err != nil {
				return err
			}
			a, _ := Fingerprint(input)
			b, _ := Fingerprint(record.Input)
			if a != b {
				return ErrContinuation
			}
		} else {
			input, err := initialInput(record.Request.Messages)
			if err != nil {
				return err
			}
			a, _ := Fingerprint(input)
			b, _ := Fingerprint(record.Input)
			if a != b {
				return ErrContinuation
			}
		}
		parent = record.Digest
	}
	return nil
}

func nextInput(previous Record, request agent.ModelRequest) ([]json.RawMessage, error) {
	before := append([]agent.Message{}, previous.Request.Messages...)
	before = append(before, agent.Message{Role: "assistant", Content: previous.Result.Content, Calls: previous.Result.Calls})
	if len(request.Messages) <= len(before) || len(previous.Result.Calls) == 0 {
		return nil, ErrContinuation
	}
	a, _ := Fingerprint(before)
	b, _ := Fingerprint(request.Messages[:len(before)])
	if a != b {
		return nil, ErrContinuation
	}
	toolsBefore, _ := Fingerprint(previous.Request.Tools)
	toolsNow, _ := Fingerprint(request.Tools)
	if toolsBefore != toolsNow {
		return nil, ErrContinuation
	}
	tail := request.Messages[len(before):]
	if len(tail) != len(previous.Result.Calls) {
		return nil, ErrContinuation
	}
	input := append([]json.RawMessage{}, previous.Input...)
	input = append(input, previous.Output...)
	for i, message := range tail {
		if message.Role != "tool" || message.CallID != previous.Result.Calls[i].ID || len(message.Calls) != 0 {
			return nil, ErrContinuation
		}
		item, err := json.Marshal(map[string]any{"type": "function_call_output", "call_id": message.CallID, "output": message.Content})
		if err != nil {
			return nil, err
		}
		input = append(input, item)
	}
	return input, nil
}

func initialInput(messages []agent.Message) ([]json.RawMessage, error) {
	if len(messages) == 0 || messages[len(messages)-1].Role != "user" {
		return nil, ErrContinuation
	}
	input := []json.RawMessage{}
	pending := map[string]bool{}
	seen := map[string]bool{}
	add := func(value any) { data, _ := json.Marshal(value); input = append(input, data) }
	for _, message := range messages {
		switch message.Role {
		case "user", "system", "assistant":
			if message.Role != "assistant" && len(message.Calls) > 0 {
				return nil, ErrProtocol
			}
			if message.Content != "" {
				add(map[string]any{"role": message.Role, "content": message.Content})
			}
			for _, call := range message.Calls {
				if seen[call.ID] || call.ID == "" || !validObject(call.Arguments) {
					return nil, ErrProtocol
				}
				seen[call.ID] = true
				pending[call.ID] = true
				add(map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": string(call.Arguments)})
			}
		case "tool":
			if !pending[message.CallID] || len(message.Calls) > 0 {
				return nil, ErrProtocol
			}
			delete(pending, message.CallID)
			add(map[string]any{"type": "function_call_output", "call_id": message.CallID, "output": message.Content})
		default:
			return nil, ErrProtocol
		}
	}
	if len(pending) != 0 {
		return nil, ErrProtocol
	}
	return input, nil
}
